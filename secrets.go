package main

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	"github.com/getsops/sops/v3/decrypt"
)

// validSecretName matches standard environment variable names: starts with
// a letter or underscore, followed by letters, digits, or underscores.
var validSecretName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const (
	secretTypeEnv  = "env"
	secretTypeFile = "file"
	sectionSecrets = "Secrets"
	sectionSOPS    = "sops"
)

// SecretEntry represents one secret to inject into a container.
type SecretEntry struct {
	Name   string // key name from [Secrets] section (e.g., "DATABASE_URL")
	Type   string // "env" or "file"
	Target string // env var name (for env) or file path (for file)
	Value  string // plaintext secret value (raw for env, raw bytes for file)
}

// decryptSOPSData decrypts SOPS-encrypted INI content from bytes.
// The SOPS_AGE_KEY_FILE env var must be set before calling this.
func decryptSOPSData(data []byte) (string, error) {
	cleartext, err := decrypt.Data(data, "ini")
	if err != nil {
		return "", fmt.Errorf("decrypting SOPS data: %w", err)
	}
	return string(cleartext), nil
}

// parseSecrets extracts and parses the [Secrets] section from a parsed INIFile.
// Returns nil if no [Secrets] section exists. Does not modify the INIFile.
//
// Value format:
//   - env:<value>          → type=env, target=key name, value=<value>
//   - file:<target>:<value> → type=file, target=<target>, value=base64-decoded <value>
func parseSecrets(ini *INIFile) ([]SecretEntry, error) {
	sec := ini.GetSection(sectionSecrets)
	if sec == nil {
		return nil, nil
	}

	var secrets []SecretEntry
	for _, e := range sec.Entries {
		if e.Key == "" {
			continue
		}
		entry, err := parseSecretValue(e.Key, e.Value)
		if err != nil {
			return nil, fmt.Errorf("secret %q: %w", e.Key, err)
		}
		secrets = append(secrets, entry)
	}
	return secrets, nil
}

// parseSecretValue parses a single secret value string.
func parseSecretValue(name, value string) (SecretEntry, error) {
	if !validSecretName.MatchString(name) {
		return SecretEntry{}, fmt.Errorf("invalid secret name %q: must match [A-Za-z_][A-Za-z0-9_]*", name)
	}
	if value == "" {
		return SecretEntry{}, fmt.Errorf("empty value")
	}

	colonIdx := strings.Index(value, ":")
	if colonIdx < 0 {
		return SecretEntry{}, fmt.Errorf("missing type prefix (expected env:... or file:...)")
	}

	typ := value[:colonIdx]
	rest := value[colonIdx+1:]

	switch typ {
	case secretTypeEnv:
		if rest == "" {
			return SecretEntry{}, fmt.Errorf("empty env value")
		}
		return SecretEntry{
			Name:   name,
			Type:   secretTypeEnv,
			Target: name,
			Value:  rest,
		}, nil

	case secretTypeFile:
		colonIdx2 := strings.Index(rest, ":")
		if colonIdx2 < 0 {
			return SecretEntry{}, fmt.Errorf("file type requires format file:<target>:<base64-value>")
		}
		target := rest[:colonIdx2]
		b64Value := rest[colonIdx2+1:]
		if target == "" {
			return SecretEntry{}, fmt.Errorf("empty file target path")
		}
		if b64Value == "" {
			return SecretEntry{}, fmt.Errorf("empty file value")
		}
		decoded, err := base64.StdEncoding.DecodeString(b64Value)
		if err != nil {
			return SecretEntry{}, fmt.Errorf("invalid base64 value: %w", err)
		}
		return SecretEntry{
			Name:   name,
			Type:   secretTypeFile,
			Target: target,
			Value:  string(decoded),
		}, nil

	default:
		return SecretEntry{}, fmt.Errorf("unknown type %q (expected env or file)", typ)
	}
}

// stripSecretsAndSOPS removes [Secrets] and [sops] sections from an INIFile.
func stripSecretsAndSOPS(ini *INIFile) {
	filtered := ini.Sections[:0]
	for _, sec := range ini.Sections {
		if sec.Name == sectionSecrets || sec.Name == sectionSOPS {
			continue
		}
		filtered = append(filtered, sec)
	}
	ini.Sections = filtered
}

// podmanSecretName returns the namespaced podman secret name.
// e.g., ("planning-webapp", "DATABASE_URL") → "planning-webapp-database-url"
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
		pName := podmanSecretName(containerName, s.Name)
		var directive string
		switch s.Type {
		case secretTypeEnv:
			directive = fmt.Sprintf("%s,type=env,target=%s", pName, s.Target)
		case secretTypeFile:
			directive = fmt.Sprintf("%s,type=mount,target=%s", pName, s.Target)
		}
		sec.Entries = append(sec.Entries, Entry{Key: "Secret", Value: directive})
	}
}

// hasSOPSSection checks raw content for a [sops] section header.
// Operates on raw text (not parsed INI) because SOPS-encrypted files
// may not parse cleanly until after decryption strips the metadata.
func hasSOPSSection(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == "[sops]" {
			return true
		}
	}
	return false
}
