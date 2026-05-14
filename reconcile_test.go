package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseEnvFile(t *testing.T) {
	tests := []struct {
		name string
		data string
		want map[string]string
	}{
		{
			name: "plain values",
			data: "FOO=bar\nBAZ=qux",
			want: map[string]string{"FOO": "bar", "BAZ": "qux"},
		},
		{
			name: "double quoted",
			data: `KEY="hello world"`,
			want: map[string]string{"KEY": "hello world"},
		},
		{
			name: "single quoted",
			data: `KEY='hello world'`,
			want: map[string]string{"KEY": "hello world"},
		},
		{
			name: "quotes with surrounding whitespace",
			data: `KEY = "hello world" `,
			want: map[string]string{"KEY": "hello world"},
		},
		{
			name: "mismatched quotes left alone",
			data: `KEY="hello'`,
			want: map[string]string{"KEY": `"hello'`},
		},
		{
			name: "comments and blanks",
			data: "# comment\n\nFOO=bar\n  # another\nBAZ=qux",
			want: map[string]string{"FOO": "bar", "BAZ": "qux"},
		},
		{
			name: "empty value",
			data: "KEY=",
			want: map[string]string{"KEY": ""},
		},
		{
			name: "quoted empty value",
			data: `KEY=""`,
			want: map[string]string{"KEY": ""},
		},
		{
			name: "value with equals sign",
			data: "KEY=a=b=c",
			want: map[string]string{"KEY": "a=b=c"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseEnvFile(tt.data)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildDesiredDuplicateStem(t *testing.T) {
	dir := t.TempDir()

	// Root-level foo.container
	rootSpec := "[Container]\nImage=docker.io/root/foo\n"
	if err := os.WriteFile(filepath.Join(dir, "foo.container"), []byte(rootSpec), 0644); err != nil {
		t.Fatal(err)
	}

	// Subdirectory with same stem
	subDir := filepath.Join(dir, "myhost")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	subSpec := "[Container]\nImage=docker.io/sub/foo\n"
	if err := os.WriteFile(filepath.Join(subDir, "foo.container"), []byte(subSpec), 0644); err != nil {
		t.Fatal(err)
	}

	// Provide a transform for "myhost" so the subdir is processed
	transform, _ := ParseINI(strings.NewReader("[Container]\n"))
	transforms := map[string]*INIFile{"myhost": transform}

	_, err := buildDesired(dir, nil, transforms, nil)
	if err == nil {
		t.Fatal("expected error for duplicate stem, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate container name") {
		t.Fatalf("expected duplicate error, got: %v", err)
	}
}

func TestBuildDesiredNoDuplicate(t *testing.T) {
	dir := t.TempDir()

	// Root-level foo.container
	rootSpec := "[Container]\nImage=docker.io/root/foo\n"
	if err := os.WriteFile(filepath.Join(dir, "foo.container"), []byte(rootSpec), 0644); err != nil {
		t.Fatal(err)
	}

	// Subdirectory with different stem
	subDir := filepath.Join(dir, "myhost")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	subSpec := "[Container]\nImage=docker.io/sub/bar\n"
	if err := os.WriteFile(filepath.Join(subDir, "bar.container"), []byte(subSpec), 0644); err != nil {
		t.Fatal(err)
	}

	transform, _ := ParseINI(strings.NewReader("[Container]\n"))
	transforms := map[string]*INIFile{"myhost": transform}

	desired, err := buildDesired(dir, nil, transforms, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(desired) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(desired))
	}
	if _, ok := desired["foo"]; !ok {
		t.Error("missing 'foo' in desired state")
	}
	if _, ok := desired["bar"]; !ok {
		t.Error("missing 'bar' in desired state")
	}
}

func TestLoadAllTransformsWithPods(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "_base.container"), []byte("[Unit]\nAfter=network.target\n"), 0644)
	os.WriteFile(filepath.Join(dir, "_base.pod"), []byte("[Pod]\n"), 0644)
	os.WriteFile(filepath.Join(dir, "_base-data.volume"), []byte("[Volume]\nLabel={{.Name}}\n"), 0644)
	os.WriteFile(filepath.Join(dir, "myhost.container"), []byte("[Container]\nNetwork=host\n"), 0644)
	os.WriteFile(filepath.Join(dir, "myhost.pod"), []byte("[Pod]\nPodmanArgs=--dns=1.1.1.1\n"), 0644)

	tr, err := loadAllTransforms(dir)
	if err != nil {
		t.Fatalf("loadAllTransforms: %v", err)
	}

	if tr.Base == nil {
		t.Fatal("expected base container transform")
	}
	if tr.BasePod == nil {
		t.Fatal("expected base pod transform")
	}
	if len(tr.DirContainer) != 1 {
		t.Fatalf("expected 1 dir container transform, got %d", len(tr.DirContainer))
	}
	if _, ok := tr.DirContainer["myhost"]; !ok {
		t.Error("missing 'myhost' container transform")
	}
	if len(tr.DirPod) != 1 {
		t.Fatalf("expected 1 dir pod transform, got %d", len(tr.DirPod))
	}
	if _, ok := tr.DirPod["myhost"]; !ok {
		t.Error("missing 'myhost' pod transform")
	}
	if len(tr.Companions) != 1 {
		t.Fatalf("expected 1 companion, got %d", len(tr.Companions))
	}
}

func TestBuildDesiredWithPodDir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "tailscale")
	os.MkdirAll(sub, 0755)

	os.WriteFile(filepath.Join(sub, "webapp.pod"), []byte("[Pod]\nPodmanArgs=--dns=1.1.1.1\n"), 0644)
	os.WriteFile(filepath.Join(sub, "webapp-web.container"), []byte("[Container]\nImage=nginx\n"), 0644)
	os.WriteFile(filepath.Join(sub, "webapp-api.container"), []byte("[Container]\nImage=api-server\n"), 0644)

	tr := Transforms{
		DirContainer: map[string]*INIFile{},
		DirPod:       map[string]*INIFile{},
	}

	desired, err := buildDesiredFull(dir, tr)
	if err != nil {
		t.Fatalf("buildDesiredFull: %v", err)
	}

	state, ok := desired["webapp"]
	if !ok {
		t.Fatal("missing 'webapp' in desired")
	}

	// Should have pod file + 2 member containers
	if _, ok := state.Files["webapp.pod"]; !ok {
		t.Error("missing webapp.pod")
	}
	if _, ok := state.Files["webapp-web.container"]; !ok {
		t.Error("missing webapp-web.container")
	}
	if _, ok := state.Files["webapp-api.container"]; !ok {
		t.Error("missing webapp-api.container")
	}

	// Pod= should be injected into members
	webContent := state.Files["webapp-web.container"]
	if !strings.Contains(webContent, "Pod=webapp.pod") {
		t.Errorf("Pod= not injected into webapp-web.container:\n%s", webContent)
	}
	apiContent := state.Files["webapp-api.container"]
	if !strings.Contains(apiContent, "Pod=webapp.pod") {
		t.Errorf("Pod= not injected into webapp-api.container:\n%s", apiContent)
	}

	// ServiceName should be pod service
	if state.ServiceName != "webapp-pod" {
		t.Errorf("expected ServiceName=webapp-pod, got %s", state.ServiceName)
	}
}

