package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestCheckContent(t *testing.T) {
	t.Run("valid content", func(t *testing.T) {
		content := "[Container]\nImage=docker.io/library/nginx\n"
		errs := checkContent("myapp", content, "test")
		if len(errs) != 0 {
			t.Fatalf("expected no errors, got %v", errs)
		}
	})

	t.Run("missing Container section", func(t *testing.T) {
		content := "[Service]\nRestart=always\n"
		errs := checkContent("myapp", content, "test")
		if len(errs) != 1 {
			t.Fatalf("expected 1 error, got %v", errs)
		}
		if !strings.Contains(errs[0].Error(), "missing [Container] section") {
			t.Fatalf("unexpected error: %v", errs[0])
		}
	})

	t.Run("missing Image", func(t *testing.T) {
		content := "[Container]\nEnvironment=FOO=bar\n"
		errs := checkContent("myapp", content, "test")
		if len(errs) != 1 {
			t.Fatalf("expected 1 error, got %v", errs)
		}
		if !strings.Contains(errs[0].Error(), "missing Image=") {
			t.Fatalf("unexpected error: %v", errs[0])
		}
	})

	t.Run("ContainerName mismatch", func(t *testing.T) {
		content := "[Container]\nImage=docker.io/library/nginx\nContainerName=other\n"
		errs := checkContent("myapp", content, "test")
		if len(errs) != 1 {
			t.Fatalf("expected 1 error, got %v", errs)
		}
		if !strings.Contains(errs[0].Error(), "ContainerName=other does not match") {
			t.Fatalf("unexpected error: %v", errs[0])
		}
	})

	t.Run("Pod= rejected in spec", func(t *testing.T) {
		content := "[Container]\nImage=nginx\nPod=webapp.pod\n"
		errs := checkContent("myapp", content, "test")
		if len(errs) != 1 {
			t.Fatalf("expected 1 error, got %v", errs)
		}
		if !strings.Contains(errs[0].Error(), "Pod= must not be set") {
			t.Fatalf("unexpected error: %v", errs[0])
		}
	})

	t.Run("file secret name collision rejected", func(t *testing.T) {
		content := "[Container]\nImage=nginx\n\n[Secrets]\nFile=/run/secrets/api-key:aGVsbG8=\nFile=/run/secrets/api_key:d29ybGQ=\n"
		errs := checkContent("myapp", content, "test")
		if len(errs) != 1 {
			t.Fatalf("expected 1 error, got %v", errs)
		}
		if !strings.Contains(errs[0].Error(), "collides with") {
			t.Fatalf("unexpected error: %v", errs[0])
		}
	})
}

func TestCheckPodContent(t *testing.T) {
	t.Run("valid pod", func(t *testing.T) {
		content := "[Pod]\nPodmanArgs=--dns=1.1.1.1\n"
		errs := checkPodContent("webapp", content, "test")
		if len(errs) != 0 {
			t.Fatalf("expected no errors, got %v", errs)
		}
	})

	t.Run("missing Pod section", func(t *testing.T) {
		content := "[Service]\nRestart=always\n"
		errs := checkPodContent("webapp", content, "test")
		if len(errs) != 1 {
			t.Fatalf("expected 1 error, got %v", errs)
		}
		if !strings.Contains(errs[0].Error(), "missing [Pod] section") {
			t.Fatalf("unexpected error: %v", errs[0])
		}
	})
}

func TestCheckPodFile(t *testing.T) {
	t.Run("valid pod name", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "webapp.pod")
		os.WriteFile(f, []byte("[Pod]\n"), 0644)
		errs := checkPodFile(f)
		if len(errs) != 0 {
			t.Fatalf("expected no errors, got %v", errs)
		}
	})

	t.Run("pod name with hyphen rejected", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "web-app.pod")
		os.WriteFile(f, []byte("[Pod]\n"), 0644)
		errs := checkPodFile(f)
		if len(errs) != 1 {
			t.Fatalf("expected 1 error, got %v", errs)
		}
		if !strings.Contains(errs[0].Error(), "not valid") {
			t.Fatalf("unexpected error: %v", errs[0])
		}
	})

	t.Run("pod name too long", func(t *testing.T) {
		dir := t.TempDir()
		f := filepath.Join(dir, "abcdefghijklmnopqrstuvwxyzabcdefgh.pod")
		os.WriteFile(f, []byte("[Pod]\n"), 0644)
		errs := checkPodFile(f)
		hasLenErr := false
		for _, e := range errs {
			if strings.Contains(e.Error(), "exceeds 32") {
				hasLenErr = true
			}
		}
		if !hasLenErr {
			t.Fatalf("expected length error, got %v", errs)
		}
	})
}

