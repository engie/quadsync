package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSecrets_EnvType(t *testing.T) {
	ini, err := ParseINI(strings.NewReader(`[Secrets]
DATABASE_URL=env:postgres://user:pass@host/db
API_KEY=env:sk-live-abc123
`))
	if err != nil {
		t.Fatal(err)
	}

	secrets, err := parseSecrets(ini)
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets) != 2 {
		t.Fatalf("expected 2 secrets, got %d", len(secrets))
	}

	s := secrets[0]
	if s.Name != "DATABASE_URL" || s.Type != "env" || s.Target != "DATABASE_URL" || s.Value != "postgres://user:pass@host/db" {
		t.Errorf("unexpected secret: %+v", s)
	}

	s = secrets[1]
	if s.Name != "API_KEY" || s.Type != "env" || s.Target != "API_KEY" || s.Value != "sk-live-abc123" {
		t.Errorf("unexpected secret: %+v", s)
	}
}

func TestParseSecrets_FileType(t *testing.T) {
	// "hello world" in base64
	ini, err := ParseINI(strings.NewReader(`[Secrets]
TLS_CERT=file:/run/secrets/tls.cert:aGVsbG8gd29ybGQ=
`))
	if err != nil {
		t.Fatal(err)
	}

	secrets, err := parseSecrets(ini)
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets) != 1 {
		t.Fatalf("expected 1 secret, got %d", len(secrets))
	}

	s := secrets[0]
	if s.Name != "TLS_CERT" {
		t.Errorf("expected name TLS_CERT, got %s", s.Name)
	}
	if s.Type != "file" {
		t.Errorf("expected type file, got %s", s.Type)
	}
	if s.Target != "/run/secrets/tls.cert" {
		t.Errorf("expected target /run/secrets/tls.cert, got %s", s.Target)
	}
	if s.Value != "hello world" {
		t.Errorf("expected value 'hello world', got %q", s.Value)
	}
}

func TestParseSecrets_NoSection(t *testing.T) {
	ini, err := ParseINI(strings.NewReader(`[Container]
Image=nginx:latest
`))
	if err != nil {
		t.Fatal(err)
	}

	secrets, err := parseSecrets(ini)
	if err != nil {
		t.Fatal(err)
	}
	if secrets != nil {
		t.Errorf("expected nil secrets, got %v", secrets)
	}
}

func TestParseSecrets_EmptySection(t *testing.T) {
	ini, err := ParseINI(strings.NewReader(`[Secrets]
`))
	if err != nil {
		t.Fatal(err)
	}

	secrets, err := parseSecrets(ini)
	if err != nil {
		t.Fatal(err)
	}
	if secrets != nil {
		t.Errorf("expected nil secrets, got %v", secrets)
	}
}

func TestParseSecrets_SkipsComments(t *testing.T) {
	ini, err := ParseINI(strings.NewReader(`[Secrets]
# this is a comment
API_KEY=env:sk-live-abc123
`))
	if err != nil {
		t.Fatal(err)
	}

	secrets, err := parseSecrets(ini)
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets) != 1 {
		t.Fatalf("expected 1 secret, got %d", len(secrets))
	}
}

