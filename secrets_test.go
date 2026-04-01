package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testAgePrivateKey = "AGE-SECRET-KEY-1RFCAQPV72HD6FQG7KWN0G5P6VTKG33VKEL97TDYLC0FJUZAET7NSR8AFE2"
const testAgeRecipient = "age1nfcfrgsay702pzl6a5c2yaqq09egvm6rnudl3e78ls9nsqqstvjquarjz3"

func writeTestAgeKey(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "age.txt")
	data := "# public key: " + testAgeRecipient + "\n" + testAgePrivateKey + "\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseSecretsPlaintext(t *testing.T) {
	ini, err := ParseINI(strings.NewReader(`[Secrets]
Environment=DATABASE_URL=postgres://user:pass@host/db
File=/run/secrets/tls.cert:aGVsbG8=
`))
	if err != nil {
		t.Fatal(err)
	}

	secrets, err := parseSecrets(ini, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets) != 2 {
		t.Fatalf("expected 2 secrets, got %d", len(secrets))
	}
	if secrets[0].Value != "postgres://user:pass@host/db" {
		t.Fatalf("unexpected env secret value: %q", secrets[0].Value)
	}
	if secrets[1].Name != "run_secrets_tls_cert" {
		t.Fatalf("unexpected derived file secret name: %+v", secrets[1])
	}
	if secrets[1].Type != secretTypeFile || secrets[1].Target != "/run/secrets/tls.cert" || secrets[1].Value != "hello" {
		t.Fatalf("unexpected file secret: %+v", secrets[1])
	}
}

func TestParseSecretsEncrypted(t *testing.T) {
	keyFile := writeTestAgeKey(t)
	key, err := loadAgeKeyMaterial(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := encryptSecretValue("abc123", key)
	if err != nil {
		t.Fatal(err)
	}

	ini, err := ParseINI(strings.NewReader("[Secrets]\nEnvironment=TOKEN=" + encrypted + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := parseSecrets(ini, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets) != 1 {
		t.Fatalf("expected 1 secret, got %d", len(secrets))
	}
	if secrets[0].Type != secretTypeEnv || secrets[0].Value != "abc123" {
		t.Fatalf("unexpected decrypted secret: %+v", secrets[0])
	}
}

func TestEncryptDecryptSecretsInPlace(t *testing.T) {
	keyFile := writeTestAgeKey(t)
	key, err := loadAgeKeyMaterial(keyFile)
	if err != nil {
		t.Fatal(err)
	}

	ini, err := ParseINI(strings.NewReader(`[Container]
Image=nginx:latest

[Secrets]
Environment=TOKEN=abc123
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := encryptSecretsInPlace(ini, key); err != nil {
		t.Fatal(err)
	}

	rendered := ini.String()
	if !strings.Contains(rendered, "Image=nginx:latest") {
		t.Fatalf("non-secret content changed unexpectedly:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Environment=TOKEN=age:"+testAgeRecipient+":") {
		t.Fatalf("secret was not encrypted inline:\n%s", rendered)
	}
	if strings.Contains(rendered, "Environment=TOKEN=abc123") {
		t.Fatalf("plaintext secret leaked:\n%s", rendered)
	}

	roundTrip, err := ParseINI(strings.NewReader(rendered))
	if err != nil {
		t.Fatal(err)
	}
	if err := decryptSecretsInPlace(roundTrip, keyFile); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(roundTrip.String(), "Environment=TOKEN=abc123") {
		t.Fatalf("failed to decrypt secret back to plaintext:\n%s", roundTrip.String())
	}
}

func TestTransformContainerFileInjectsSecrets(t *testing.T) {
	dir := t.TempDir()
	keyFile := writeTestAgeKey(t)
	key, err := loadAgeKeyMaterial(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := encryptSecretValue("abc123", key)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "app.container")
	content := "[Container]\nImage=nginx:latest\n\n[Secrets]\nEnvironment=TOKEN=" + encrypted + "\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	transformed, secrets, err := transformContainerFile(path, nil, nil, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(transformed, "[Secrets]") {
		t.Fatalf("[Secrets] section should be stripped:\n%s", transformed)
	}
	if !strings.Contains(transformed, "Secret=app-token,type=env,target=TOKEN") {
		t.Fatalf("missing Secret= directive:\n%s", transformed)
	}
	if len(secrets) != 1 || secrets[0].Value != "abc123" {
		t.Fatalf("unexpected parsed secrets: %+v", secrets)
	}
}

func TestEncryptDecryptFileSecretWithoutExplicitName(t *testing.T) {
	keyFile := writeTestAgeKey(t)
	key, err := loadAgeKeyMaterial(keyFile)
	if err != nil {
		t.Fatal(err)
	}

	ini, err := ParseINI(strings.NewReader(`[Secrets]
File=/run/secrets/token.json:aGVsbG8=
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := encryptSecretsInPlace(ini, key); err != nil {
		t.Fatal(err)
	}

	rendered := ini.String()
	if !strings.Contains(rendered, "File=/run/secrets/token.json:age:"+testAgeRecipient+":") {
		t.Fatalf("file secret was not encrypted inline:\n%s", rendered)
	}

	roundTrip, err := ParseINI(strings.NewReader(rendered))
	if err != nil {
		t.Fatal(err)
	}
	if err := decryptSecretsInPlace(roundTrip, keyFile); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(roundTrip.String(), "File=/run/secrets/token.json:aGVsbG8=") {
		t.Fatalf("failed to decrypt file secret back to plaintext:\n%s", roundTrip.String())
	}
}

func TestValidateSecretsSectionRejectsDerivedFileSecretCollisions(t *testing.T) {
	ini, err := ParseINI(strings.NewReader(`[Secrets]
File=/run/secrets/api-key:aGVsbG8=
File=/run/secrets/api_key:d29ybGQ=
`))
	if err != nil {
		t.Fatal(err)
	}

	err = validateSecretsSection(ini)
	if err == nil {
		t.Fatal("expected collision error, got nil")
	}
	if !strings.Contains(err.Error(), `collides with "/run/secrets/api-key"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}
