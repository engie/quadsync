package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"filippo.io/age"
)

// validSecretName matches standard environment variable names.
var validSecretName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const (
	secretTypeEnv         = "env"
	secretTypeFile        = "file"
	secretTypeAge         = "age"
	sectionSecrets        = "Secrets"
	secretKeyEnvironment  = "Environment"
	secretKeyFile         = "File"
	encryptedSecretPrefix = "age:"
)

// SecretEntry represents one secret to inject into a container.
type SecretEntry struct {
	Name   string
	Type   string
	Target string
	Value  string
}

// AgeKeyMaterial contains the private identity and matching recipient string.
type AgeKeyMaterial struct {
	Identity  age.Identity
	Recipient string
}

// parseSecrets extracts and parses the [Secrets] section from a parsed INIFile.
func parseSecrets(ini *INIFile, ageKeyFile string) ([]SecretEntry, error) {
	sec := ini.GetSection(sectionSecrets)
	if sec == nil {
		return nil, nil
	}

	var secrets []SecretEntry
	for _, e := range sec.Entries {
		if e.Key == "" {
			continue
		}
		entry, err := parseSecretEntry(e.Key, e.Value, ageKeyFile)
		if err != nil {
			return nil, fmt.Errorf("secret %q: %w", e.Key, err)
		}
		secrets = append(secrets, entry)
	}
	if len(secrets) == 0 {
		return nil, nil
	}
	return secrets, nil
}

// parseSecretEntry parses one [Secrets] entry in repeated-key form.
func parseSecretEntry(key, value, ageKeyFile string) (SecretEntry, error) {
	switch key {
	case secretKeyEnvironment:
		name, payload, err := splitEnvironmentSecret(value)
		if err != nil {
			return SecretEntry{}, err
		}
		if strings.HasPrefix(payload, encryptedSecretPrefix) {
			payload, err = decryptSecretValue(payload, ageKeyFile)
			if err != nil {
				return SecretEntry{}, err
			}
		}
		if payload == "" {
			return SecretEntry{}, fmt.Errorf("empty environment value")
		}
		if !validSecretName.MatchString(name) {
			return SecretEntry{}, fmt.Errorf("invalid secret name %q: must match [A-Za-z_][A-Za-z0-9_]*", name)
		}
		return SecretEntry{Name: name, Type: secretTypeEnv, Target: name, Value: payload}, nil
	case secretKeyFile:
		name, target, payload, err := splitFileSecret(value)
		if err != nil {
			return SecretEntry{}, err
		}
		if !validSecretName.MatchString(name) {
			return SecretEntry{}, fmt.Errorf("invalid secret name %q: must match [A-Za-z_][A-Za-z0-9_]*", name)
		}
		if target == "" {
			return SecretEntry{}, fmt.Errorf("empty file target path")
		}
		if payload == "" {
			return SecretEntry{}, fmt.Errorf("empty file value")
		}
		if strings.HasPrefix(payload, encryptedSecretPrefix) {
			payload, err = decryptSecretValue(payload, ageKeyFile)
			if err != nil {
				return SecretEntry{}, err
			}
			return SecretEntry{Name: name, Type: secretTypeFile, Target: target, Value: payload}, nil
		}
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return SecretEntry{}, fmt.Errorf("invalid base64 value: %w", err)
		}
		return SecretEntry{Name: name, Type: secretTypeFile, Target: target, Value: string(decoded)}, nil
	default:
		return SecretEntry{}, fmt.Errorf("unknown secret directive %q (expected %s or %s)", key, secretKeyEnvironment, secretKeyFile)
	}
}

func splitEnvironmentSecret(value string) (string, string, error) {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("environment secret requires format NAME=value")
	}
	name := parts[0]
	payload := parts[1]
	if name == "" {
		return "", "", fmt.Errorf("empty environment secret name")
	}
	return name, payload, nil
}

func splitFileSecret(value string) (string, string, string, error) {
	parts := strings.SplitN(value, ":", 3)
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("file secret requires format NAME:<target>:<base64-value>")
	}
	name := parts[0]
	target := parts[1]
	payload := parts[2]
	if name == "" {
		return "", "", "", fmt.Errorf("empty file secret name")
	}
	return name, target, payload, nil
}

