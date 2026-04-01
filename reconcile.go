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

// ContainerSecret pairs a secret with the container name used when the
// Secret= directive was injected into the quadlet.
type ContainerSecret struct {
	ContainerName string
	Entry         SecretEntry
}

// DesiredState holds all quadlet files for a single container user.
type DesiredState struct {
	Files       map[string]string // filename → content (e.g. "myapp.container", "myapp-data.volume")
	ServiceName string            // systemd service to restart (e.g. "nginx-demo" for standalone, "webapp-pod" for pods)
	Secrets     []ContainerSecret
}

// Config holds the deployer configuration.
type Config struct {
	GitURL       string
	GitBranch    string
	TransformDir string
	StateDir     string
	UserGroup    string
	SSHKey       string // path to SSH deploy key for git
	AgeKeyFile   string // path to age private key for inline secret decryption
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
		AgeKeyFile:   env["QUADSYNC_AGE_KEY"],
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
	transforms, err := loadAllTransforms(config.TransformDir)
	if err != nil {
		return fmt.Errorf("loading transforms: %w", err)
	}

	// 4. Build desired state
	transforms.AgeKeyFile = config.AgeKeyFile
	desired, err := buildDesiredFull(config.RepoPath, transforms)
	if err != nil {
		return fmt.Errorf("building desired state: %w", err)
	}

	// 5. Validate merged output
	if errs := CheckDesired(desired); len(errs) > 0 {
		for _, e := range errs {
			log.Printf("post-merge validation error: %v", e)
		}
		return fmt.Errorf("post-merge validation failed: %d error(s)", len(errs))
	}

	// 6. Get current managed users
	current, err := managedUsers(config.UserGroup)
	if err != nil {
		return fmt.Errorf("listing managed users: %w", err)
	}
	currentSet := map[Username]bool{}
	for _, u := range current {
		currentSet[u] = true
	}

	// 7. Deploy
	hashDir := filepath.Join(config.StateDir, "hashes")
	if err := os.MkdirAll(hashDir, 0755); err != nil {
		return fmt.Errorf("creating hash dir: %w", err)
	}

	// Deploy loop error policy: quadsync reports failure for its own
	// mechanisms (user creation, quadlet writing, daemon-reload). If a
	// container fails to start, that is the container's problem — we log
	// it as a warning but do not count it as a quadsync failure.
	var errs []error

	names := make([]Username, 0, len(desired))
	for name := range desired {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })

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
		if err := waitForUserManager(name); err != nil {
			log.Printf("error waiting for user manager %s: %v", name, err)
			errs = append(errs, fmt.Errorf("waiting for user manager %s: %w", name, err))
			continue
		}
		if len(state.Secrets) > 0 {
			if err := createPodmanSecrets(name, state.Secrets); err != nil {
				log.Printf("error creating secrets for %s: %v", name, err)
				errs = append(errs, fmt.Errorf("creating secrets for %s: %w", name, err))
				continue
			}
		}
		if err := daemonReload(name); err != nil {
			log.Printf("error daemon-reload for %s: %v", name, err)
			errs = append(errs, fmt.Errorf("daemon-reload for %s: %w", name, err))
			continue
		}

		// Quadlet is written and systemd knows about it — quadsync's job
		// is done. Persist the hash so we don't re-deploy next cycle.
		if err := saveHash(hashDir, name, state); err != nil {
			log.Printf("error saving hash for %s: %v", name, err)
			errs = append(errs, fmt.Errorf("saving hash for %s: %w", name, err))
		}

		// Best-effort service restart. If the container fails to come up
		// that is the container's concern, not ours.
		if err := restartService(name, state.ServiceName); err != nil {
			log.Printf("warning: restarting %s: %v (container may need attention)", name, err)
		}
	}

	// 8. Cleanup: remove containers not in desired
	for _, name := range current {
		if _, exists := desired[name]; !exists {
			log.Printf("%s: removing", name)
			if err := stopService(name, string(name)); err != nil {
				log.Printf("warning: stopping %s: %v", name, err)
			}
			if err := removeAllQuadlets(name); err != nil {
				log.Printf("warning: removing quadlets for %s: %v", name, err)
			}
			if err := deleteUser(name); err != nil {
				log.Printf("error deleting user %s: %v", name, err)
				errs = append(errs, fmt.Errorf("deleting user %s: %w", name, err))
			}
			os.Remove(filepath.Join(hashDir, string(name)))
		}
	}

	return errors.Join(errs...)
}

// Transforms holds all loaded transform data from the transform directory.
type Transforms struct {
	Base         *INIFile            // from _base.container, applied to all .container files
	BasePod      *INIFile            // from _base.pod, applied to all .pod files
	DirContainer map[string]*INIFile // directory-specific .container transforms
	DirPod       map[string]*INIFile // directory-specific .pod transforms
	Companions   []CompanionTemplate
	AgeKeyFile   string
}

