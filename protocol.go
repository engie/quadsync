package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// The control protocol is newline-delimited JSON (NDJSON) over a Unix socket:
// one Request object per line in, one Response object per line back. It is
// spoken by the privileged daemon (`quadsync serve`) and the unprivileged web
// frontend (`quadsync webui`). Kept deliberately tiny — a fixed set of ops, no
// free-form commands — so the root side has very little surface to get wrong.

// Op constants name the verbs the control socket accepts.
const (
	OpList     = "list"     // snapshot of every managed container
	OpGet      = "get"      // snapshot of one container
	OpLogs     = "logs"     // recent journal lines for one container
	OpRestart  = "restart"  // systemctl --user restart
	OpStop     = "stop"     // systemctl --user stop
	OpStart    = "start"    // systemctl --user start
	OpRedeploy = "redeploy" // clear deploy hash + re-sync
	OpRepull   = "repull"   // force a fresh image pull + recreate
	OpSync     = "sync"     // run a full quadsync sync
)

// Request is a single NDJSON control request.
type Request struct {
	Op    string `json:"op"`
	Name  string `json:"name,omitempty"`  // container/user name for name-scoped ops
	Lines int    `json:"lines,omitempty"` // log line count for OpLogs
}

// ContainerInfo is the status/build/health snapshot for one managed container.
type ContainerInfo struct {
	Name        string `json:"name"`
	ActiveState string `json:"active_state,omitempty"` // systemd ActiveState (active/failed/inactive/unknown)
	SubState    string `json:"sub_state,omitempty"`    // systemd SubState (running/dead/...)
	MainPID     string `json:"main_pid,omitempty"`
	ActiveEnter string `json:"active_enter,omitempty"` // ActiveEnterTimestamp
	Image       string `json:"image,omitempty"`        // image reference (tag)
	ImageID     string `json:"image_id,omitempty"`     // resolved image digest/ID
	Health      string `json:"health,omitempty"`       // healthy/unhealthy/starting/none
	Hash        string `json:"hash,omitempty"`         // quadsync deploy hash ("build")
}

// Response is a single NDJSON control response.
type Response struct {
	OK         bool            `json:"ok"`
	Error      string          `json:"error,omitempty"`
	Containers []ContainerInfo `json:"containers,omitempty"` // OpList
	Container  *ContainerInfo  `json:"container,omitempty"`  // OpGet
	Logs       string          `json:"logs,omitempty"`       // OpLogs
	Message    string          `json:"message,omitempty"`    // action ops
}

// socketCallTimeout bounds a single request/response round trip. Generous,
// because OpSync and OpRepull shell out to git and podman.
const socketCallTimeout = 3 * time.Minute

// callSocket sends one request over the control socket and reads one response.
// Used by the web frontend; one-shot per call (dial, write a line, read a line).
func callSocket(socketPath string, req Request) (Response, error) {
	conn, err := net.DialTimeout("unix", socketPath, 10*time.Second)
	if err != nil {
		return Response{}, fmt.Errorf("dialing %s: %w", socketPath, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(socketCallTimeout))

	line, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return Response{}, fmt.Errorf("writing request: %w", err)
	}

	r := bufio.NewReaderSize(conn, 1<<20)
	respLine, err := r.ReadBytes('\n')
	if err != nil && len(respLine) == 0 {
		return Response{}, fmt.Errorf("reading response: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(respLine, &resp); err != nil {
		return Response{}, fmt.Errorf("decoding response: %w", err)
	}
	return resp, nil
}