// encryptSecretsInPlace rewrites plaintext [Secrets] entries into inline age
// ciphertext while keeping the rest of the INI readable.
func encryptSecretsInPlace(ini *INIFile, key AgeKeyMaterial) error {
	sec := ini.GetSection(sectionSecrets)
	if sec == nil {
		return nil
	}
	for i := range sec.Entries {
		if sec.Entries[i].Key == "" {
			continue
		}
		switch sec.Entries[i].Key {
		case secretKeyEnvironment:
			name, payload, err := splitEnvironmentSecret(sec.Entries[i].Value)
			if err != nil {
				return fmt.Errorf("secret %q: %w", sec.Entries[i].Key, err)
			}
			if !validSecretName.MatchString(name) {
				return fmt.Errorf("secret %q: invalid secret name %q: must match [A-Za-z_][A-Za-z0-9_]*", sec.Entries[i].Key, name)
			}
			if strings.HasPrefix(payload, encryptedSecretPrefix) {
				continue
			}
			if payload == "" {
				return fmt.Errorf("secret %q: empty environment value", sec.Entries[i].Key)
			}
			encrypted, err := encryptSecretValue(payload, key)
			if err != nil {
				return fmt.Errorf("secret %q: %w", sec.Entries[i].Key, err)
			}
			sec.Entries[i].Value = name + "=" + encrypted
		case secretKeyFile:
			name, target, payload, err := splitFileSecret(sec.Entries[i].Value)
			if err != nil {
				return fmt.Errorf("secret %q: %w", sec.Entries[i].Key, err)
			}
			if !validSecretName.MatchString(name) {
				return fmt.Errorf("secret %q: invalid secret name %q: must match [A-Za-z_][A-Za-z0-9_]*", sec.Entries[i].Key, name)
			}
			if target == "" {
				return fmt.Errorf("secret %q: empty file target path", sec.Entries[i].Key)
			}
			if strings.HasPrefix(payload, encryptedSecretPrefix) {
				continue
			}
			decoded, err := base64.StdEncoding.DecodeString(payload)
			if err != nil {
				return fmt.Errorf("secret %q: invalid base64 value: %w", sec.Entries[i].Key, err)
			}
			encrypted, err := encryptSecretValue(string(decoded), key)
			if err != nil {
				return fmt.Errorf("secret %q: %w", sec.Entries[i].Key, err)
			}
			sec.Entries[i].Value = name + ":" + target + ":" + encrypted
		default:
			return fmt.Errorf("secret %q: unknown secret directive %q (expected %s or %s)", sec.Entries[i].Key, sec.Entries[i].Key, secretKeyEnvironment, secretKeyFile)
		}
	}
	return nil
}

// decryptSecretsInPlace rewrites inline age ciphertext in [Secrets] back to
// plaintext Environment/File payloads for editing.
func decryptSecretsInPlace(ini *INIFile, ageKeyFile string) error {
	sec := ini.GetSection(sectionSecrets)
	if sec == nil {
		return nil
	}
	for i := range sec.Entries {
		if sec.Entries[i].Key == "" {
			continue
		}
		switch sec.Entries[i].Key {
		case secretKeyEnvironment:
			name, payload, err := splitEnvironmentSecret(sec.Entries[i].Value)
			if err != nil {
				return fmt.Errorf("secret %q: %w", sec.Entries[i].Key, err)
			}
			if !strings.HasPrefix(payload, encryptedSecretPrefix) {
				continue
			}
			decrypted, err := decryptSecretValue(payload, ageKeyFile)
			if err != nil {
				return fmt.Errorf("secret %q: %w", sec.Entries[i].Key, err)
			}
			sec.Entries[i].Value = name + "=" + decrypted
		case secretKeyFile:
			name, target, payload, err := splitFileSecret(sec.Entries[i].Value)
			if err != nil {
				return fmt.Errorf("secret %q: %w", sec.Entries[i].Key, err)
			}
			if !strings.HasPrefix(payload, encryptedSecretPrefix) {
				continue
			}
			decrypted, err := decryptSecretValue(payload, ageKeyFile)
			if err != nil {
				return fmt.Errorf("secret %q: %w", sec.Entries[i].Key, err)
			}
			sec.Entries[i].Value = name + ":" + target + ":" + base64.StdEncoding.EncodeToString([]byte(decrypted))
		default:
			continue
		}
	}
	return nil
}