func TestBuildDesiredPodWithTransforms(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "tailscale")
	os.MkdirAll(sub, 0755)

	os.WriteFile(filepath.Join(sub, "webapp.pod"), []byte("[Pod]\n"), 0644)
	os.WriteFile(filepath.Join(sub, "webapp-web.container"), []byte("[Container]\nImage=nginx\n"), 0644)

	basePod, _ := ParseINI(strings.NewReader("[Pod]\nPodmanArgs=--dns=1.1.1.1\n"))
	dirPod, _ := ParseINI(strings.NewReader("[Pod]\nNetwork=slirp4netns\n"))
	base, _ := ParseINI(strings.NewReader("[Service]\nRestart=on-failure\n"))
	dirContainer, _ := ParseINI(strings.NewReader("[Container]\nPodmanArgs=--pidfile %t/%N.pid\n"))

	tr := Transforms{
		Base:         base,
		BasePod:      basePod,
		DirContainer: map[string]*INIFile{"tailscale": dirContainer},
		DirPod:       map[string]*INIFile{"tailscale": dirPod},
	}

	desired, err := buildDesiredFull(dir, tr)
	if err != nil {
		t.Fatalf("buildDesiredFull: %v", err)
	}

	state := desired["webapp"]

	// Pod should have both base and dir transforms applied
	podContent := state.Files["webapp.pod"]
	if !strings.Contains(podContent, "PodmanArgs=--dns=1.1.1.1") {
		t.Errorf("base pod transform not applied:\n%s", podContent)
	}
	if !strings.Contains(podContent, "Network=slirp4netns") {
		t.Errorf("dir pod transform not applied:\n%s", podContent)
	}

	// Container should have both base and dir transforms applied
	webContent := state.Files["webapp-web.container"]
	if !strings.Contains(webContent, "Restart=on-failure") {
		t.Errorf("base container transform not applied:\n%s", webContent)
	}
	if !strings.Contains(webContent, "PodmanArgs=--pidfile %t/%N.pid") {
		t.Errorf("dir container transform not applied:\n%s", webContent)
	}
}