func TestParseSecrets_Errors(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty value", "[Secrets]\nFOO=\n"},
		{"no type prefix", "[Secrets]\nFOO=justvalue\n"},
		{"empty env value", "[Secrets]\nFOO=env:\n"},
		{"unknown type", "[Secrets]\nFOO=unknown:value\n"},
		{"file missing target", "[Secrets]\nFOO=file:\n"},
		{"file empty target", "[Secrets]\nFOO=file::aGVsbG8=\n"},
		{"file empty value", "[Secrets]\nFOO=file:/path:\n"},
		{"file invalid base64", "[Secrets]\nFOO=file:/path:not-valid-base64!!!\n"},
		{"shell injection in name", "[Secrets]\nfoo$(rm -rf /)=env:value\n"},
		{"space in name", "[Secrets]\nfoo bar=env:value\n"},
		{"starts with digit", "[Secrets]\n123start=env:value\n"},
		{"hyphen in name", "[Secrets]\nfoo-bar=env:value\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ini, err := ParseINI(strings.NewReader(tt.input))
			if err != nil {
				t.Fatal(err)
			}
			_, err = parseSecrets(ini)
			if err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestParseSecrets_ValidNames(t *testing.T) {
	tests := []struct {
		name string
	}{
		{"A"},
		{"_PRIVATE"},
		{"DATABASE_URL"},
		{"api_key_v2"},
		{"X123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ini, err := ParseINI(strings.NewReader("[Secrets]\n" + tt.name + "=env:value\n"))
			if err != nil {
				t.Fatal(err)
			}
			secrets, err := parseSecrets(ini)
			if err != nil {
				t.Errorf("valid name %q rejected: %v", tt.name, err)
			}
			if len(secrets) != 1 {
				t.Errorf("expected 1 secret, got %d", len(secrets))
			}
		})
	}
}

func TestPodmanSecretName(t *testing.T) {
	tests := []struct {
		container, secret, want string
	}{
		{"planning-webapp", "DATABASE_URL", "planning-webapp-database-url"},
		{"nginx-demo", "API_KEY", "nginx-demo-api-key"},
		{"myapp", "TLS_CERT", "myapp-tls-cert"},
		{"myapp", "simple", "myapp-simple"},
	}

	for _, tt := range tests {
		got := podmanSecretName(tt.container, tt.secret)
		if got != tt.want {
			t.Errorf("podmanSecretName(%q, %q) = %q, want %q", tt.container, tt.secret, got, tt.want)
		}
	}
}

func TestStripSecretsAndSOPS(t *testing.T) {
	ini, err := ParseINI(strings.NewReader(`[Container]
Image=nginx:latest

[Secrets]
DB=env:postgres://localhost/db

[sops]
version=3.9.0
mac=ENC[AES256_GCM,data:abc]

[Service]
Restart=on-failure
`))
	if err != nil {
		t.Fatal(err)
	}

	stripSecretsAndSOPS(ini)

	result := ini.String()
	if strings.Contains(result, "[Secrets]") {
		t.Error("expected [Secrets] to be stripped")
	}
	if strings.Contains(result, "[sops]") {
		t.Error("expected [sops] to be stripped")
	}
	if !strings.Contains(result, "[Container]") {
		t.Error("expected [Container] to be preserved")
	}
	if !strings.Contains(result, "[Service]") {
		t.Error("expected [Service] to be preserved")
	}
	if !strings.Contains(result, "Image=nginx:latest") {
		t.Error("expected Image= to be preserved")
	}
}

func TestInjectSecretDirectives(t *testing.T) {
	ini, err := ParseINI(strings.NewReader(`[Container]
Image=nginx:latest
ContainerName=planning-webapp
`))
	if err != nil {
		t.Fatal(err)
	}

	secrets := []SecretEntry{
		{Name: "DATABASE_URL", Type: "env", Target: "DATABASE_URL", Value: "postgres://localhost/db"},
		{Name: "TLS_CERT", Type: "file", Target: "/run/secrets/tls.cert", Value: "cert-content"},
	}

	injectSecretDirectives(ini, "planning-webapp", secrets)

	result := ini.String()
	if !strings.Contains(result, "Secret=planning-webapp-database-url,type=env,target=DATABASE_URL") {
		t.Errorf("expected env secret directive, got:\n%s", result)
	}
	if !strings.Contains(result, "Secret=planning-webapp-tls-cert,type=mount,target=/run/secrets/tls.cert") {
		t.Errorf("expected file secret directive, got:\n%s", result)
	}
}

func TestInjectSecretDirectives_NoContainerSection(t *testing.T) {
	ini, err := ParseINI(strings.NewReader(`[Unit]
Description=test
`))
	if err != nil {
		t.Fatal(err)
	}

	secrets := []SecretEntry{
		{Name: "FOO", Type: "env", Target: "FOO", Value: "bar"},
	}

	// Should not panic
	injectSecretDirectives(ini, "test", secrets)
}

func TestHasSOPSSection(t *testing.T) {
	if !hasSOPSSection("[Container]\nImage=nginx\n\n[sops]\nversion=3.9.0\n") {
		t.Error("expected true for content with [sops]")
	}
	if hasSOPSSection("[Container]\nImage=nginx\n") {
		t.Error("expected false for content without [sops]")
	}
}

// Test key material generated with age-keygen, encrypted with sops CLI.
// Private key is test-only — not used anywhere else.
const testAgePrivateKey = "AGE-SECRET-KEY-1HLHXXTAUFDRRWUZG4T4RTLWFL0PD7438KD5KHJ38AV5WQDP7A3JQE0R86W"

// SOPS-encrypted INI produced by:
//
//	sops encrypt \
//	  --age age1wxn9jnwxrp27uct82nlejusunw790829aaphsr77h69xpu0x0dgskvc2cq \
//	  --input-type ini --output-type ini plaintext.ini
//
// Plaintext contained:
//
//	[Container]
//	Image=registry.example.com/planning-webapp:latest
//	ContainerName=planning-webapp
//
//	[Secrets]
//	DATABASE_URL=env:postgres://admin:s3cret@db.internal:5432/planning
//	API_KEY=env:sk-live-abc123xyz
//	TLS_CERT=file:/run/secrets/tls.cert:aGVsbG8gd29ybGQ=
//
//	[Service]
//	Restart=on-failure
const testEncryptedINI = `[Container]
Image         = ENC[AES256_GCM,data:GRpGGbUsVRQQihdbQ+3gG9OafXTw+NUOlc6ixwistVwX/A8ZQgm1Nqowlw==,iv:5M/z9g6f3MQQwAzBpNlcUjRjxWaRFTU9NxACeWuNRr4=,tag:sZxWL0WuzNS1nbjCO5okhA==,type:str]
ContainerName = ENC[AES256_GCM,data:3TBwUZRsIei+nW62DTUU,iv:eyCzWysW6DYRUbHrGJuL4MecaCkcmRtvtiLejzTqEIM=,tag:Rfu6Q8jmpgHV8r21HQillA==,type:str]

[Secrets]
DATABASE_URL = ENC[AES256_GCM,data:xDjdja81sEKmW6ZypIRpIOQv5QhT6ObVJkzLyDiLfvoQjexlfZcUcWByBGSOu7gLeHjyjK0=,iv:aOKQnerH/DYvvI+/c5NJPUeAhaAWmxA+3LUf8nnPHa0=,tag:VCh6DwaaQJJzaD/rdPMZzA==,type:str]
API_KEY      = ENC[AES256_GCM,data:YjuFGye+LCaOQH5aDAFYPwWNoC/c,iv:4n9X2mqgpzNnPK5Cn//QpzUyC53Mu78JFXs1Ot7lZi4=,tag:bCdDGwV3aiI+nOrf7D/8aA==,type:str]
TLS_CERT     = ENC[AES256_GCM,data:qjJgwtqRWl5vJd5fSJpJi/WCAQ6yejaCefLB0D63Gq+udVg0RSXrXXSsLw==,iv:dbxRCmZuFv80FItrggqly4ClIWwxj2Xb4LHTd4LwWhI=,tag:vxa9pYZZlxzppMHw0MZZjA==,type:str]

[Service]
Restart = ENC[AES256_GCM,data:hGzZYf3Qan0RYg==,iv:yx20y1z4eWUn40l+yMStS3PUh++YWXFxCocLsmJFdEA=,tag:+JyCdmxgnkXFnvCN/oxaCQ==,type:str]

[sops]
age__list_0__map_enc       = -----BEGIN AGE ENCRYPTED FILE-----\nYWdlLWVuY3J5cHRpb24ub3JnL3YxCi0+IFgyNTUxOSBsQkEzMVF6T0NMMnl0SGJI\nQi93L3pQQVY4YytQRVRXbDg5WUh3cldtb1VZCjVNR2FCSm5TYXI3cDNNSTc2Y0lB\nNFdPalhMNFFiSkxYZVlvS2djZCtacEkKLS0tIFJLcXVXQ3MxNmRXcldjbDFhNGhE\nQ2IrMFN2Y1BhTW81VFhveXlsUTJnSTAKsSjaKYOXXexh8bA4UICi7LPVXFuM9KFd\nhED5lRPQtEBmt7+bzh0+UEtcXAe6v1yaibY52oD8ED85XDlxYh+waw==\n-----END AGE ENCRYPTED FILE-----\n
lastmodified               = 2026-03-31T21:43:21Z
mac                        = ENC[AES256_GCM,data:4DKI7mXYMO9D22YiKgw9MUSWyafoJ08XjLLfq2pihwHSL2MMl4EqRH2uy2WY/kP5TyACfK9HsvGgT7uzipRCvXwZcf9qwlPHC40rZ+RuZa+f1TFJ7txx3dLMBjt2rgkr5eGPkt/NOTjX+ixg1NvR5jO+Phv8GBNoqezqMXNWBLM=,iv:SwELMg8vfjBwjJi1xx6veaoNNBwZjAm4uKDppiWwaZM=,tag:NIAQ75z8zy9Yh/cHws/hkA==,type:str]
unencrypted_suffix         = _unencrypted
version                    = 3.12.2
age__list_0__map_recipient = age1wxn9jnwxrp27uct82nlejusunw790829aaphsr77h69xpu0x0dgskvc2cq
`

func TestSecretsRoundTrip(t *testing.T) {
	// Write the age private key to a temp file for SOPS to use.
	keyFile := filepath.Join(t.TempDir(), "age.key")
	if err := os.WriteFile(keyFile, []byte(testAgePrivateKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)

	// Step 1: Verify hasSOPSSection detects the encrypted file.
	if !hasSOPSSection(testEncryptedINI) {
		t.Fatal("hasSOPSSection should detect [sops] in encrypted content")
	}

	// Step 2: Decrypt.
	decrypted, err := decryptSOPSData([]byte(testEncryptedINI))
	if err != nil {
		t.Fatalf("decryptSOPSData: %v", err)
	}

	// Verify plaintext values survived decryption.
	if !strings.Contains(decrypted, "registry.example.com/planning-webapp:latest") {
		t.Errorf("decrypted content missing Image value:\n%s", decrypted)
	}
	if !strings.Contains(decrypted, "postgres://admin:s3cret@db.internal:5432/planning") {
		t.Errorf("decrypted content missing DATABASE_URL value:\n%s", decrypted)
	}

	// Step 3: Parse the decrypted INI.
	ini, err := ParseINI(strings.NewReader(decrypted))
	if err != nil {
		t.Fatalf("ParseINI: %v", err)
	}

	// Step 4: Extract secrets.
	secrets, err := parseSecrets(ini)
	if err != nil {
		t.Fatalf("parseSecrets: %v", err)
	}
	if len(secrets) != 3 {
		t.Fatalf("expected 3 secrets, got %d", len(secrets))
	}

	// Verify each secret was parsed correctly.
	wantSecrets := []struct {
		name, typ, target, value string
	}{
		{"DATABASE_URL", "env", "DATABASE_URL", "postgres://admin:s3cret@db.internal:5432/planning"},
		{"API_KEY", "env", "API_KEY", "sk-live-abc123xyz"},
		{"TLS_CERT", "file", "/run/secrets/tls.cert", "hello world"},
	}
	for i, want := range wantSecrets {
		got := secrets[i]
		if got.Name != want.name || got.Type != want.typ || got.Target != want.target || got.Value != want.value {
			t.Errorf("secret[%d]: got %+v, want name=%s type=%s target=%s value=%s",
				i, got, want.name, want.typ, want.target, want.value)
		}
	}

	// Step 5: Strip [Secrets] and [sops] sections.
	stripSecretsAndSOPS(ini)
	result := ini.String()
	if strings.Contains(result, "[Secrets]") {
		t.Error("[Secrets] section not stripped")
	}
	if strings.Contains(result, "[sops]") {
		t.Error("[sops] section not stripped")
	}

	// Step 6: Inject Secret= directives.
	ini2, err := ParseINI(strings.NewReader(result))
	if err != nil {
		t.Fatalf("re-parse after strip: %v", err)
	}
	injectSecretDirectives(ini2, "planning-webapp", secrets)
	final := ini2.String()

	// Verify the final quadlet has Secret= lines and no leftover sections.
	if !strings.Contains(final, "Secret=planning-webapp-database-url,type=env,target=DATABASE_URL") {
		t.Errorf("missing DATABASE_URL secret directive in final output:\n%s", final)
	}
	if !strings.Contains(final, "Secret=planning-webapp-api-key,type=env,target=API_KEY") {
		t.Errorf("missing API_KEY secret directive in final output:\n%s", final)
	}
	if !strings.Contains(final, "Secret=planning-webapp-tls-cert,type=mount,target=/run/secrets/tls.cert") {
		t.Errorf("missing TLS_CERT secret directive in final output:\n%s", final)
	}
	if !strings.Contains(final, "Image") {
		t.Error("Image line missing from final output")
	}
	if !strings.Contains(final, "Restart") {
		t.Error("Service/Restart missing from final output")
	}
}

func TestParseSecrets_EnvValueWithColons(t *testing.T) {
	// Env values may contain colons (e.g., postgres URLs with port)
	ini, err := ParseINI(strings.NewReader(`[Secrets]
DATABASE_URL=env:postgres://user:pass@host:5432/db
`))
	if err != nil {
		t.Fatal(err)
	}

	secrets, err := parseSecrets(ini)
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets) != 1 {
		t.Fatalf("expected 1 secret, got %d", len(secrets))
	}
	if secrets[0].Value != "postgres://user:pass@host:5432/db" {
		t.Errorf("expected full URL with colons, got %q", secrets[0].Value)
	}
}