func TestCheckDesired(t *testing.T) {
	t.Run("valid desired state", func(t *testing.T) {
		desired := map[Username]DesiredState{
			"myapp": {Files: map[string]string{
				"myapp.container": "[Container]\nImage=docker.io/library/nginx\n",
			}},
		}
		errs := CheckDesired(desired)
		if len(errs) != 0 {
			t.Fatalf("expected no errors, got %v", errs)
		}
	})

	t.Run("broken container fails", func(t *testing.T) {
		desired := map[Username]DesiredState{
			"myapp": {Files: map[string]string{
				"myapp.container": "[Service]\nRestart=always\n",
			}},
		}
		errs := CheckDesired(desired)
		if len(errs) != 1 {
			t.Fatalf("expected 1 error, got %v", errs)
		}
	})

	t.Run("companion files not validated as containers", func(t *testing.T) {
		desired := map[Username]DesiredState{
			"myapp": {Files: map[string]string{
				"myapp.container":   "[Container]\nImage=docker.io/library/nginx\n",
				"myapp-data.volume": "[Volume]\nDriver=local\n",
			}},
		}
		errs := CheckDesired(desired)
		if len(errs) != 0 {
			t.Fatalf("expected no errors, got %v", errs)
		}
	})

	t.Run("pod desired state validates pod and containers", func(t *testing.T) {
		desired := map[Username]DesiredState{
			"webapp": {Files: map[string]string{
				"webapp.pod":           "[Pod]\nPodmanArgs=--dns=1.1.1.1\n",
				"webapp-web.container": "[Container]\nImage=nginx\nPod=webapp.pod\n",
				"webapp-api.container": "[Container]\nImage=api\nPod=webapp.pod\n",
			}},
		}
		errs := CheckDesired(desired)
		if len(errs) != 0 {
			t.Fatalf("expected no errors, got %v", errs)
		}
	})

	t.Run("pod missing Pod section fails", func(t *testing.T) {
		desired := map[Username]DesiredState{
			"webapp": {Files: map[string]string{
				"webapp.pod": "[Service]\nRestart=always\n",
			}},
		}
		errs := CheckDesired(desired)
		if len(errs) != 1 {
			t.Fatalf("expected 1 error, got %v", errs)
		}
	})
}

func TestCheckDirWithPods(t *testing.T) {
	t.Run("orphan container in pod dir errors", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "mydir")
		os.Mkdir(sub, 0755)
		os.WriteFile(filepath.Join(sub, "webapp.pod"), []byte("[Pod]\n"), 0644)
		os.WriteFile(filepath.Join(sub, "webapp-web.container"), []byte("[Container]\nImage=nginx\n"), 0644)
		os.WriteFile(filepath.Join(sub, "orphan.container"), []byte("[Container]\nImage=nginx\n"), 0644)

		errs := CheckDir(dir)
		hasOrphan := false
		for _, e := range errs {
			if strings.Contains(e.Error(), "does not belong to any pod") {
				hasOrphan = true
			}
		}
		if !hasOrphan {
			t.Fatalf("expected orphan error, got %v", errs)
		}
	})

	t.Run("pod members validated", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "mydir")
		os.Mkdir(sub, 0755)
		os.WriteFile(filepath.Join(sub, "webapp.pod"), []byte("[Pod]\n"), 0644)
		os.WriteFile(filepath.Join(sub, "webapp-web.container"), []byte("[Container]\nImage=nginx\n"), 0644)
		os.WriteFile(filepath.Join(sub, "webapp-api.container"), []byte("[Container]\nImage=api\n"), 0644)

		errs := CheckDir(dir)
		if len(errs) != 0 {
			t.Fatalf("expected no errors, got %v", errs)
		}
	})
}

