package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var validNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// discoverContainers finds deployable .container files using the same two-level
// layout that buildDesired uses: root-level files and one level of non-dot
// subdirectories. Returns root files and a map of subdirectory name to files.
func discoverContainers(repoPath string) (root []string, subdirs map[string][]string, err error) {
	subdirs = map[string][]string{}

	rootFiles, err := filepath.Glob(filepath.Join(repoPath, "*.container"))
	if err != nil {
		return nil, nil, err
	}
	root = rootFiles

	entries, err := os.ReadDir(repoPath)
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dirName := entry.Name()
		subFiles, err := filepath.Glob(filepath.Join(repoPath, dirName, "*.container"))
		if err != nil {
			return nil, nil, err
		}
		if len(subFiles) > 0 {
			subdirs[dirName] = subFiles
		}
	}

	return root, subdirs, nil
}

// CheckDir validates all .container files in a directory.
// Returns a list of errors found.
func CheckDir(dir string) []error {
	var errs []error

	root, subdirs, err := discoverContainers(dir)
	if err != nil {
		return []error{fmt.Errorf("reading directory %s: %w", dir, err)}
	}

	for _, f := range root {
		errs = append(errs, checkFile(f)...)
	}
	for _, files := range subdirs {
		for _, f := range files {
			errs = append(errs, checkFile(f)...)
		}
	}

	return errs
}

func checkFile(path string) []error {
	var errs []error

	name := strings.TrimSuffix(filepath.Base(path), ".container")

	// Validate filename is a valid Linux username
	if len(name) > 32 {
		errs = append(errs, fmt.Errorf("%s: filename stem exceeds 32 characters", path))
	}
	if !validNameRe.MatchString(name) {
		errs = append(errs, fmt.Errorf("%s: filename stem %q is not a valid username ([a-z][a-z0-9-]*)", path, name))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return []error{fmt.Errorf("%s: %w", path, err)}
	}

	f, err := ParseINI(strings.NewReader(string(data)))
	if err != nil {
		return []error{fmt.Errorf("%s: parse error: %w", path, err)}
	}

	// Must have [Container] section with Image=
	container := f.GetSection("Container")
	if container == nil {
		errs = append(errs, fmt.Errorf("%s: missing [Container] section", path))
	} else if !container.HasKey("Image") {
		errs = append(errs, fmt.Errorf("%s: missing Image= in [Container]", path))
	}

	// If ContainerName is set, it must match the filename stem
	if container != nil {
		for _, e := range container.Entries {
			if e.Key == "ContainerName" && e.Value != name {
				errs = append(errs, fmt.Errorf("%s: ContainerName=%s does not match filename stem %s", path, e.Value, name))
			}
		}
	}

	return errs
}