// loadTransforms reads all files from the transform directory.
// Returns a base transform (from _base.container, may be nil), a map of
// directory name → parsed INI for directory-specific transforms, and a list
// of companion templates (files matching _base-<suffix>.<ext>).
func loadTransforms(dir string) (*INIFile, map[string]*INIFile, []CompanionTemplate, error) {
	t, err := loadAllTransforms(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	return t.Base, t.DirContainer, t.Companions, nil
}

// loadAllTransforms reads all files from the transform directory including pod transforms.
func loadAllTransforms(dir string) (Transforms, error) {
	t := Transforms{
		DirContainer: map[string]*INIFile{},
		DirPod:       map[string]*INIFile{},
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return t, nil
		}
		return Transforms{}, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()

		if name == "_base.container" {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return Transforms{}, fmt.Errorf("reading transform %s: %w", name, err)
			}
			f, err := ParseINI(strings.NewReader(string(data)))
			if err != nil {
				return Transforms{}, fmt.Errorf("parsing transform %s: %w", name, err)
			}
			t.Base = f
		} else if name == "_base.pod" {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return Transforms{}, fmt.Errorf("reading transform %s: %w", name, err)
			}
			f, err := ParseINI(strings.NewReader(string(data)))
			if err != nil {
				return Transforms{}, fmt.Errorf("parsing transform %s: %w", name, err)
			}
			t.BasePod = f
		} else if strings.HasPrefix(name, "_base-") {
			suffixAndExt := strings.TrimPrefix(name, "_base")
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return Transforms{}, fmt.Errorf("reading companion %s: %w", name, err)
			}
			t.Companions = append(t.Companions, CompanionTemplate{
				SuffixAndExt: suffixAndExt,
				Content:      string(data),
			})
		} else if strings.HasSuffix(name, ".pod") {
			dirName := strings.TrimSuffix(name, ".pod")
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return Transforms{}, fmt.Errorf("reading transform %s: %w", name, err)
			}
			f, err := ParseINI(strings.NewReader(string(data)))
			if err != nil {
				return Transforms{}, fmt.Errorf("parsing transform %s: %w", name, err)
			}
			t.DirPod[dirName] = f
		} else if strings.HasSuffix(name, ".container") {
			dirName := strings.TrimSuffix(name, ".container")
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return Transforms{}, fmt.Errorf("reading transform %s: %w", name, err)
			}
			f, err := ParseINI(strings.NewReader(string(data)))
			if err != nil {
				return Transforms{}, fmt.Errorf("parsing transform %s: %w", name, err)
			}
			t.DirContainer[dirName] = f
		} else {
			return Transforms{}, fmt.Errorf("unexpected file in transform directory: %s", name)
		}
	}

	return t, nil
}

// buildDesired scans the repo and builds the desired state map.
// base is the optional base transform applied to all containers.
func buildDesired(repoPath string, base *INIFile, transforms map[string]*INIFile, companions []CompanionTemplate) (map[Username]DesiredState, error) {
	t := Transforms{
		Base:         base,
		DirContainer: transforms,
		DirPod:       map[string]*INIFile{},
		Companions:   companions,
	}
	return buildDesiredFull(repoPath, t)
}

