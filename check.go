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

// SubdirSpecs holds discovered container and pod files in a subdirectory.
type SubdirSpecs struct {
	Containers []string
	Pods       []string
}

// discoverContainers finds deployable .container and .pod files using the same
// two-level layout that buildDesired uses: root-level files and one level of
// non-dot subdirectories.
func discoverContainers(repoPath string) (rootContainers []string, rootPods []string, subdirs map[string]SubdirSpecs, err error) {
	subdirs = map[string]SubdirSpecs{}

	rootContainers, err = filepath.Glob(filepath.Join(repoPath, "*.container"))
	if err != nil {
		return nil, nil, nil, err
	}
	rootPods, err = filepath.Glob(filepath.Join(repoPath, "*.pod"))
	if err != nil {
		return nil, nil, nil, err
	}

	entries, err := os.ReadDir(repoPath)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dirName := entry.Name()
		containerFiles, err := filepath.Glob(filepath.Join(repoPath, dirName, "*.container"))
		if err != nil {
			return nil, nil, nil, err
		}
		podFiles, err := filepath.Glob(filepath.Join(repoPath, dirName, "*.pod"))
		if err != nil {
			return nil, nil, nil, err
		}
		if len(containerFiles) > 0 || len(podFiles) > 0 {
			subdirs[dirName] = SubdirSpecs{
				Containers: containerFiles,
				Pods:       podFiles,
			}
		}
	}

	return rootContainers, rootPods, subdirs, nil
}

// CheckDir validates all .container and .pod files in a directory.
// Returns a list of errors found.
func CheckDir(dir string) []error {
	var errs []error

	rootContainers, rootPods, subdirs, err := discoverContainers(dir)
	if err != nil {
		return []error{fmt.Errorf("reading directory %s: %w", dir, err)}
	}

	// Validate root-level containers (standalone)
	for _, f := range rootContainers {
		errs = append(errs, checkFile(f, false)...)
	}
	// Validate root-level pods
	for _, f := range rootPods {
		errs = append(errs, checkPodFile(f)...)
	}

	for dirName, specs := range subdirs {
		if len(specs.Pods) == 0 {
			// No pods — all containers are standalone
			for _, f := range specs.Containers {
				errs = append(errs, checkFile(f, false)...)
			}
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

	if f.GetSection(sectionSecrets) != nil {
		errs = append(errs, fmt.Errorf("%s: [Secrets] section should have been stripped", source))
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

	return errs
}