func TestBuildDesiredPodNameSubstitution(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "webapp.pod"), []byte("[Pod]\nPodmanArgs=--name={{.Name}}\n"), 0644)
	os.WriteFile(filepath.Join(dir, "webapp-web.container"), []byte("[Container]\nImage=nginx\nVolume={{.Name}}-data:/data\n"), 0644)

	tr := Transforms{
		DirContainer: map[string]*INIFile{},
		DirPod:       map[string]*INIFile{},
	}

	desired, err := buildDesiredFull(dir, tr)
	if err != nil {
		t.Fatalf("buildDesiredFull: %v", err)
	}

	state := desired["webapp"]

	// {{.Name}} in pod file → pod stem
	podContent := state.Files["webapp.pod"]
	if strings.Contains(podContent, "{{.Name}}") {
		t.Errorf("{{.Name}} not replaced in pod:\n%s", podContent)
	}
	if !strings.Contains(podContent, "PodmanArgs=--name=webapp") {
		t.Errorf("expected pod name substitution, got:\n%s", podContent)
	}

	// {{.Name}} in member container → member full name
	webContent := state.Files["webapp-web.container"]
	if strings.Contains(webContent, "{{.Name}}") {
		t.Errorf("{{.Name}} not replaced in member:\n%s", webContent)
	}
	if !strings.Contains(webContent, "Volume=webapp-web-data:/data") {
		t.Errorf("expected member name substitution, got:\n%s", webContent)
	}
}

func TestBuildDesiredPodWithCompanions(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "tailscale")
	os.MkdirAll(sub, 0755)

	os.WriteFile(filepath.Join(sub, "webapp.pod"), []byte("[Pod]\n"), 0644)
	os.WriteFile(filepath.Join(sub, "webapp-web.container"), []byte("[Container]\nImage=nginx\n"), 0644)

	tr := Transforms{
		DirContainer: map[string]*INIFile{},
		DirPod:       map[string]*INIFile{},
		Companions: []CompanionTemplate{
			{SuffixAndExt: "-data.volume", Content: "[Volume]\nLabel={{.Name}}\n"},
			{SuffixAndExt: "-sidecar.container", Content: "[Container]\nImage=sidecar\nEnvironment=APP={{.Name}}\n"},
		},
	}

	desired, err := buildDesiredFull(dir, tr)
	if err != nil {
		t.Fatalf("buildDesiredFull: %v", err)
	}

	state := desired["webapp"]

	// Per-member companions
	if _, ok := state.Files["webapp-web-data.volume"]; !ok {
		t.Error("missing webapp-web-data.volume companion")
	}
	sidecar, ok := state.Files["webapp-web-sidecar.container"]
	if !ok {
		t.Fatal("missing webapp-web-sidecar.container companion")
	}
	// Companion .container should get Pod= injected
	if !strings.Contains(sidecar, "Pod=webapp.pod") {
		t.Errorf("Pod= not injected into companion container:\n%s", sidecar)
	}
	if !strings.Contains(sidecar, "APP=webapp-web") {
		t.Errorf("{{.Name}} not replaced in companion:\n%s", sidecar)
	}
}

