package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"filippo.io/age"
	"github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/aes"
	sopsage "github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/cmd/sops/common"
	"github.com/getsops/sops/v3/config"
	"github.com/getsops/sops/v3/keyservice"
	inistore "github.com/getsops/sops/v3/stores/ini"
	"github.com/getsops/sops/v3/version"
	"github.com/google/shlex"
)

var runEditor = runEditorCommand

const secretsScaffoldComment = "; Set secret data with env:<value> or file:<target-path>:<base64-value>"

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

	isEncrypted := hasSOPSSection(string(data))
	hadSecrets := hasSecretsSection(data)

	var plaintext []byte
	var tree *sops.Tree
	var dataKey []byte
	store := iniStore()

	if isEncrypted {
		if ageKeyFile == "" {
			return fmt.Errorf("QUADSYNC_AGE_KEY must be set for encrypted files or files with [Secrets], either in the environment or %s", configPath)
		}
		if cfgPath, ok := findSOPSConfig(filepath.Dir(absPath)); ok {
			return fmt.Errorf("%s present at %s: compatibility can't be guaranteed so aborting",
				filepath.Base(cfgPath), cfgPath)
		}
		os.Setenv("SOPS_AGE_KEY_FILE", ageKeyFile)
		tree, dataKey, plaintext, err = decryptEncryptedINI(absPath, store)
		if err != nil {
			return err
		}
	} else {
		plaintext = data
		if !hadSecrets {
			plaintext = appendSecretsScaffold(plaintext)
		}
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
	if !isEncrypted {
		edited = stripEmptySecretsSection(edited)
	}
	editedHasSecrets := hasSecretsSection(edited)
	shouldEncrypt := editedHasSecrets

	if !shouldEncrypt && bytes.Equal(edited, plaintext) {
		return nil
	}
	if shouldEncrypt && ageKeyFile == "" {
		return fmt.Errorf("QUADSYNC_AGE_KEY must be set for encrypted files or files with [Secrets], either in the environment or %s", configPath)
	}
	if shouldEncrypt {
		if cfgPath, ok := findSOPSConfig(filepath.Dir(absPath)); ok {
			return fmt.Errorf("%s present at %s: compatibility can't be guaranteed so aborting",
				filepath.Base(cfgPath), cfgPath)
		}
		os.Setenv("SOPS_AGE_KEY_FILE", ageKeyFile)
	}

	if isEncrypted && bytes.Equal(edited, plaintext) {
		return nil
	}

	var encrypted []byte
	if shouldEncrypt && isEncrypted {
		branches, err := store.LoadPlainFile(edited)
		if err != nil {
			return fmt.Errorf("edited file is not valid INI: %w", err)
		}
		tree.Branches = branches
		if err := applySecretsOnlyEncryptionPolicy(&tree.Metadata, edited); err != nil {
			return err
		}

		if err := common.EncryptTree(common.EncryptTreeOpts{
			DataKey: dataKey,
			Tree:    tree,
			Cipher:  aes.NewCipher(),
		}); err != nil {
			return fmt.Errorf("re-encrypting %s: %w", absPath, err)
		}

		encrypted, err = store.EmitEncryptedFile(*tree)
		if err != nil {
			return fmt.Errorf("encoding encrypted %s: %w", absPath, err)
		}
	} else if shouldEncrypt {
		recipient, err := recipientFromAgeKeyFile(ageKeyFile)
		if err != nil {
			return err
		}
		encrypted, err = encryptPlaintextINI(absPath, edited, recipient)
		if err != nil {
			return err
		}
	} else {
		encrypted = edited
	}
	mode := os.FileMode(0600)
	if !shouldEncrypt {
		mode = 0644
	}
	tmpOut := absPath + ".new"
	if err := os.WriteFile(tmpOut, encrypted, mode); err != nil {
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

func findSOPSConfig(startDir string) (string, bool) {
	dir := startDir
	for {
		for _, name := range []string{".sops.yaml", ".sops.yml"} {
			path := filepath.Join(dir, name)
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				return path, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func iniStore() *inistore.Store {
	cfg := &config.INIStoreConfig{}
	return inistore.NewStore(cfg)
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

func hasSecretsSection(data []byte) bool {
	ini, err := ParseINI(bytes.NewReader(data))
	if err != nil {
		return false
	}
	return ini.GetSection(sectionSecrets) != nil
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

func decryptEncryptedINI(path string, store *inistore.Store) (*sops.Tree, []byte, []byte, error) {
	tree, err := common.LoadEncryptedFileWithBugFixes(common.GenericDecryptOpts{
		Cipher:          aes.NewCipher(),
		InputStore:      store,
		InputPath:       path,
		IgnoreMAC:       false,
		KeyServices:     []keyservice.KeyServiceClient{keyservice.NewLocalClient()},
		DecryptionOrder: sops.DefaultDecryptionOrder,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("loading encrypted %s: %w", path, err)
	}

	dataKey, err := common.DecryptTree(common.DecryptTreeOpts{
		Cipher:          aes.NewCipher(),
		IgnoreMac:       false,
		Tree:            tree,
		KeyServices:     []keyservice.KeyServiceClient{keyservice.NewLocalClient()},
		DecryptionOrder: sops.DefaultDecryptionOrder,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("decrypting %s: %w", path, err)
	}

	plaintext, err := store.EmitPlainFile(tree.Branches)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encoding plaintext %s: %w", path, err)
	}
	return tree, dataKey, plaintext, nil
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

func recipientFromAgeKeyFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading age key file %s: %w", path, err)
	}
	ids, err := age.ParseIdentities(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("parsing age key file %s: %w", path, err)
	}
	for _, id := range ids {
		x25519, ok := id.(*age.X25519Identity)
		if !ok {
			continue
		}
		return x25519.Recipient().String(), nil
	}
	return "", fmt.Errorf("no X25519 age identity found in %s", path)
}

func encryptPlaintextINI(path string, plaintext []byte, recipient string) ([]byte, error) {
	store := iniStore()
	branches, err := store.LoadPlainFile(plaintext)
	if err != nil {
		return nil, fmt.Errorf("loading plaintext %s: %w", path, err)
	}
	if len(branches) == 0 {
		return nil, fmt.Errorf("%s: file cannot be empty", path)
	}
	if store.HasSopsTopLevelKey(branches[0]) {
		return nil, fmt.Errorf("%s: file already looks SOPS-encrypted", path)
	}

	key, err := sopsage.MasterKeyFromRecipient(recipient)
	if err != nil {
		return nil, fmt.Errorf("parsing age recipient %q: %w", recipient, err)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", path, err)
	}
	tree := sops.Tree{
		Branches: branches,
		Metadata: sops.Metadata{
			KeyGroups:         []sops.KeyGroup{{key}},
			ShamirThreshold:   1,
			Version:           version.Version,
		},
		FilePath: absPath,
	}
	if err := applySecretsOnlyEncryptionPolicy(&tree.Metadata, plaintext); err != nil {
		return nil, err
	}

	dataKey, errs := tree.GenerateDataKeyWithKeyServices([]keyservice.KeyServiceClient{keyservice.NewLocalClient()})
	if len(errs) > 0 {
		return nil, fmt.Errorf("generating SOPS data key: %v", errs)
	}
	if err := common.EncryptTree(common.EncryptTreeOpts{
		DataKey: dataKey,
		Tree:    &tree,
		Cipher:  aes.NewCipher(),
	}); err != nil {
		return nil, fmt.Errorf("encrypting %s: %w", path, err)
	}
	encrypted, err := store.EmitEncryptedFile(tree)
	if err != nil {
		return nil, fmt.Errorf("encoding encrypted %s: %w", path, err)
	}
	return encrypted, nil
}

func applySecretsOnlyEncryptionPolicy(md *sops.Metadata, plaintext []byte) error {
	secretKeys, err := secretKeysInSecretsSection(plaintext)
	if err != nil {
		return fmt.Errorf("parsing secrets for encryption policy: %w", err)
	}
	if len(secretKeys) == 0 {
		return fmt.Errorf("cannot encrypt: [Secrets] section has no secret entries")
	}
	md.EncryptedRegex = buildSecretKeyRegex(secretKeys)
	md.UnencryptedSuffix = ""
	md.EncryptedSuffix = ""
	md.UnencryptedRegex = ""
	md.UnencryptedCommentRegex = ""
	md.EncryptedCommentRegex = ""
	return nil
}

func secretKeysInSecretsSection(data []byte) ([]string, error) {
	ini, err := ParseINI(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	sec := ini.GetSection(sectionSecrets)
	if sec == nil {
		return nil, nil
	}
	var keys []string
	for _, e := range sec.Entries {
		if e.Key != "" {
			keys = append(keys, e.Key)
		}
	}
	return keys, nil
}

func buildSecretKeyRegex(keys []string) string {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, regexp.QuoteMeta(k))
	}
	return "^(" + strings.Join(parts, "|") + ")$"
}
