package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditContainerFileAllowsPlaintextWithoutAgeKey(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "app.container")
	original := "[Container]\nImage=nginx:latest\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	oldRunEditor := runEditor
	defer func() { runEditor = oldRunEditor }()

	runEditor = func(editor []string, gotPath string) error {
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
	final, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(final) != original {
		t.Fatalf("expected untouched plaintext file to remain unchanged, got:\n%s", final)
	}
}

func TestEditContainerFileEncryptsSecrets(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "app.container")
	if err := os.WriteFile(path, []byte("[Container]\nImage=nginx:latest\n"), 0644); err != nil {
		t.Fatal(err)
	}
	keyFile := writeTestAgeKey(t)

	oldRunEditor := runEditor
	defer func() { runEditor = oldRunEditor }()

	runEditor = func(editor []string, gotPath string) error {
		return os.WriteFile(gotPath, []byte("[Container]\nImage=nginx:latest\n\n[Secrets]\nEnvironment=TOKEN=abc123\n"), 0600)
	}

	t.Setenv("EDITOR", "true")
	if err := editContainerFile(path, keyFile); err != nil {
		t.Fatalf("expected edit to succeed, got %v", err)
	}

	encrypted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encrypted), "Environment=TOKEN=abc123") {
		t.Fatalf("plaintext secret leaked:\n%s", encrypted)
	}
	if !strings.Contains(string(encrypted), "Environment=TOKEN=age:"+testAgeRecipient+":") {
		t.Fatalf("secret was not encrypted inline:\n%s", encrypted)
	}

	ini, err := ParseINI(strings.NewReader(string(encrypted)))
	if err != nil {
		t.Fatal(err)
	}
	if err := decryptSecretsInPlace(ini, keyFile); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ini.String(), "Environment=TOKEN=abc123") {
		t.Fatalf("failed to decrypt edited file:\n%s", ini.String())
	}
}

func TestEditContainerFileDecryptsEncryptedSecretsIntoScratch(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "app.container")
	keyFile := writeTestAgeKey(t)
	key, err := loadAgeKeyMaterial(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := encryptSecretValue("abc123", key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[Container]\nImage=nginx:latest\n\n[Secrets]\nEnvironment=TOKEN="+encrypted+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	oldRunEditor := runEditor
	defer func() { runEditor = oldRunEditor }()

	runEditor = func(editor []string, gotPath string) error {
		scratch, err := os.ReadFile(gotPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(scratch), "Environment=TOKEN=abc123") {
			t.Fatalf("scratch file was not decrypted:\n%s", scratch)
		}
		return nil
	}

	t.Setenv("EDITOR", "true")
	if err := editContainerFile(path, keyFile); err != nil {
		t.Fatalf("expected edit to succeed, got %v", err)
	}
}

func TestEditContainerFileRejectsCollidingFileSecrets(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "app.container")
	content := "[Container]\nImage=nginx:latest\n\n[Secrets]\nFile=/run/secrets/api-key:aGVsbG8=\nFile=/run/secrets/api_key:d29ybGQ=\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("EDITOR", "true")
	err := editContainerFile(path, "")
	if err == nil {
		t.Fatal("expected collision error, got nil")
	}
	if !strings.Contains(err.Error(), "collides with") {
		t.Fatalf("unexpected error: %v", err)
	}
}
