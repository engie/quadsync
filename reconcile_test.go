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

	_, err := buildDesired(dir, transforms)
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

	desired, err := buildDesired(dir, transforms)
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
