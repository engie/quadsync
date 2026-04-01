package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindSOPSConfig(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "repo", "apps")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}

	cfg := filepath.Join(root, "repo", ".sops.yaml")
	if err := os.WriteFile(cfg, []byte("creation_rules: []\n"), 0644); err != nil {
		t.Fatal(err)
	}

	got, ok := findSOPSConfig(sub)
	if !ok {
		t.Fatal("expected .sops.yaml to be found")
	}
	if got != cfg {
		t.Fatalf("got %q, want %q", got, cfg)
	}
}

func TestFindSOPSConfig_NotFound(t *testing.T) {
	root := t.TempDir()
	got, ok := findSOPSConfig(root)
	if ok {
		t.Fatalf("unexpected config found: %s", got)
	}
}

func TestRecipientFromAgeKeyFile(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "age.key")
	if err := os.WriteFile(keyFile, []byte(testAgePrivateKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	got, err := recipientFromAgeKeyFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "age1wxn9jnwxrp27uct82nlejusunw790829aaphsr77h69xpu0x0dgskvc2cq"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestEncryptPlaintextINIRoundTrip(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "age.key")
	if err := os.WriteFile(keyFile, []byte(testAgePrivateKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	recipient, err := recipientFromAgeKeyFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}

	plaintext := []byte("[Container]\nImage=nginx:latest\n\n[Secrets]\n; Set secret data with env:<value> or file:<target-path>:<base64-value>\nTOKEN=env:abc123\n")
	path := filepath.Join(t.TempDir(), "app.container")
	encrypted, err := encryptPlaintextINI(path, plaintext, recipient)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSOPSSection(string(encrypted)) {
		t.Fatalf("expected encrypted output to include [sops], got:\n%s", encrypted)
	}
	if !strings.Contains(string(encrypted), "Image = nginx:latest") {
		t.Fatalf("expected non-secret values to stay plaintext, got:\n%s", encrypted)
	}
	if !strings.Contains(string(encrypted), strings.TrimPrefix(secretsScaffoldComment, "; ")) {
		t.Fatalf("expected comment to stay plaintext, got:\n%s", encrypted)
	}
	if strings.Contains(string(encrypted), "TOKEN = env:abc123") {
		t.Fatalf("expected secret value to be encrypted, got:\n%s", encrypted)
	}

	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)
	decrypted, err := decryptSOPSData(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(decrypted, "Image = nginx:latest") {
		t.Fatalf("decrypted output missing Image line:\n%s", decrypted)
	}
	if !strings.Contains(decrypted, "TOKEN = env:abc123") {
		t.Fatalf("decrypted output missing secret line:\n%s", decrypted)
	}
}

func TestEditContainerFileRejectsSOPSConfig(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".sops.yaml"), []byte("creation_rules: []\n"), 0644); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(root, "app.container")
	keyFile := filepath.Join(t.TempDir(), "age.key")
	if err := os.WriteFile(keyFile, []byte(testAgePrivateKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(testEncryptedINI), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("EDITOR", "true")
	err := editContainerFile(path, keyFile)
	if err == nil {
		t.Fatal("expected editContainerFile to reject .sops.yaml")
	}
	if !strings.Contains(err.Error(), "compatibility can't be guaranteed so aborting") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEditContainerFileAllowsPlaintextWithoutAgeKey(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "app.container")
	original := "[Container]\nImage=nginx:latest\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	oldRunEditor := runEditor
	defer func() { runEditor = oldRunEditor }()

	called := false
	runEditor = func(editor []string, gotPath string) error {
		called = true
		if gotPath == path {
			t.Fatal("editor should receive a scratch path, not the original file")
		}
		scratch, err := os.ReadFile(gotPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(scratch), "[Secrets]\n"+secretsScaffoldComment) {
			t.Fatalf("expected secrets scaffold in scratch file, got:\n%s", scratch)
		}
		return nil
	}

	t.Setenv("EDITOR", "true")
	if err := editContainerFile(path, ""); err != nil {
		t.Fatalf("expected plaintext edit to succeed without age key, got %v", err)
	}
	if !called {
		t.Fatal("expected editor to be invoked for plaintext file")
	}
	final, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(final) != original {
		t.Fatalf("expected untouched plaintext file to remain unchanged, got:\n%s", final)
	}
}

func TestEditContainerFileAllowsPlaintextUnderSOPSConfig(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".sops.yaml"), []byte("creation_rules: []\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "app.container")
	if err := os.WriteFile(path, []byte("[Container]\nImage=nginx:latest\n"), 0644); err != nil {
		t.Fatal(err)
	}

	oldRunEditor := runEditor
	defer func() { runEditor = oldRunEditor }()

	called := false
	runEditor = func(editor []string, gotPath string) error {
		called = true
		return nil
	}

	t.Setenv("EDITOR", "true")
	if err := editContainerFile(path, ""); err != nil {
		t.Fatalf("expected plaintext edit under .sops.yaml to succeed, got %v", err)
	}
	if !called {
		t.Fatal("expected editor to be invoked for plaintext file")
	}
}

func TestEditContainerFileEncryptsPlaintextSecretsFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "app.container")
	plaintext := "[Container]\nImage=nginx:latest\n\n[Secrets]\nTOKEN=env:abc123\n"
	if err := os.WriteFile(path, []byte(plaintext), 0644); err != nil {
		t.Fatal(err)
	}

	keyFile := filepath.Join(t.TempDir(), "age.key")
	if err := os.WriteFile(keyFile, []byte(testAgePrivateKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	oldRunEditor := runEditor
	defer func() { runEditor = oldRunEditor }()

	runEditor = func(editor []string, gotPath string) error {
		return nil
	}

	t.Setenv("EDITOR", "true")
	if err := editContainerFile(path, keyFile); err != nil {
		t.Fatalf("expected secrets file edit to succeed, got %v", err)
	}

	encrypted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSOPSSection(string(encrypted)) {
		t.Fatalf("expected file to be encrypted after edit, got:\n%s", encrypted)
	}
	if !strings.Contains(string(encrypted), "Image = nginx:latest") {
		t.Fatalf("expected non-secret values to stay plaintext, got:\n%s", encrypted)
	}
	if strings.Contains(string(encrypted), "TOKEN = env:abc123") {
		t.Fatalf("expected secret value to be encrypted, got:\n%s", encrypted)
	}

	t.Setenv("SOPS_AGE_KEY_FILE", keyFile)
	decrypted, err := decryptSOPSData(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(decrypted, "TOKEN = env:abc123") {
		t.Fatalf("decrypted output missing secret line:\n%s", decrypted)
	}
}

func TestEditContainerFileSecretsFileRequiresAgeKey(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "app.container")
	if err := os.WriteFile(path, []byte("[Container]\nImage=nginx:latest\n\n[Secrets]\nTOKEN=env:abc123\n"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("EDITOR", "true")
	err := editContainerFile(path, "")
	if err == nil {
		t.Fatal("expected secrets file edit to fail without age key")
	}
	if !strings.Contains(err.Error(), "files with [Secrets]") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEditContainerFileEncryptsWhenSecretsAddedDuringEdit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "app.container")
	if err := os.WriteFile(path, []byte("[Container]\nImage=nginx:latest\n"), 0644); err != nil {
		t.Fatal(err)
	}

	keyFile := filepath.Join(t.TempDir(), "age.key")
	if err := os.WriteFile(keyFile, []byte(testAgePrivateKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	oldRunEditor := runEditor
	defer func() { runEditor = oldRunEditor }()

	runEditor = func(editor []string, gotPath string) error {
		if gotPath == path {
			t.Fatal("editor should receive a scratch path, not the original file")
		}
		return os.WriteFile(gotPath, []byte("[Container]\nImage=nginx:latest\n\n[Secrets]\nTOKEN=env:abc123\n"), 0600)
	}

	t.Setenv("EDITOR", "true")
	if err := editContainerFile(path, keyFile); err != nil {
		t.Fatalf("expected edit to succeed, got %v", err)
	}

	encrypted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSOPSSection(string(encrypted)) {
		t.Fatalf("expected file to be encrypted after adding secrets, got:\n%s", encrypted)
	}
	if !strings.Contains(string(encrypted), "Image = nginx:latest") {
		t.Fatalf("expected non-secret values to stay plaintext, got:\n%s", encrypted)
	}
	if strings.Contains(string(encrypted), "TOKEN = env:abc123") {
		t.Fatalf("expected secret value to be encrypted, got:\n%s", encrypted)
	}
}

func TestEditContainerFileDecryptsWhenSecretsRemovedDuringEdit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "app.container")
	if err := os.WriteFile(path, []byte(testEncryptedINI), 0600); err != nil {
		t.Fatal(err)
	}

	keyFile := filepath.Join(t.TempDir(), "age.key")
	if err := os.WriteFile(keyFile, []byte(testAgePrivateKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	oldRunEditor := runEditor
	defer func() { runEditor = oldRunEditor }()

	// The editor removes the [Secrets] section entirely, keeping only [Container] and [Service].
	runEditor = func(editor []string, gotPath string) error {
		return os.WriteFile(gotPath, []byte("[Container]\nImage = registry.example.com/planning-webapp:latest\nContainerName = planning-webapp\n\n[Service]\nRestart = on-failure\n"), 0600)
	}

	t.Setenv("EDITOR", "true")
	if err := editContainerFile(path, keyFile); err != nil {
		t.Fatalf("expected edit to succeed, got %v", err)
	}

	result, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// The output should be plaintext (no [sops] section).
	if hasSOPSSection(string(result)) {
		t.Fatalf("expected plaintext output after removing secrets, got encrypted:\n%s", result)
	}

	// The output should still contain the container config.
	if !strings.Contains(string(result), "Image") {
		t.Fatalf("expected Image line in output, got:\n%s", result)
	}
	if !strings.Contains(string(result), "Restart") {
		t.Fatalf("expected Restart line in output, got:\n%s", result)
	}

	// Verify the [Secrets] section is gone.
	if strings.Contains(string(result), "[Secrets]") {
		t.Fatalf("expected [Secrets] section to be removed, got:\n%s", result)
	}
}

func TestStripEmptySecretsSection(t *testing.T) {
	input := []byte("[Container]\nImage=nginx:latest\n\n[Secrets]\n; Set secret data with env:<value> or file:<target-path>:<base64-value>\n")
	got := string(stripEmptySecretsSection(input))
	want := "[Container]\nImage=nginx:latest\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
