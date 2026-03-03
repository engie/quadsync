package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestDiscoverContainers(t *testing.T) {
	t.Run("root files", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "app.container"), []byte("[Container]\nImage=x\n"), 0644)
		os.WriteFile(filepath.Join(dir, "web.container"), []byte("[Container]\nImage=x\n"), 0644)

		root, subdirs, err := discoverContainers(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(root) != 2 {
			t.Fatalf("expected 2 root files, got %d", len(root))
		}
		if len(subdirs) != 0 {
			t.Fatalf("expected 0 subdirs, got %d", len(subdirs))
		}
	})

	t.Run("subdirectory files", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "staging")
		os.Mkdir(sub, 0755)
		os.WriteFile(filepath.Join(sub, "svc.container"), []byte("[Container]\nImage=x\n"), 0644)

		root, subdirs, err := discoverContainers(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(root) != 0 {
			t.Fatalf("expected 0 root files, got %d", len(root))
		}
		if len(subdirs) != 1 {
			t.Fatalf("expected 1 subdir, got %d", len(subdirs))
		}
		files := subdirs["staging"]
		if len(files) != 1 {
			t.Fatalf("expected 1 file in staging, got %d", len(files))
		}
	})

	t.Run("dot directories skipped", func(t *testing.T) {
		dir := t.TempDir()
		dot := filepath.Join(dir, ".git")
		os.Mkdir(dot, 0755)
		os.WriteFile(filepath.Join(dot, "config.container"), []byte("[Container]\nImage=x\n"), 0644)

		root, subdirs, err := discoverContainers(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(root) != 0 {
			t.Fatalf("expected 0 root files, got %d", len(root))
		}
		if len(subdirs) != 0 {
			t.Fatalf("expected 0 subdirs, got %d", len(subdirs))
		}
	})

	t.Run("non-container files ignored", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0644)
		os.WriteFile(filepath.Join(dir, "app.volume"), []byte("[Volume]\n"), 0644)
		os.WriteFile(filepath.Join(dir, "svc.container"), []byte("[Container]\nImage=x\n"), 0644)

		root, _, err := discoverContainers(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(root) != 1 {
			t.Fatalf("expected 1 root file, got %d", len(root))
		}
	})

	t.Run("deeply nested files ignored", func(t *testing.T) {
		dir := t.TempDir()
		deep := filepath.Join(dir, "a", "b")
		os.MkdirAll(deep, 0755)
		os.WriteFile(filepath.Join(deep, "deep.container"), []byte("[Container]\nImage=x\n"), 0644)

		root, subdirs, err := discoverContainers(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(root) != 0 {
			t.Fatalf("expected 0 root files, got %d", len(root))
		}
		// "a" subdir is found but "b" inside it is not — only one level
		if files, ok := subdirs["a"]; ok {
			if len(files) != 0 {
				t.Fatalf("expected 0 files in subdir a, got %d", len(files))
			}
		}
	})

	t.Run("mixed root and subdirectory", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "root.container"), []byte("[Container]\nImage=x\n"), 0644)
		sub := filepath.Join(dir, "prod")
		os.Mkdir(sub, 0755)
		os.WriteFile(filepath.Join(sub, "api.container"), []byte("[Container]\nImage=x\n"), 0644)
		os.WriteFile(filepath.Join(sub, "web.container"), []byte("[Container]\nImage=x\n"), 0644)

		root, subdirs, err := discoverContainers(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(root) != 1 {
			t.Fatalf("expected 1 root file, got %d", len(root))
		}
		files := subdirs["prod"]
		if len(files) != 2 {
			t.Fatalf("expected 2 files in prod, got %d", len(files))
		}
		// Verify filenames
		var names []string
		for _, f := range files {
			names = append(names, filepath.Base(f))
		}
		sort.Strings(names)
		if names[0] != "api.container" || names[1] != "web.container" {
			t.Fatalf("unexpected files: %v", names)
		}
	})
}
