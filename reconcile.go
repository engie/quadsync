package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// CompanionTemplate is an additional quadlet file template deployed alongside
// each container. The SuffixAndExt (e.g. "-data.volume") is appended to the
// container name to form the filename, and {{.Name}} in Content is replaced
// with the container name.
type CompanionTemplate struct {
	SuffixAndExt string // e.g. "-litestream.container", "-data.volume"
	Content      string // raw content with {{.Name}} placeholders
}

// DesiredState holds all quadlet files for a single container user.
type DesiredState struct {
	Files map[string]string // filename → content (e.g. "myapp.container", "myapp-data.volume")
}

// Config holds the deployer configuration.
type Config struct {
	GitURL       string
	GitBranch    string
	TransformDir string
	StateDir     string
	UserGroup    string
	SSHKey       string // path to SSH deploy key for git
	RepoPath     string // derived: StateDir + "/repo"
}

// LoadConfig reads config from an env file.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("reading config: %w", err)
	}

	env := parseEnvFile(string(data))
	c := Config{
		GitURL:       env["QUADSYNC_GIT_URL"],
		GitBranch:    env["QUADSYNC_GIT_BRANCH"],
		TransformDir: env["QUADSYNC_TRANSFORM_DIR"],
		StateDir:     env["QUADSYNC_STATE_DIR"],
		UserGroup:    env["QUADSYNC_USER_GROUP"],
		SSHKey:       env["QUADSYNC_SSH_KEY"],
	}

	if c.GitURL == "" {
		return Config{}, fmt.Errorf("QUADSYNC_GIT_URL not set in config")
	}
	if c.GitBranch == "" {
		c.GitBranch = "main"
	}
	if c.TransformDir == "" {
		c.TransformDir = "/etc/quadsync/transforms"
	}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/quadsync"
	}
	if c.UserGroup == "" {
		c.UserGroup = "cusers"
	}
	c.RepoPath = filepath.Join(c.StateDir, "repo")
	return c, nil
}

func parseEnvFile(data string) map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.Index(line, "="); idx >= 0 {
			key := strings.TrimSpace(line[:idx])
			value := strings.TrimSpace(line[idx+1:])
			value = unquote(value)
			env[key] = value
		}
	}
	return env
}

// unquote strips matched single or double quotes from a value.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

