package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/shlex"
)

var runEditor = runEditorCommand

const secretsScaffoldComment = "; Use Environment=NAME=value or File=/target/path:<base64-value>"

func cmdEdit() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "Usage: quadsync edit <file>")
		os.Exit(2)
	}

	if err := editContainerFile(os.Args[2], editAgeKeyFile()); err != nil {
		log.Fatalf("edit failed: %v", err)
	}
}

func editAgeKeyFile() string {
	if v := strings.TrimSpace(os.Getenv("QUADSYNC_AGE_KEY")); v != "" {
		return v
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return ""
	}
	return cfg.AgeKeyFile
}

func editContainerFile(filePath, ageKeyFile string) error {
	if !strings.HasSuffix(filePath, ".container") {
		return fmt.Errorf("%s: expected a .container file", filePath)
	}

	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", filePath, err)
	}

	editor, err := editorCommand()
	if err != nil {
		return err
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", absPath, err)
	}

	ini, err := ParseINI(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("parsing %s: %w", absPath, err)
	}
	if err := validateSecretsSection(ini); err != nil {
		return fmt.Errorf("validating %s: %w", absPath, err)
	}
	if err := decryptSecretsInPlace(ini, ageKeyFile); err != nil {
		return fmt.Errorf("decrypting %s: %w", absPath, err)
	}

	plaintext := []byte(ini.String())
	if ini.GetSection(sectionSecrets) == nil {
		plaintext = appendSecretsScaffold(plaintext)
	}

	tmpPath, cleanup, err := writeEditScratch(absPath, plaintext)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := runEditor(editor, tmpPath); err != nil {
		return err
	}

	edited, err := os.ReadFile(tmpPath)
	if err != nil {
		return fmt.Errorf("reading edited file %s: %w", tmpPath, err)
	}
	edited = stripEmptySecretsSection(edited)
	if bytes.Equal(edited, plaintext) {
		if ini.GetSection(sectionSecrets) == nil {
			return nil
		}
	}

	editedINI, err := ParseINI(bytes.NewReader(edited))
	if err != nil {
		return fmt.Errorf("parsing edited %s: %w", absPath, err)
	}
	if err := validateSecretsSection(editedINI); err != nil {
		return fmt.Errorf("validating edited %s: %w", absPath, err)
	}

	hasSecrets := editedINI.GetSection(sectionSecrets) != nil
	mode := os.FileMode(0644)
	output := edited
	if hasSecrets {
		if ageKeyFile == "" {
			return fmt.Errorf("QUADSYNC_AGE_KEY must be set for files with [Secrets], either in the environment or %s", configPath)
		}
		key, err := loadAgeKeyMaterial(ageKeyFile)
		if err != nil {
			return err
		}
		if err := encryptSecretsInPlace(editedINI, key); err != nil {
			return fmt.Errorf("encrypting %s: %w", absPath, err)
		}
		output = []byte(editedINI.String())
		mode = 0600
	}

	tmpOut := absPath + ".new"
	if err := os.WriteFile(tmpOut, output, mode); err != nil {
		return fmt.Errorf("writing %s: %w", tmpOut, err)
	}
	if err := os.Rename(tmpOut, absPath); err != nil {
		return fmt.Errorf("replacing %s: %w", absPath, err)
	}
	return nil
}

func editorCommand() ([]string, error) {
	editor := strings.TrimSpace(os.Getenv("EDITOR"))
	if editor == "" {
		return nil, fmt.Errorf("$EDITOR is not set")
	}
	args, err := shlex.Split(editor)
	if err != nil {
		return nil, fmt.Errorf("parsing $EDITOR: %w", err)
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("$EDITOR is empty")
	}
	return args, nil
}

func runEditorCommand(editor []string, path string) error {
	args := append(append([]string{}, editor[1:]...), path)
	cmd := exec.Command(editor[0], args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s: %w", editor[0], err)
	}
	return nil
}

func appendSecretsScaffold(data []byte) []byte {
	base := strings.TrimRight(string(data), "\n")
	var b strings.Builder
	if base != "" {
		b.WriteString(base)
		b.WriteString("\n\n")
	}
	b.WriteString("[Secrets]\n")
	b.WriteString(secretsScaffoldComment)
	b.WriteString("\n")
	return []byte(b.String())
}

func stripEmptySecretsSection(data []byte) []byte {
	ini, err := ParseINI(bytes.NewReader(data))
	if err != nil {
		return data
	}
	filtered := ini.Sections[:0]
	for _, sec := range ini.Sections {
		if sec.Name != sectionSecrets {
			filtered = append(filtered, sec)
			continue
		}
		hasEntries := false
		for _, e := range sec.Entries {
			if e.Key != "" {
				hasEntries = true
				break
			}
		}
		if hasEntries {
			filtered = append(filtered, sec)
		}
	}
	ini.Sections = filtered
	return []byte(strings.TrimRight(ini.String(), "\n") + "\n")
}

func writeEditScratch(originalPath string, plaintext []byte) (string, func(), error) {
	tmpDir := scratchDir()
	f, err := os.CreateTemp(tmpDir, filepath.Base(originalPath)+".*")
	if err != nil {
		return "", nil, fmt.Errorf("creating temporary file: %w", err)
	}
	path := f.Name()
	cleanup := func() {
		os.Remove(path)
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		cleanup()
		return "", nil, fmt.Errorf("setting temporary file permissions: %w", err)
	}
	if _, err := f.Write(plaintext); err != nil {
		f.Close()
		cleanup()
		return "", nil, fmt.Errorf("writing temporary file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("closing temporary file: %w", err)
	}
	return path, cleanup, nil
}

func scratchDir() string {
	if info, err := os.Stat("/dev/shm"); err == nil && info.IsDir() {
		return "/dev/shm"
	}
	return os.TempDir()
}