// buildDesiredFull scans the repo and builds the desired state map using full transforms.
func buildDesiredFull(repoPath string, t Transforms) (map[Username]DesiredState, error) {
	desired := map[Username]DesiredState{}
	sources := map[Username]string{} // name → source path (for collision detection)

	rootContainers, rootPods, subdirSpecs, err := discoverContainers(repoPath)
	if err != nil {
		return nil, err
	}

	// Build set of root pod stems to identify pod members
	rootPodStems := map[string]string{} // stem → pod file path
	for _, f := range rootPods {
		stem := strings.TrimSuffix(filepath.Base(f), ".pod")
		rootPodStems[stem] = f
	}

	// Group root containers by pod membership
	rootStandalone := []string{}
	rootPodMembers := map[string][]string{} // pod stem → member files
	for _, f := range rootContainers {
		name := strings.TrimSuffix(filepath.Base(f), ".container")
		memberOf := ""
		for stem := range rootPodStems {
			if strings.HasPrefix(name, stem+"-") {
				memberOf = stem
				break
			}
		}
		if memberOf != "" {
			rootPodMembers[memberOf] = append(rootPodMembers[memberOf], f)
		} else {
			rootStandalone = append(rootStandalone, f)
		}
	}

	// Root-level standalone .container files — base transform only
	for _, f := range rootStandalone {
		name, err := NewUsername(strings.TrimSuffix(filepath.Base(f), ".container"))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		content, secrets, err := transformContainerFile(f, t.Base, nil, t.AgeKeyFile)
		if err != nil {
			return nil, err
		}
		desired[name] = buildDesiredState(name, content, t.Companions, secrets)
		sources[name] = f
	}

	// Root-level pods
	for stem, podFile := range rootPodStems {
		name, err := NewPodUsername(stem)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", podFile, err)
		}
		if prev, exists := sources[name]; exists {
			return nil, fmt.Errorf("duplicate name %q: %s and %s", name, prev, podFile)
		}
		members := rootPodMembers[stem]
		state, err := buildPodDesired(stem, podFile, members, t, nil)
		if err != nil {
			return nil, err
		}
		desired[name] = state
		sources[name] = podFile
	}

	// Subdirectories
	for dirName, specs := range subdirSpecs {
		if len(specs.Pods) == 0 {
			// No pods — all containers are standalone
			dirTransform := t.DirContainer[dirName]
			if dirTransform == nil {
				return nil, fmt.Errorf("no transform for directory %s", dirName)
			}

			for _, f := range specs.Containers {
				name, err := NewUsername(strings.TrimSuffix(filepath.Base(f), ".container"))
				if err != nil {
					return nil, fmt.Errorf("%s: %w", f, err)
				}
				if prev, exists := sources[name]; exists {
					return nil, fmt.Errorf("duplicate container name %q: %s and %s", name, prev, f)
				}
				content, secrets, err := transformContainerFile(f, t.Base, dirTransform, t.AgeKeyFile)
				if err != nil {
					return nil, err
				}
				desired[name] = buildDesiredState(name, content, t.Companions, secrets)
				sources[name] = f
			}
			continue
		}

		// Directory has pods — group containers by pod
		podStems := map[string]string{} // stem → pod file path
		for _, f := range specs.Pods {
			stem := strings.TrimSuffix(filepath.Base(f), ".pod")
			podStems[stem] = f
		}

		podMembers := map[string][]string{} // pod stem → member files
		for _, f := range specs.Containers {
			name := strings.TrimSuffix(filepath.Base(f), ".container")
			memberOf := ""
			for stem := range podStems {
				if strings.HasPrefix(name, stem+"-") {
					memberOf = stem
					break
				}
			}
			if memberOf == "" {
				return nil, fmt.Errorf("%s: container does not belong to any pod in directory %s", f, dirName)
			}
			podMembers[memberOf] = append(podMembers[memberOf], f)
		}

		for stem, podFile := range podStems {
			name, err := NewPodUsername(stem)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", podFile, err)
			}
			if prev, exists := sources[name]; exists {
				return nil, fmt.Errorf("duplicate name %q: %s and %s", name, prev, podFile)
			}
			members := podMembers[stem]
			state, err := buildPodDesired(stem, podFile, members, t, &dirName)
			if err != nil {
				return nil, err
			}
			desired[name] = state
			sources[name] = podFile
		}
	}

	return desired, nil
}

// transformContainerFile reads and applies transforms to a container file.
func transformContainerFile(path string, base, dirTransform *INIFile, ageKeyFile string) (string, []SecretEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("reading %s: %w", path, err)
	}
	spec, err := ParseINI(strings.NewReader(string(data)))
	if err != nil {
		return "", nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	secrets, err := parseSecrets(spec, ageKeyFile)
	if err != nil {
		return "", nil, fmt.Errorf("parsing secrets in %s: %w", path, err)
	}

	stripSecretsSections(spec)
	if len(secrets) > 0 {
		containerName := strings.TrimSuffix(filepath.Base(path), ".container")
		injectSecretDirectives(spec, containerName, secrets)
	}

	var tList []*INIFile
	if base != nil {
		tList = append(tList, base)
	}
	if dirTransform != nil {
		tList = append(tList, dirTransform)
	}
	if len(tList) > 0 {
		spec = applyTransforms(spec, tList)
	}
	return spec.String(), secrets, nil
}