// Sync performs the full reconciliation: git sync, transform merge, deploy, cleanup.
func Sync(config Config) error {
	// Set GIT_SSH_COMMAND from config so git operations use the deploy key.
	if config.SSHKey != "" {
		os.Setenv("GIT_SSH_COMMAND", "ssh -i "+config.SSHKey+" -o StrictHostKeyChecking=accept-new")
	}

	// Ensure state dir exists
	if err := os.MkdirAll(config.StateDir, 0755); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}

	// Acquire exclusive lock to prevent overlapping sync runs.
	lockFile, err := os.OpenFile(filepath.Join(config.StateDir, "sync.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("opening lock file: %w", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another sync is already running")
	}

	// 1. Git sync
	if _, err := os.Stat(config.RepoPath); os.IsNotExist(err) {
		log.Printf("cloning %s", config.GitURL)
		if err := gitClone(config.GitURL, config.RepoPath, config.GitBranch); err != nil {
			return fmt.Errorf("git clone: %w", err)
		}
	} else {
		changed, err := gitFetch(config.RepoPath, config.GitBranch)
		if err != nil {
			return fmt.Errorf("git fetch: %w", err)
		}
		if changed {
			log.Printf("changes detected, updating")
			if err := gitResetHard(config.RepoPath, config.GitBranch); err != nil {
				return fmt.Errorf("git reset: %w", err)
			}
		}
	}

	// 2. Validate specs
	if errs := CheckDir(config.RepoPath); len(errs) > 0 {
		for _, e := range errs {
			log.Printf("validation error: %v", e)
		}
		return fmt.Errorf("validation failed: %d error(s)", len(errs))
	}

	// 3. Load transforms
	base, transforms, companions, err := loadTransforms(config.TransformDir)
	if err != nil {
		return fmt.Errorf("loading transforms: %w", err)
	}

	// 4. Build desired state
	desired, err := buildDesired(config.RepoPath, base, transforms, companions)
	if err != nil {
		return fmt.Errorf("building desired state: %w", err)
	}

	// 5. Get current managed users
	current, err := managedUsers(config.UserGroup)
	if err != nil {
		return fmt.Errorf("listing managed users: %w", err)
	}
	currentSet := map[string]bool{}
	for _, u := range current {
		currentSet[u] = true
	}

	// 6. Deploy
	hashDir := filepath.Join(config.StateDir, "hashes")
	if err := os.MkdirAll(hashDir, 0755); err != nil {
		return fmt.Errorf("creating hash dir: %w", err)
	}

	// Deploy loop error policy: quadsync reports failure for its own
	// mechanisms (user creation, quadlet writing, daemon-reload). If a
	// container fails to start, that is the container's problem — we log
	// it as a warning but do not count it as a quadsync failure.
	var errs []error

	names := make([]string, 0, len(desired))
	for name := range desired {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		state := desired[name]
		if !currentSet[name] {
			log.Printf("creating user %s", name)
			if err := createUser(name, config.UserGroup); err != nil {
				log.Printf("error creating user %s: %v", name, err)
				errs = append(errs, fmt.Errorf("creating user %s: %w", name, err))
				continue
			}
		}

		if !specChanged(hashDir, name, state) {
			log.Printf("%s: unchanged, skipping", name)
			continue
		}

		log.Printf("%s: deploying", name)
		failed := false
		for filename, content := range state.Files {
			if err := writeQuadletFile(name, filename, content); err != nil {
				log.Printf("error writing %s for %s: %v", filename, name, err)
				errs = append(errs, fmt.Errorf("writing %s for %s: %w", filename, name, err))
				failed = true
				break
			}
		}
		if failed {
			continue
		}
		if err := cleanStaleQuadlets(name, state.Files); err != nil {
			log.Printf("error cleaning stale quadlets for %s: %v", name, err)
			errs = append(errs, fmt.Errorf("cleaning stale quadlets for %s: %w", name, err))
			continue
		}
		if err := chownQuadletDir(name); err != nil {
			log.Printf("error chowning quadlet dir for %s: %v", name, err)
			errs = append(errs, fmt.Errorf("chowning quadlet dir for %s: %w", name, err))
			continue
		}
		if err := waitForUserManager(name); err != nil {
			log.Printf("error waiting for user manager %s: %v", name, err)
			errs = append(errs, fmt.Errorf("waiting for user manager %s: %w", name, err))
			continue
		}
		if err := daemonReload(name); err != nil {
			log.Printf("error daemon-reload for %s: %v", name, err)
			errs = append(errs, fmt.Errorf("daemon-reload for %s: %w", name, err))
			continue
		}

		// Quadlet is written and systemd knows about it — quadsync's job
		// is done. Persist the hash so we don't re-deploy next cycle.
		saveHash(hashDir, name, state)

		// Best-effort service restart. If the container fails to come up
		// that is the container's concern, not ours.
		if err := restartService(name, name); err != nil {
			log.Printf("warning: restarting %s: %v (container may need attention)", name, err)
		}
	}

	// 7. Cleanup: remove containers not in desired
	for _, name := range current {
		if _, exists := desired[name]; !exists {
			log.Printf("%s: removing", name)
			if err := stopService(name, name); err != nil {
				log.Printf("warning: stopping %s: %v", name, err)
			}
			if err := removeAllQuadlets(name); err != nil {
				log.Printf("warning: removing quadlets for %s: %v", name, err)
			}
			if err := deleteUser(name); err != nil {
				log.Printf("error deleting user %s: %v", name, err)
				errs = append(errs, fmt.Errorf("deleting user %s: %w", name, err))
			}
			os.Remove(filepath.Join(hashDir, name))
		}
	}

	return errors.Join(errs...)
}

