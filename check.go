package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var validNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
var validPodNameRe = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// Username is a validated Linux username suitable for user creation and systemd
// operations. Construct via NewUsername or NewPodUsername; the underlying string
// is guaranteed to match [a-z][a-z0-9-]* and be at most 32 characters.
type Username string

func NewUsername(s string) (Username, error) {
	if len(s) > 32 {
		return "", fmt.Errorf("name %q exceeds 32 characters", s)
	}
	if !validNameRe.MatchString(s) {
		return "", fmt.Errorf("name %q is not a valid username ([a-z][a-z0-9-]*)", s)
	}
	return Username(s), nil
}

func NewPodUsername(s string) (Username, error) {
	if len(s) > 32 {
		return "", fmt.Errorf("pod name %q exceeds 32 characters", s)
	}
	if !validPodNameRe.MatchString(s) {
		return "", fmt.Errorf("pod name %q is not valid ([a-z][a-z0-9]*)", s)
	}
	return Username(s), nil
}

// SubdirSpecs holds discovered container, pod, and sidecar files in a scope
// (root of the repo or a single non-dot subdirectory).
type SubdirSpecs struct {
	Containers []string
	Pods       []string
	Services   []string // *.service (plain systemd unit, sidecar of a .container)
	Timers     []string // *.timer (plain systemd unit, sidecar of a .container)
}

// discoverContainers finds deployable files using the same two-level layout
// that buildDesired uses: root-level files and one level of non-dot
// subdirectories. Returns the root scope and a map of subdirectory scopes.
func discoverContainers(repoPath string) (root SubdirSpecs, subdirs map[string]SubdirSpecs, err error) {
	subdirs = map[string]SubdirSpecs{}

	root, err = globScope(repoPath)
	if err != nil {
		return SubdirSpecs{}, nil, err
	}

	entries, err := os.ReadDir(repoPath)
	if err != nil {
		return SubdirSpecs{}, nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dirName := entry.Name()
		spec, err := globScope(filepath.Join(repoPath, dirName))
		if err != nil {
			return SubdirSpecs{}, nil, err
		}
		if len(spec.Containers) > 0 || len(spec.Pods) > 0 || len(spec.Services) > 0 || len(spec.Timers) > 0 {
			subdirs[dirName] = spec
		}
	}

	return root, subdirs, nil
}

// globScope finds all deployable files in a single directory (no recursion).
func globScope(dir string) (SubdirSpecs, error) {
	var spec SubdirSpecs
	var err error
	if spec.Containers, err = filepath.Glob(filepath.Join(dir, "*.container")); err != nil {
		return SubdirSpecs{}, err
	}
	if spec.Pods, err = filepath.Glob(filepath.Join(dir, "*.pod")); err != nil {
		return SubdirSpecs{}, err
	}
	if spec.Services, err = filepath.Glob(filepath.Join(dir, "*.service")); err != nil {
		return SubdirSpecs{}, err
	}
	if spec.Timers, err = filepath.Glob(filepath.Join(dir, "*.timer")); err != nil {
		return SubdirSpecs{}, err
	}
	return spec, nil
}