func TestBuildDesiredMultiplePodsInDir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "pods")
	os.MkdirAll(sub, 0755)

	os.WriteFile(filepath.Join(sub, "webapp.pod"), []byte("[Pod]\n"), 0644)
	os.WriteFile(filepath.Join(sub, "webapp-web.container"), []byte("[Container]\nImage=nginx\n"), 0644)
	os.WriteFile(filepath.Join(sub, "feedranker.pod"), []byte("[Pod]\n"), 0644)
	os.WriteFile(filepath.Join(sub, "feedranker-worker.container"), []byte("[Container]\nImage=worker\n"), 0644)

	tr := Transforms{
		DirContainer: map[string]*INIFile{},
		DirPod:       map[string]*INIFile{},
	}

	desired, err := buildDesiredFull(dir, tr)
	if err != nil {
		t.Fatalf("buildDesiredFull: %v", err)
	}

	if len(desired) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(desired))
	}
	if _, ok := desired["webapp"]; !ok {
		t.Error("missing 'webapp'")
	}
	if _, ok := desired["feedranker"]; !ok {
		t.Error("missing 'feedranker'")
	}
}

func TestBuildDesiredPodStandaloneCollision(t *testing.T) {
	dir := t.TempDir()

	// Standalone at root
	os.WriteFile(filepath.Join(dir, "webapp.container"), []byte("[Container]\nImage=nginx\n"), 0644)

	// Pod in subdir with same name
	sub := filepath.Join(dir, "pods")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "webapp.pod"), []byte("[Pod]\n"), 0644)
	os.WriteFile(filepath.Join(sub, "webapp-web.container"), []byte("[Container]\nImage=nginx\n"), 0644)

	tr := Transforms{
		DirContainer: map[string]*INIFile{},
		DirPod:       map[string]*INIFile{},
	}

	_, err := buildDesiredFull(dir, tr)
	if err == nil {
		t.Fatal("expected collision error, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate error, got: %v", err)
	}
}

func TestBuildDesiredRootPod(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "webapp.pod"), []byte("[Pod]\n"), 0644)
	os.WriteFile(filepath.Join(dir, "webapp-web.container"), []byte("[Container]\nImage=nginx\n"), 0644)
	os.WriteFile(filepath.Join(dir, "standalone.container"), []byte("[Container]\nImage=redis\n"), 0644)

	tr := Transforms{
		DirContainer: map[string]*INIFile{},
		DirPod:       map[string]*INIFile{},
	}

	desired, err := buildDesiredFull(dir, tr)
	if err != nil {
		t.Fatalf("buildDesiredFull: %v", err)
	}

	if len(desired) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(desired))
	}

	// Pod entry
	state, ok := desired["webapp"]
	if !ok {
		t.Fatal("missing 'webapp'")
	}
	if _, ok := state.Files["webapp.pod"]; !ok {
		t.Error("missing webapp.pod")
	}
	if _, ok := state.Files["webapp-web.container"]; !ok {
		t.Error("missing webapp-web.container")
	}
	if state.ServiceName != "webapp-pod" {
		t.Errorf("expected ServiceName=webapp-pod, got %s", state.ServiceName)
	}

	// Standalone entry
	standalone, ok := desired["standalone"]
	if !ok {
		t.Fatal("missing 'standalone'")
	}
	if standalone.ServiceName != "standalone" {
		t.Errorf("expected ServiceName=standalone, got %s", standalone.ServiceName)
	}
}

