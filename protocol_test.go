package main

import (
	"encoding/json"
	"testing"
)

func TestRequestRoundTrip(t *testing.T) {
	cases := []Request{
		{Op: OpList},
		{Op: OpGet, Name: "nginx-demo"},
		{Op: OpLogs, Name: "nginx-demo", Lines: 200},
		{Op: OpRepull, Name: "web-app"},
		{Op: OpSync},
	}
	for _, want := range cases {
		b, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal %+v: %v", want, err)
		}
		var got Request
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", b, err)
		}
		if got != want {
			t.Errorf("round trip: got %+v, want %+v", got, want)
		}
	}
}

func TestResponseRoundTrip(t *testing.T) {
	want := Response{
		OK: true,
		Containers: []ContainerInfo{
			{Name: "nginx-demo", ActiveState: "active", SubState: "running",
				Image: "docker.io/library/nginx:latest", ImageID: "sha256:abc",
				Health: "healthy", Hash: "deadbeef"},
		},
	}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Response
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.OK || len(got.Containers) != 1 || got.Containers[0] != want.Containers[0] {
		t.Errorf("round trip mismatch: got %+v", got)
	}
}

// omitempty keeps the wire format compact: an empty Request must not carry
// name/lines, and a bare OK Response must not carry empty collections.
func TestOmitEmpty(t *testing.T) {
	b, _ := json.Marshal(Request{Op: OpSync})
	if got := string(b); got != `{"op":"sync"}` {
		t.Errorf("request omitempty: got %s", got)
	}
	b, _ = json.Marshal(Response{OK: true})
	if got := string(b); got != `{"ok":true}` {
		t.Errorf("response omitempty: got %s", got)
	}
}

func TestValidateManaged(t *testing.T) {
	managed := []Username{"nginx-demo", "web-app"}

	if u, err := validateManaged("nginx-demo", managed); err != nil || u != "nginx-demo" {
		t.Errorf("managed name should validate: u=%q err=%v", u, err)
	}

	if _, err := validateManaged("not-managed", managed); err == nil {
		t.Error("unmanaged name should be rejected")
	}

	// Invalid username never validates, even against a non-empty set.
	if _, err := validateManaged("Bad Name", managed); err == nil {
		t.Error("invalid username should be rejected")
	}

	// Empty managed set rejects everything.
	if _, err := validateManaged("nginx-demo", nil); err == nil {
		t.Error("empty managed set should reject all names")
	}
}