// loadTransforms reads all files from the transform directory.
// Returns a base transform (from _base.container, may be nil), a map of
// directory name → parsed INI for directory-specific transforms, and a list
// of companion templates (files matching _base-<suffix>.<ext>).
func loadTransforms(dir string) (*INIFile, map[string]*INIFile, []CompanionTemplate, error) {
	var base *INIFile
	transforms := map[string]*INIFile{}
	var companions []CompanionTemplate

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, transforms, nil, nil
		}
		return nil, nil, nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()

		if name == "_base.container" {
			// Base transform — merged into every .container
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return nil, nil, nil, fmt.Errorf("reading transform %s: %w", name, err)
			}
			f, err := ParseINI(strings.NewReader(string(data)))
			if err != nil {
				return nil, nil, nil, fmt.Errorf("parsing transform %s: %w", name, err)
			}
			base = f
		} else if strings.HasPrefix(name, "_base-") {
			// Companion template — deployed as <name><suffix>.<ext>
			suffixAndExt := strings.TrimPrefix(name, "_base")
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return nil, nil, nil, fmt.Errorf("reading companion %s: %w", name, err)
			}
			companions = append(companions, CompanionTemplate{
				SuffixAndExt: suffixAndExt,
				Content:      string(data),
			})
		} else if strings.HasSuffix(name, ".container") {
			// Directory-specific transform
			dirName := strings.TrimSuffix(name, ".container")
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return nil, nil, nil, fmt.Errorf("reading transform %s: %w", name, err)
			}
			f, err := ParseINI(strings.NewReader(string(data)))
			if err != nil {
				return nil, nil, nil, fmt.Errorf("parsing transform %s: %w", name, err)
			}
			transforms[dirName] = f
		} else {
			return nil, nil, nil, fmt.Errorf("unexpected file in transform directory: %s", name)
		}
	}

	return base, transforms, companions, nil
}

// buildDesired scans the repo and builds the desired state map.
// base is the optional base transform applied to all containers.
func buildDesired(repoPath string, base *INIFile, transforms map[string]*INIFile, companions []CompanionTemplate) (map[string]DesiredState, error) {
	desired := map[string]DesiredState{}
	sources := map[string]string{} // name → source path (for collision detection)

	// Root-level .container files — base transform only
	rootFiles, err := filepath.Glob(filepath.Join(repoPath, "*.container"))
	if err != nil {
		return nil, err
	}
	for _, f := range rootFiles {
		name := strings.TrimSuffix(filepath.Base(f), ".container")
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", f, err)
		}
		var content string
		if base != nil {
			spec, err := ParseINI(strings.NewReader(string(data)))
			if err != nil {
				return nil, fmt.Errorf("parsing %s: %w", f, err)
			}
			content = applyTransforms(spec, []*INIFile{base}).String()
		} else {
			content = string(data)
		}
		desired[name] = buildDesiredState(name, content, companions)
		sources[name] = f
	}

	// Subdirectories — apply base + matching transform
	entries, err := os.ReadDir(repoPath)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dirName := entry.Name()
		transform, ok := transforms[dirName]
		if !ok {
			return nil, fmt.Errorf("no transform for directory %s", dirName)
		}

		subFiles, err := filepath.Glob(filepath.Join(repoPath, dirName, "*.container"))
		if err != nil {
			return nil, err
		}
		for _, f := range subFiles {
			name := strings.TrimSuffix(filepath.Base(f), ".container")
			if prev, exists := sources[name]; exists {
				return nil, fmt.Errorf("duplicate container name %q: %s and %s", name, prev, f)
			}
			data, err := os.ReadFile(f)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", f, err)
			}
			spec, err := ParseINI(strings.NewReader(string(data)))
			if err != nil {
				return nil, fmt.Errorf("parsing %s: %w", f, err)
			}
			var tList []*INIFile
			if base != nil {
				tList = append(tList, base)
			}
			tList = append(tList, transform)
			content := applyTransforms(spec, tList).String()
			desired[name] = buildDesiredState(name, content, companions)
			sources[name] = f
		}
	}

	return desired, nil
}

// buildDesiredState creates a DesiredState for a container, including its
// main .container file and any companion files from templates.
func buildDesiredState(name, containerContent string, companions []CompanionTemplate) DesiredState {
	files := map[string]string{name + ".container": containerContent}
	for _, c := range companions {
		filename := name + c.SuffixAndExt
		content := strings.ReplaceAll(c.Content, "{{.Name}}", name)
		files[filename] = content
	}
	return DesiredState{Files: files}
}

func specChanged(hashDir, name string, state DesiredState) bool {
	hashFile := filepath.Join(hashDir, name)
	existing, err := os.ReadFile(hashFile)
	if err != nil {
		return true // file doesn't exist, treat as changed
	}
	return strings.TrimSpace(string(existing)) != compositeHash(state)
}

func saveHash(hashDir, name string, state DesiredState) {
	hashFile := filepath.Join(hashDir, name)
	os.WriteFile(hashFile, []byte(compositeHash(state)), 0644)
}

// compositeHash computes a single hash over all files in a DesiredState,
// sorted by filename for determinism.
func compositeHash(state DesiredState) string {
	h := sha256.New()
	names := make([]string, 0, len(state.Files))
	for n := range state.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		h.Write([]byte(n))
		h.Write([]byte(state.Files[n]))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