// findSidecarOwner returns the container stem that owns the given sidecar
// file by longest-prefix match. The match rule is: the sidecar's
// filename-without-extension starts with "<stem>-" for some stem in
// containerStems. The longest such stem wins. Returns ("", false) if no
// container claims the sidecar.
func findSidecarOwner(sidecarFile string, containerStems []string) (string, bool) {
	stem := strings.TrimSuffix(filepath.Base(sidecarFile), filepath.Ext(sidecarFile))
	best := ""
	for _, c := range containerStems {
		if strings.HasPrefix(stem, c+"-") && len(c) > len(best) {
			best = c
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

// containerStemsOf extracts ".container" stems from a slice of file paths.
func containerStemsOf(containerFiles []string) []string {
	stems := make([]string, 0, len(containerFiles))
	for _, f := range containerFiles {
		stems = append(stems, strings.TrimSuffix(filepath.Base(f), ".container"))
	}
	return stems
}

// CheckDir validates all .container, .pod, .service, and .timer files in a
// directory. Returns a list of errors found.
func CheckDir(dir string) []error {
	var errs []error

	root, subdirs, err := discoverContainers(dir)
	if err != nil {
		return []error{fmt.Errorf("reading directory %s: %w", dir, err)}
	}

	// Root scope: containers are standalone. Sidecars must match a root container.
	for _, f := range root.Containers {
		errs = append(errs, checkFile(f, false)...)
	}
	for _, f := range root.Pods {
		errs = append(errs, checkPodFile(f)...)
	}
	errs = append(errs, checkSidecars(root, "root", containerStemsOf(root.Containers))...)

	for dirName, specs := range subdirs {
		if len(specs.Pods) == 0 {
			// No pods — all containers are standalone; sidecars match any container in the subdir.
			for _, f := range specs.Containers {
				errs = append(errs, checkFile(f, false)...)
			}
			errs = append(errs, checkSidecars(specs, dirName, containerStemsOf(specs.Containers))...)
			continue
		}

		// Validate pod files
		podStems := map[string]bool{}
		for _, f := range specs.Pods {
			errs = append(errs, checkPodFile(f)...)
			stem := strings.TrimSuffix(filepath.Base(f), ".pod")
			podStems[stem] = true
		}

		// Classify containers as pod members or orphans
		for _, f := range specs.Containers {
			name := strings.TrimSuffix(filepath.Base(f), ".container")
			isMember := false
			for stem := range podStems {
				if strings.HasPrefix(name, stem+"-") {
					isMember = true
					break
				}
			}
			if !isMember {
				errs = append(errs, fmt.Errorf("%s: container in directory %s does not belong to any pod (no matching pod prefix)", f, dirName))
				continue
			}
			errs = append(errs, checkFile(f, true)...)
		}

		// Sidecars in a pod subdir must match a member container.
		errs = append(errs, checkSidecars(specs, dirName, containerStemsOf(specs.Containers))...)
	}

	return errs
}

// checkSidecars validates .service and .timer files in a scope: each must
// parse, have the right section, and match a .container in the scope by
// filename prefix. dirName is "root" or a subdirectory name (for messages).
func checkSidecars(specs SubdirSpecs, dirName string, containerStems []string) []error {
	var errs []error
	for _, f := range specs.Services {
		errs = append(errs, checkSidecarFile(f, ".service", "Service", dirName, containerStems)...)
	}
	for _, f := range specs.Timers {
		errs = append(errs, checkSidecarFile(f, ".timer", "Timer", dirName, containerStems)...)
	}
	return errs
}

// checkSidecarFile validates a single sidecar file.
func checkSidecarFile(path, ext, requiredSection, dirName string, containerStems []string) []error {
	var errs []error

	if _, ok := findSidecarOwner(path, containerStems); !ok {
		errs = append(errs, fmt.Errorf("%s: %s in %s has no matching <stem>.container (filename must start with `<container-stem>-`)", path, ext, dirName))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", path, err))
		return errs
	}

	f, err := ParseINI(strings.NewReader(string(data)))
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: parse error: %w", path, err))
		return errs
	}
	if f.GetSection(requiredSection) == nil {
		errs = append(errs, fmt.Errorf("%s: missing [%s] section", path, requiredSection))
	}
	return errs
}

func checkFile(path string, isPodMember bool) []error {
	var errs []error

	name := strings.TrimSuffix(filepath.Base(path), ".container")

	if !isPodMember {
		if _, err := NewUsername(name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return []error{fmt.Errorf("%s: %w", path, err)}
	}

	errs = append(errs, checkContent(name, string(data), path)...)
	return errs
}

func checkPodFile(path string) []error {
	var errs []error

	name := strings.TrimSuffix(filepath.Base(path), ".pod")

	if _, err := NewPodUsername(name); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", path, err))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return []error{fmt.Errorf("%s: %w", path, err)}
	}

	errs = append(errs, checkPodContent(name, string(data), path)...)
	return errs
}

// checkPodContent validates parsed INI content for a .pod file.
func checkPodContent(_, content, source string) []error {
	var errs []error

	f, err := ParseINI(strings.NewReader(content))
	if err != nil {
		return []error{fmt.Errorf("%s: parse error: %w", source, err)}
	}

	if f.GetSection("Pod") == nil {
		errs = append(errs, fmt.Errorf("%s: missing [Pod] section", source))
	}

	return errs
}

// checkContent validates parsed INI content for a .container file.
// source is a human-readable label for error messages.
func checkContent(name, content, source string) []error {
	var errs []error

	f, err := ParseINI(strings.NewReader(content))
	if err != nil {
		return []error{fmt.Errorf("%s: parse error: %w", source, err)}
	}
	if err := validateSecretsSection(f); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", source, err))
	}

	// Must have [Container] section with Image=
	container := f.GetSection("Container")
	if container == nil {
		errs = append(errs, fmt.Errorf("%s: missing [Container] section", source))
	} else {
		if !container.HasKey("Image") {
			errs = append(errs, fmt.Errorf("%s: missing Image= in [Container]", source))
		}
		// Specs must not contain Pod= — quadsync injects it automatically
		if container.HasKey("Pod") {
			errs = append(errs, fmt.Errorf("%s: Pod= must not be set in container spec (quadsync injects it automatically)", source))
		}
	}

	// If ContainerName is set, it must match the filename stem
	if container != nil {
		for _, e := range container.Entries {
			if e.Key == "ContainerName" && e.Value != name {
				errs = append(errs, fmt.Errorf("%s: ContainerName=%s does not match filename stem %s", source, e.Value, name))
			}
		}
	}

	return errs
}

// CheckDesired validates .container and .pod files in each DesiredState entry.
// Companion files (.volume etc.) are not validated as container specs.
func CheckDesired(desired map[Username]DesiredState) []error {
	var errs []error
	for name, state := range desired {
		// Validate pod file if present
		podFile := string(name) + ".pod"
		if content, ok := state.Files[podFile]; ok {
			source := fmt.Sprintf("merged output for %s", name)
			errs = append(errs, checkPodContent(string(name), content, source)...)
		}

		// Validate all .container files in this state
		for filename, content := range state.Files {
			if !strings.HasSuffix(filename, ".container") {
				continue
			}
			containerName := strings.TrimSuffix(filename, ".container")
			source := fmt.Sprintf("merged output for %s", containerName)
			errs = append(errs, checkContentPostMerge(containerName, content, source)...)
		}
	}
	return errs
}

// checkContentPostMerge validates a container after transforms have been applied.
// Unlike checkContent, it allows Pod= (since quadsync injects it).
func checkContentPostMerge(name, content, source string) []error {
	var errs []error

	f, err := ParseINI(strings.NewReader(content))
	if err != nil {
		return []error{fmt.Errorf("%s: parse error: %w", source, err)}
	}

	container := f.GetSection("Container")
	if container == nil {
		errs = append(errs, fmt.Errorf("%s: missing [Container] section", source))
	} else if !container.HasKey("Image") {
		errs = append(errs, fmt.Errorf("%s: missing Image= in [Container]", source))
	}

	if container != nil {
		for _, e := range container.Entries {
			if e.Key == "ContainerName" && e.Value != name {
				errs = append(errs, fmt.Errorf("%s: ContainerName=%s does not match filename stem %s", source, e.Value, name))
			}
		}
	}

	if f.GetSection(sectionSecrets) != nil {
		errs = append(errs, fmt.Errorf("%s: [Secrets] section should have been stripped", source))
	}

	return errs
}
