package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompanionNameSubstitution(t *testing.T) {
	companions := []CompanionTemplate{
		{
			SuffixAndExt: "-data.volume",
			Content:      "[Volume]\nLabel=app={{.Name}}\n",
		},
		{
			SuffixAndExt: "-litestream.container",
			Content:      "[Container]\nImage=litestream\nEnvironment=DB_NAME={{.Name}}\n",
		},
	}

	state := buildDesiredState("myapp", "[Container]\nImage=myapp\n", companions)

	if len(state.Files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(state.Files))
	}

	// Main container file
	if _, ok := state.Files["myapp.container"]; !ok {
		t.Error("missing myapp.container")
	}

	// Volume companion
	vol, ok := state.Files["myapp-data.volume"]
	if !ok {
		t.Fatal("missing myapp-data.volume")
	}
	if !strings.Contains(vol, "Label=app=myapp") {
		t.Errorf("{{.Name}} not replaced in volume content: %s", vol)
	}

	// Litestream companion
	ls, ok := state.Files["myapp-litestream.container"]
	if !ok {
		t.Fatal("missing myapp-litestream.container")
	}
	if !strings.Contains(ls, "DB_NAME=myapp") {
		t.Errorf("{{.Name}} not replaced in litestream content: %s", ls)
	}
}

func TestCompositeHash(t *testing.T) {
	state1 := DesiredState{Files: map[string]string{
		"app.container":  "[Container]\nImage=app\n",
		"app-data.volume": "[Volume]\n",
	}}
	state2 := DesiredState{Files: map[string]string{
		"app.container":  "[Container]\nImage=app\n",
		"app-data.volume": "[Volume]\n",
	}}
	state3 := DesiredState{Files: map[string]string{
		"app.container":  "[Container]\nImage=app:v2\n",
		"app-data.volume": "[Volume]\n",
	}}

	h1 := compositeHash(state1)
	h2 := compositeHash(state2)
	h3 := compositeHash(state3)

	if h1 != h2 {
		t.Error("identical states should produce same hash")
	}
	if h1 == h3 {
		t.Error("different states should produce different hash")
	}
	if len(h1) != 64 {
		t.Errorf("expected 64-char hex hash, got %d chars", len(h1))
	}
}

func TestLoadTransformsWithCompanions(t *testing.T) {
	dir := t.TempDir()

	// Base transform
	os.WriteFile(filepath.Join(dir, "_base.container"), []byte("[Unit]\nAfter=network.target\n"), 0644)

	// Companion templates
	os.WriteFile(filepath.Join(dir, "_base-data.volume"), []byte("[Volume]\nLabel=app={{.Name}}\n"), 0644)
	os.WriteFile(filepath.Join(dir, "_base-sidecar.container"), []byte("[Container]\nImage=sidecar\nPodmanArgs={{.Name}}\n"), 0644)

	// Directory transform
	os.WriteFile(filepath.Join(dir, "myhost.container"), []byte("[Container]\nNetwork=host\n"), 0644)

	base, transforms, companions, err := loadTransforms(dir)
	if err != nil {
		t.Fatalf("loadTransforms: %v", err)
	}

	if base == nil {
		t.Fatal("expected base transform, got nil")
	}

	if len(transforms) != 1 {
		t.Fatalf("expected 1 directory transform, got %d", len(transforms))
	}
	if _, ok := transforms["myhost"]; !ok {
		t.Error("missing 'myhost' transform")
	}

	if len(companions) != 2 {
		t.Fatalf("expected 2 companions, got %d", len(companions))
	}

	// Check companion suffixes (order may vary due to readdir)
	suffixes := map[string]bool{}
	for _, c := range companions {
		suffixes[c.SuffixAndExt] = true
	}
	if !suffixes["-data.volume"] {
		t.Error("missing companion with suffix -data.volume")
	}
	if !suffixes["-sidecar.container"] {
		t.Error("missing companion with suffix -sidecar.container")
	}
}

func TestLoadTransformsRejectsUnexpectedFiles(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "random.txt"), []byte("oops"), 0644)

	_, _, _, err := loadTransforms(dir)
	if err == nil {
		t.Fatal("expected error for unexpected file, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected file") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestBuildDesiredWithCompanions(t *testing.T) {
	dir := t.TempDir()

	// Root-level container
	os.WriteFile(filepath.Join(dir, "webapp.container"), []byte("[Container]\nImage=webapp\n"), 0644)

	companions := []CompanionTemplate{
		{SuffixAndExt: "-data.volume", Content: "[Volume]\nLabel={{.Name}}\n"},
	}

	desired, err := buildDesired(dir, nil, nil, companions)
	if err != nil {
		t.Fatalf("buildDesired: %v", err)
	}

	state, ok := desired["webapp"]
	if !ok {
		t.Fatal("missing 'webapp' in desired")
	}

	if len(state.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(state.Files))
	}
	if _, ok := state.Files["webapp.container"]; !ok {
		t.Error("missing webapp.container")
	}
	vol, ok := state.Files["webapp-data.volume"]
	if !ok {
		t.Fatal("missing webapp-data.volume")
	}
	if !strings.Contains(vol, "Label=webapp") {
		t.Errorf("{{.Name}} not replaced: %s", vol)
	}
}