func TestDiscoverContainers(t *testing.T) {
	t.Run("root files", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "app.container"), []byte("[Container]\nImage=x\n"), 0644)
		os.WriteFile(filepath.Join(dir, "web.container"), []byte("[Container]\nImage=x\n"), 0644)

		root, rootPods, subdirs, err := discoverContainers(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(root) != 2 {
			t.Fatalf("expected 2 root files, got %d", len(root))
		}
		if len(rootPods) != 0 {
			t.Fatalf("expected 0 root pods, got %d", len(rootPods))
		}
		if len(subdirs) != 0 {
			t.Fatalf("expected 0 subdirs, got %d", len(subdirs))
		}
	})

	t.Run("root pods", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "webapp.pod"), []byte("[Pod]\n"), 0644)
		os.WriteFile(filepath.Join(dir, "webapp-web.container"), []byte("[Container]\nImage=x\n"), 0644)

		root, rootPods, _, err := discoverContainers(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(root) != 1 {
			t.Fatalf("expected 1 root container, got %d", len(root))
		}
		if len(rootPods) != 1 {
			t.Fatalf("expected 1 root pod, got %d", len(rootPods))
		}
	})

	t.Run("subdirectory files", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "staging")
		os.Mkdir(sub, 0755)
		os.WriteFile(filepath.Join(sub, "svc.container"), []byte("[Container]\nImage=x\n"), 0644)

		root, _, subdirs, err := discoverContainers(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(root) != 0 {
			t.Fatalf("expected 0 root files, got %d", len(root))
		}
		if len(subdirs) != 1 {
			t.Fatalf("expected 1 subdir, got %d", len(subdirs))
		}
		specs := subdirs["staging"]
		if len(specs.Containers) != 1 {
			t.Fatalf("expected 1 container in staging, got %d", len(specs.Containers))
		}
	})

	t.Run("subdirectory with pods", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "tailscale")
		os.Mkdir(sub, 0755)
		os.WriteFile(filepath.Join(sub, "webapp.pod"), []byte("[Pod]\n"), 0644)
		os.WriteFile(filepath.Join(sub, "webapp-web.container"), []byte("[Container]\nImage=x\n"), 0644)

		_, _, subdirs, err := discoverContainers(dir)
		if err != nil {
			t.Fatal(err)
		}
		specs := subdirs["tailscale"]
		if len(specs.Containers) != 1 {
			t.Fatalf("expected 1 container, got %d", len(specs.Containers))
		}
		if len(specs.Pods) != 1 {
			t.Fatalf("expected 1 pod, got %d", len(specs.Pods))
		}
	})

	t.Run("dot directories skipped", func(t *testing.T) {
		dir := t.TempDir()
		dot := filepath.Join(dir, ".git")
		os.Mkdir(dot, 0755)
		os.WriteFile(filepath.Join(dot, "config.container"), []byte("[Container]\nImage=x\n"), 0644)

		root, _, subdirs, err := discoverContainers(dir)
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

		root, _, _, err := discoverContainers(dir)
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

		root, _, subdirs, err := discoverContainers(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(root) != 0 {
			t.Fatalf("expected 0 root files, got %d", len(root))
		}
		// "a" subdir is found but "b" inside it is not — only one level
		if specs, ok := subdirs["a"]; ok {
			if len(specs.Containers) != 0 {
				t.Fatalf("expected 0 files in subdir a, got %d", len(specs.Containers))
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

		root, _, subdirs, err := discoverContainers(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(root) != 1 {
			t.Fatalf("expected 1 root file, got %d", len(root))
		}
		specs := subdirs["prod"]
		if len(specs.Containers) != 2 {
			t.Fatalf("expected 2 files in prod, got %d", len(specs.Containers))
		}
		// Verify filenames
		var names []string
		for _, f := range specs.Containers {
			names = append(names, filepath.Base(f))
		}
		sort.Strings(names)
		if names[0] != "api.container" || names[1] != "web.container" {
			t.Fatalf("unexpected files: %v", names)
		}
	})
}