func loadAgeKeyMaterial(path string) (AgeKeyMaterial, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AgeKeyMaterial{}, fmt.Errorf("reading age key file %s: %w", path, err)
	}
	identities, err := age.ParseIdentities(bytes.NewReader(data))
	if err != nil {
		return AgeKeyMaterial{}, fmt.Errorf("parsing age key file %s: %w", path, err)
	}
	for _, identity := range identities {
		x25519, ok := identity.(*age.X25519Identity)
		if !ok {
			continue
		}
		return AgeKeyMaterial{Identity: x25519, Recipient: x25519.Recipient().String()}, nil
	}
	return AgeKeyMaterial{}, fmt.Errorf("no X25519 age identity found in %s", path)
}

func encryptSecretValue(value string, key AgeKeyMaterial) (string, error) {
	var buf bytes.Buffer
	recipient, err := age.ParseX25519Recipient(key.Recipient)
	if err != nil {
		return "", fmt.Errorf("parsing age recipient %q: %w", key.Recipient, err)
	}
	w, err := age.Encrypt(&buf, recipient)
	if err != nil {
		return "", fmt.Errorf("initializing age encryption: %w", err)
	}
	if _, err := io.WriteString(w, value); err != nil {
		return "", fmt.Errorf("encrypting secret: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("finalizing secret encryption: %w", err)
	}
	return encryptedSecretPrefix + key.Recipient + ":" + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func decryptSecretValue(value, ageKeyFile string) (string, error) {
	if ageKeyFile == "" {
		return "", fmt.Errorf("encrypted secret requires QUADSYNC_AGE_KEY")
	}
	parts := strings.SplitN(value, ":", 3)
	if len(parts) != 3 || parts[0] != secretTypeAge {
		return "", fmt.Errorf("invalid encrypted secret encoding")
	}
	key, err := loadAgeKeyMaterial(ageKeyFile)
	if err != nil {
		return "", err
	}
	if key.Recipient != parts[1] {
		return "", fmt.Errorf("encrypted secret is for recipient %s, but key file provides %s", parts[1], key.Recipient)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("decoding encrypted secret: %w", err)
	}
	r, err := age.Decrypt(bytes.NewReader(ciphertext), key.Identity)
	if err != nil {
		return "", fmt.Errorf("decrypting secret: %w", err)
	}
	plaintext, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("reading decrypted secret: %w", err)
	}
	return string(plaintext), nil
}

func stripSecretsSections(ini *INIFile) {
	filtered := ini.Sections[:0]
	for _, sec := range ini.Sections {
		if sec.Name == sectionSecrets {
			continue
		}
		filtered = append(filtered, sec)
	}
	ini.Sections = filtered
}

// podmanSecretName returns the namespaced podman secret name.
func podmanSecretName(container, secretName string) string {
	name := strings.ToLower(secretName)
	name = strings.ReplaceAll(name, "_", "-")
	return container + "-" + name
}

// injectSecretDirectives adds Secret= lines to an INIFile's [Container] section.
func injectSecretDirectives(ini *INIFile, containerName string, secrets []SecretEntry) {
	sec := ini.GetSection("Container")
	if sec == nil {
		return
	}
	for _, s := range secrets {
		podmanName := podmanSecretName(containerName, s.Name)
		var directive string
		switch s.Type {
		case secretTypeEnv:
			directive = fmt.Sprintf("%s,type=env,target=%s", podmanName, s.Target)
		case secretTypeFile:
			directive = fmt.Sprintf("%s,type=mount,target=%s", podmanName, s.Target)
		}
		sec.Entries = append(sec.Entries, Entry{Key: "Secret", Value: directive})
	}
}
