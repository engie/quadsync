package main

import (
	"net"
	"path/filepath"
	"testing"
)

// startTestDaemon spins up the real accept/handleConn loop on a temp socket
// with the given config, and returns its path. No root required.
func startTestDaemon(t *testing.T, cfg Config) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "ctl.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go handleConn(cfg, conn)
		}
	}()
	return sock
}

func TestDaemonOpListEmptyGroup(t *testing.T) {
	// A group with no members (here, one that does not exist) yields an empty
	// but successful list — the frontend renders "no managed containers".
	sock := startTestDaemon(t, Config{UserGroup: "quadsync-nonexistent-group"})

	resp, err := callSocket(sock, Request{Op: OpList})
	if err != nil {
		t.Fatalf("callSocket: %v", err)
	}
	if !resp.OK {
		t.Fatalf("list not ok: %q", resp.Error)
	}
	if len(resp.Containers) != 0 {
		t.Errorf("expected no containers, got %d", len(resp.Containers))
	}
}

func TestDaemonRejectsUnmanagedAndUnknownOp(t *testing.T) {
	sock := startTestDaemon(t, Config{UserGroup: "quadsync-nonexistent-group"})

	// Name-scoped op against a name that isn't managed must be refused before
	// any privileged action is attempted.
	resp, err := callSocket(sock, Request{Op: OpRestart, Name: "nginx-demo"})
	if err != nil {
		t.Fatalf("callSocket: %v", err)
	}
	if resp.OK || resp.Error == "" {
		t.Errorf("expected rejection for unmanaged name, got %+v", resp)
	}

	// Unknown ops are reported, not executed.
	resp, err = callSocket(sock, Request{Op: "destroy-everything"})
	if err != nil {
		t.Fatalf("callSocket: %v", err)
	}
	if resp.OK || resp.Error == "" {
		t.Errorf("expected rejection for unknown op, got %+v", resp)
	}
}