func TestBuildDesiredWithSidecars(t *testing.T) {
	t.Run("standalone with sidecars", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "library.container"),
			[]byte("[Container]\nImage=lib\n"), 0644)
		os.WriteFile(filepath.Join(dir, "library-refresh.service"),
			[]byte("[Service]\nExecStart=/bin/true\n"), 0644)
		os.WriteFile(filepath.Join(dir, "library-refresh.timer"),
			[]byte("[Timer]\nOnCalendar=03:00\n"), 0644)

		desired, err := buildDesired(dir, nil, nil, nil)
		if err != nil {
			t.Fatalf("buildDesired: %v", err)
		}
		state, ok := desired["library"]
		if !ok {
			t.Fatal("missing 'library' in desired")
		}
		if _, ok := state.Files["library-refresh.service"]; !ok {
			t.Errorf("missing library-refresh.service in DesiredState; have %v", keysOf(state.Files))
		}
		if _, ok := state.Files["library-refresh.timer"]; !ok {
			t.Errorf("missing library-refresh.timer in DesiredState; have %v", keysOf(state.Files))
		}
	})

	t.Run("pod member sidecar attaches to pod state", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "tailscale")
		os.Mkdir(sub, 0755)
		os.WriteFile(filepath.Join(sub, "webapp.pod"), []byte("[Pod]\n"), 0644)
		os.WriteFile(filepath.Join(sub, "webapp-web.container"),
			[]byte("[Container]\nImage=nginx\n"), 0644)
		os.WriteFile(filepath.Join(sub, "webapp-web-refresh.timer"),
			[]byte("[Timer]\nOnCalendar=03:00\n"), 0644)
		os.WriteFile(filepath.Join(sub, "webapp-web-refresh.service"),
			[]byte("[Service]\nExecStart=/bin/true\n"), 0644)

		tr := Transforms{
			DirContainer: map[string]*INIFile{},
			DirPod:       map[string]*INIFile{},
		}
		desired, err := buildDesiredFull(dir, tr)
		if err != nil {
			t.Fatalf("buildDesiredFull: %v", err)
		}
		state, ok := desired["webapp"]
		if !ok {
			t.Fatal("missing 'webapp' in desired")
		}
		if _, ok := state.Files["webapp-web-refresh.service"]; !ok {
			t.Errorf("expected service in webapp pod state; have %v", keysOf(state.Files))
		}
		if _, ok := state.Files["webapp-web-refresh.timer"]; !ok {
			t.Errorf("expected timer in webapp pod state; have %v", keysOf(state.Files))
		}
	})

	t.Run("orphan sidecar fails build", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "other.container"),
			[]byte("[Container]\nImage=x\n"), 0644)
		os.WriteFile(filepath.Join(dir, "library-refresh.timer"),
			[]byte("[Timer]\nOnCalendar=03:00\n"), 0644)

		_, err := buildDesired(dir, nil, nil, nil)
		if err == nil {
			t.Fatal("expected build error for orphan sidecar")
		}
		if !strings.Contains(err.Error(), "has no matching") {
			t.Fatalf("expected orphan error, got: %v", err)
		}
	})

	t.Run("sidecar content changes the hash", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "library.container"),
			[]byte("[Container]\nImage=lib\n"), 0644)
		os.WriteFile(filepath.Join(dir, "library-refresh.timer"),
			[]byte("[Timer]\nOnCalendar=03:00\n"), 0644)
		os.WriteFile(filepath.Join(dir, "library-refresh.service"),
			[]byte("[Service]\nExecStart=/bin/true\n"), 0644)

		d1, err := buildDesired(dir, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		h1 := compositeHash(d1["library"])

		os.WriteFile(filepath.Join(dir, "library-refresh.timer"),
			[]byte("[Timer]\nOnCalendar=04:00\n"), 0644)
		d2, err := buildDesired(dir, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		h2 := compositeHash(d2["library"])

		if h1 == h2 {
			t.Fatal("expected hash change after sidecar edit")
		}
	})
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestUserFileDir(t *testing.T) {
	cases := []struct {
		filename string
		want     string
	}{
		{"library.container", quadletDir},
		{"webapp.pod", quadletDir},
		{"library-data.volume", quadletDir},
		{"library-refresh.service", userUnitDir},
		{"library-refresh.timer", userUnitDir},
	}
	for _, c := range cases {
		got := userFileDir(c.filename)
		if got != c.want {
			t.Errorf("userFileDir(%q) = %q, want %q", c.filename, got, c.want)
		}
	}
}

func TestBuildDesiredSubdirNoPodNoTransformAllowed(t *testing.T) {
	// Subdir with pods but no dir container transform — should work (members get only base)
	dir := t.TempDir()
	sub := filepath.Join(dir, "mydir")
	os.MkdirAll(sub, 0755)

	os.WriteFile(filepath.Join(sub, "webapp.pod"), []byte("[Pod]\n"), 0644)
	os.WriteFile(filepath.Join(sub, "webapp-web.container"), []byte("[Container]\nImage=nginx\n"), 0644)

	base, _ := ParseINI(strings.NewReader("[Service]\nRestart=on-failure\n"))
	tr := Transforms{
		Base:         base,
		DirContainer: map[string]*INIFile{},
		DirPod:       map[string]*INIFile{},
	}

	desired, err := buildDesiredFull(dir, tr)
	if err != nil {
		t.Fatalf("buildDesiredFull: %v", err)
	}

	state := desired["webapp"]
	webContent := state.Files["webapp-web.container"]
	if !strings.Contains(webContent, "Restart=on-failure") {
		t.Errorf("base transform not applied:\n%s", webContent)
	}
}
