package main

import (
	"embed"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strconv"
)

//go:embed dashboard.html
var dashboardFS embed.FS

// cmdWebUI runs the unprivileged HTTP frontend. It holds no host privilege of
// its own — every privileged action goes to `quadsync serve` over the control
// socket, which re-validates each request. Designed to run in a rootless
// tailnet container whose user is a member of the socket's group.
func cmdWebUI() {
	fs := flag.NewFlagSet("webui", flag.ExitOnError)
	addr := fs.String("addr", ":8765", "HTTP listen address")
	socketPath := fs.String("socket", defaultControlSocket, "control socket path")
	_ = fs.Parse(os.Args[2:])

	srv := &webServer{socket: *socketPath}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.handleIndex)
	mux.HandleFunc("GET /api/containers", srv.handleList)
	mux.HandleFunc("GET /api/containers/{name}", srv.handleGet)
	mux.HandleFunc("GET /api/containers/{name}/logs", srv.handleLogs)
	mux.HandleFunc("POST /api/containers/{name}/{action}", srv.handleAction)
	mux.HandleFunc("POST /api/sync", srv.handleSync)

	log.Printf("webui listening on %s (socket %s)", *addr, *socketPath)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("webui: %v", err)
	}
}

type webServer struct{ socket string }

// call forwards a control request to the daemon and writes the response as JSON.
func (s *webServer) call(w http.ResponseWriter, req Request) {
	resp, err := callSocket(s.socket, req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, Response{OK: false, Error: err.Error()})
		return
	}
	code := http.StatusOK
	if !resp.OK {
		code = http.StatusBadRequest
	}
	writeJSON(w, code, resp)
}

func (s *webServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := dashboardFS.ReadFile("dashboard.html")
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (s *webServer) handleList(w http.ResponseWriter, r *http.Request) {
	s.call(w, Request{Op: OpList})
}

func (s *webServer) handleGet(w http.ResponseWriter, r *http.Request) {
	s.call(w, Request{Op: OpGet, Name: r.PathValue("name")})
}

func (s *webServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	s.call(w, Request{Op: OpLogs, Name: r.PathValue("name"), Lines: lines})
}

func (s *webServer) handleAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	switch action {
	case OpRestart, OpStop, OpStart, OpRedeploy, OpRepull:
		s.call(w, Request{Op: action, Name: r.PathValue("name")})
	default:
		writeJSON(w, http.StatusBadRequest, Response{OK: false, Error: "unknown action: " + action})
	}
}

func (s *webServer) handleSync(w http.ResponseWriter, r *http.Request) {
	s.call(w, Request{Op: OpSync})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