// buildPodDesired builds a DesiredState for a pod and its members.
// dirName is nil for root-level pods.
func buildPodDesired(podStem, podFile string, memberFiles []string, t Transforms, dirName *string) (DesiredState, error) {
	files := map[string]string{}
	var allSecrets []ContainerSecret

	// Process pod file
	podData, err := os.ReadFile(podFile)
	if err != nil {
		return DesiredState{}, fmt.Errorf("reading %s: %w", podFile, err)
	}
	var podTList []*INIFile
	if t.BasePod != nil {
		podTList = append(podTList, t.BasePod)
	}
	if dirName != nil {
		if dt, ok := t.DirPod[*dirName]; ok {
			podTList = append(podTList, dt)
		}
	}
	var podContent string
	if len(podTList) > 0 {
		spec, err := ParseINI(strings.NewReader(string(podData)))
		if err != nil {
			return DesiredState{}, fmt.Errorf("parsing %s: %w", podFile, err)
		}
		podContent = applyTransforms(spec, podTList).String()
	} else {
		podContent = string(podData)
	}
	podContent = strings.ReplaceAll(podContent, "{{.Name}}", podStem)
	files[podStem+".pod"] = podContent

	podFilename := podStem + ".pod"

	// Process each member
	for _, f := range memberFiles {
		memberFullName := strings.TrimSuffix(filepath.Base(f), ".container")

		var dirTransform *INIFile
		if dirName != nil {
			dirTransform = t.DirContainer[*dirName]
		}
		content, memberSecrets, err := transformContainerFile(f, t.Base, dirTransform, t.AgeKeyFile)
		if err != nil {
			return DesiredState{}, err
		}
		for _, s := range memberSecrets {
			allSecrets = append(allSecrets, ContainerSecret{ContainerName: memberFullName, Entry: s})
		}

		// Inject Pod= into the container
		ini, err := ParseINI(strings.NewReader(content))
		if err != nil {
			return DesiredState{}, fmt.Errorf("parsing merged %s: %w", f, err)
		}
		injectPod(ini, podFilename)
		content = ini.String()
		content = strings.ReplaceAll(content, "{{.Name}}", memberFullName)
		files[memberFullName+".container"] = content

		// Generate companions for this member
		for _, c := range t.Companions {
			companionFilename := memberFullName + c.SuffixAndExt
			companionContent := strings.ReplaceAll(c.Content, "{{.Name}}", memberFullName)
			// Inject Pod= into companion .container files
			if strings.HasSuffix(c.SuffixAndExt, ".container") {
				cIni, err := ParseINI(strings.NewReader(companionContent))
				if err != nil {
					return DesiredState{}, fmt.Errorf("parsing companion %s: %w", companionFilename, err)
				}
				injectPod(cIni, podFilename)
				companionContent = cIni.String()
			}
			files[companionFilename] = companionContent
		}

	}

	if len(memberFiles) == 0 {
		log.Printf("warning: pod %s has no member containers", podStem)
	}

	return DesiredState{
		Files:       files,
		ServiceName: podStem + "-pod",
		Secrets:     allSecrets,
	}, nil
}

// buildDesiredState creates a DesiredState for a container, including its
// main .container file and any companion files from templates.
func buildDesiredState(name Username, containerContent string, companions []CompanionTemplate, secrets []SecretEntry) DesiredState {
	nameStr := string(name)
	containerContent = strings.ReplaceAll(containerContent, "{{.Name}}", nameStr)
	files := map[string]string{nameStr + ".container": containerContent}
	for _, c := range companions {
		filename := nameStr + c.SuffixAndExt
		content := strings.ReplaceAll(c.Content, "{{.Name}}", nameStr)
		files[filename] = content
	}
	var containerSecrets []ContainerSecret
	for _, s := range secrets {
		containerSecrets = append(containerSecrets, ContainerSecret{ContainerName: nameStr, Entry: s})
	}
	return DesiredState{Files: files, ServiceName: nameStr, Secrets: containerSecrets}
}

func specChanged(hashDir string, name Username, state DesiredState) bool {
	hashFile := filepath.Join(hashDir, string(name))
	existing, err := os.ReadFile(hashFile)
	if err != nil {
		return true // file doesn't exist, treat as changed
	}
	return strings.TrimSpace(string(existing)) != compositeHash(state)
}

func saveHash(hashDir string, name Username, state DesiredState) error {
	hashFile := filepath.Join(hashDir, string(name))
	return os.WriteFile(hashFile, []byte(compositeHash(state)), 0644)
}

// compositeHash computes a single hash over all files and secrets in a
// DesiredState, sorted for determinism.
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
	sortedSecrets := make([]ContainerSecret, len(state.Secrets))
	copy(sortedSecrets, state.Secrets)
	sort.Slice(sortedSecrets, func(i, j int) bool {
		if sortedSecrets[i].ContainerName != sortedSecrets[j].ContainerName {
			return sortedSecrets[i].ContainerName < sortedSecrets[j].ContainerName
		}
		return sortedSecrets[i].Entry.Name < sortedSecrets[j].Entry.Name
	})
	for _, s := range sortedSecrets {
		h.Write([]byte(s.ContainerName))
		h.Write([]byte(s.Entry.Name))
		h.Write([]byte(s.Entry.Type))
		h.Write([]byte(s.Entry.Target))
		h.Write([]byte(s.Entry.Value))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
