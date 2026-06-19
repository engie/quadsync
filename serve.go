package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// defaultControlSocket is where the daemon listens and the frontend connects.
const defaultControlSocket = "/run/quadsync/control.sock"

// cmdServe runs the privileged control-socket daemon. It must run as root: it
// drives per-user systemd managers, reads quadsync state, and re-syncs.
func cmdServe() {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	socketPath := fs.String("socket", defaultControlSocket, "unix socket path to listen on")
	_ = fs.Parse(os.Args[2:])

	cfg, err := LoadConfig(configPath)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if err := serve(cfg, *socketPath); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func serve(cfg Config, socketPath string) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0750); err != nil {
		return fmt.Errorf("creating socket dir: %w", err)
	}
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing stale socket: %w", err)
	}
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socketPath, err)
	}
	defer l.Close()

	// Access control is filesystem-based: root owns the socket, the managed-user
	// group (cusers) gets rw. The frontend container runs as a cusers member.
	if err := chownSocket(socketPath, cfg.UserGroup); err != nil {
		return err
	}

	log.Printf("listening on %s (group %s)", socketPath, cfg.UserGroup)
	for {
		conn, err := l.Accept()
		if err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		go handleConn(cfg, conn)
	}
}

// chownSocket sets the socket to root:<group> 0660 so only group members connect.
func chownSocket(path, group string) error {
	gid := -1
	if g, err := user.LookupGroup(group); err == nil {
		if n, err := strconv.Atoi(g.Gid); err == nil {
			gid = n
		}
	} else {
		log.Printf("warning: group %q not found, leaving socket group unchanged: %v", group, err)
	}
	if gid >= 0 {
		if err := os.Chown(path, 0, gid); err != nil {
			return fmt.Errorf("chown socket: %w", err)
		}
	}
	if err := os.Chmod(path, 0660); err != nil {
		return fmt.Errorf("chmod socket: %w", err)
	}
	return nil
}

// handleConn reads NDJSON requests off a connection until EOF, responding to each.
func handleConn(cfg Config, conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var req Request
		var resp Response
		if err := json.Unmarshal(line, &req); err != nil {
			resp = Response{OK: false, Error: fmt.Sprintf("bad request: %v", err)}
		} else {
			resp = dispatch(cfg, req)
		}
		out, _ := json.Marshal(resp)
		out = append(out, '\n')
		if _, err := conn.Write(out); err != nil {
			return
		}
	}
}

// dispatch maps one request to a handler. Every name-scoped op first validates
// the name against the managed-user set, so the frontend can never reach a
// service outside quadsync's control.
func dispatch(cfg Config, req Request) Response {
	switch req.Op {
	case OpList:
		return opList(cfg)
	case OpGet:
		name, err := requireManaged(cfg, req.Name)
		if err != nil {
			return errResp(err)
		}
		info := gatherInfo(cfg, name)
		return Response{OK: true, Container: &info}
	case OpLogs:
		name, err := requireManaged(cfg, req.Name)
		if err != nil {
			return errResp(err)
		}
		return opLogs(name, req.Lines)
	case OpRestart, OpStop, OpStart:
		name, err := requireManaged(cfg, req.Name)
		if err != nil {
			return errResp(err)
		}
		if err := runUserM(name, req.Op, string(name)+".service"); err != nil {
			return errResp(err)
		}
		return Response{OK: true, Message: fmt.Sprintf("%s %s", req.Op, name)}
	case OpRedeploy:
		name, err := requireManaged(cfg, req.Name)
		if err != nil {
			return errResp(err)
		}
		return opRedeploy(cfg, name)
	case OpRepull:
		name, err := requireManaged(cfg, req.Name)
		if err != nil {
			return errResp(err)
		}
		if err := doRepull(name); err != nil {
			return errResp(err)
		}
		return Response{OK: true, Message: "repulled " + string(name)}
	case OpSync:
		if err := Sync(cfg); err != nil {
			return errResp(err)
		}
		return Response{OK: true, Message: "sync complete"}
	default:
		return Response{OK: false, Error: "unknown op: " + req.Op}
	}
}

func errResp(err error) Response { return Response{OK: false, Error: err.Error()} }

// validateManaged checks that name is a valid username present in the managed
// set. Pure helper (no system calls) so it is unit-testable.
func validateManaged(name string, managed []Username) (Username, error) {
	u, err := NewUsername(name)
	if err != nil {
		return "", err
	}
	for _, m := range managed {
		if m == u {
			return u, nil
		}
	}
	return "", fmt.Errorf("%q is not a managed container", name)
}

// requireManaged resolves the live managed-user set and validates name against it.
func requireManaged(cfg Config, name string) (Username, error) {
	managed, err := managedUsers(cfg.UserGroup)
	if err != nil {
		return "", fmt.Errorf("listing managed users: %w", err)
	}
	return validateManaged(name, managed)
}

func opList(cfg Config) Response {
	users, err := managedUsers(cfg.UserGroup)
	if err != nil {
		return errResp(fmt.Errorf("listing managed users: %w", err))
	}
	infos := make([]ContainerInfo, 0, len(users))
	for _, u := range users {
		infos = append(infos, gatherInfo(cfg, u))
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return Response{OK: true, Containers: infos}
}

// gatherInfo collects systemd state, image/health, and the quadsync deploy hash
// for one container. Best-effort: missing pieces are left empty.
func gatherInfo(cfg Config, name Username) ContainerInfo {
	info := ContainerInfo{Name: string(name)}

	if props, err := userSystemctlShow(name); err == nil {
		info.ActiveState = props["ActiveState"]
		info.SubState = props["SubState"]
		info.MainPID = props["MainPID"]
		info.ActiveEnter = props["ActiveEnterTimestamp"]
	} else {
		info.ActiveState = "unknown"
	}

	img, id, health := podmanInspect(name)
	if img == "" {
		img = imageFromQuadlet(name)
	}
	if health == "" {
		health = "none"
	}
	info.Image = img
	info.ImageID = id
	info.Health = health

	if b, err := os.ReadFile(filepath.Join(cfg.StateDir, "hashes", string(name))); err == nil {
		info.Hash = strings.TrimSpace(string(b))
	}
	return info
}

// userSystemctlShow reads unit properties via `systemctl --user show`, run as
// the target user with XDG_RUNTIME_DIR set. We use runuser (not the -M
// transport) because -M's stdout cannot be captured by Go (see system.go).
func userSystemctlShow(name Username) (map[string]string, error) {
	cmd := fmt.Sprintf("export XDG_RUNTIME_DIR=/run/user/$(id -u); systemctl --user show %s.service -p ActiveState -p SubState -p MainPID -p ActiveEnterTimestamp",
		string(name))
	out, err := runAsUser(shortTimeout, name, cmd)
	if err != nil {
		return nil, err
	}
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "="); i > 0 {
			props[line[:i]] = strings.TrimSpace(line[i+1:])
		}
	}
	return props, nil
}

// podmanInspect resolves image reference, image ID, and health for the running
// container. Returns empty strings if the container does not currently exist.
func podmanInspect(name Username) (image, id, health string) {
	cmd := fmt.Sprintf(
		"cd ~ 2>/dev/null; podman inspect %s --format '{{.ImageName}}|{{.Image}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}'",
		string(name))
	out, err := runAsUser(shortTimeout, name, cmd)
	if err != nil {
		return "", "", ""
	}
	parts := strings.SplitN(strings.TrimSpace(out), "|", 3)
	if len(parts) == 3 {
		return parts[0], parts[1], parts[2]
	}
	return "", "", ""
}

// imageFromQuadlet reads Image= from the deployed quadlet as a fallback when the
// container is not currently running (so podman inspect yields nothing).
func imageFromQuadlet(name Username) string {
	home := homeDir(name)
	if home == "" {
		return ""
	}
	path := filepath.Join(home, quadletDir, string(name)+".container")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Image=") {
			return strings.TrimPrefix(line, "Image=")
		}
	}
	return ""
}

func homeDir(name Username) string {
	u, err := user.Lookup(string(name))
	if err != nil {
		return ""
	}
	return u.HomeDir
}

func opLogs(name Username, lines int) Response {
	if lines <= 0 || lines > 2000 {
		lines = 200
	}
	cmd := fmt.Sprintf(
		"export XDG_RUNTIME_DIR=/run/user/$(id -u); journalctl --user -u %s.service -n %d --no-pager 2>&1",
		string(name), lines)
	// journalctl may exit non-zero yet still produce useful output; runAsUser
	// returns the combined output either way, so surface it.
	out, _ := runAsUser(systemdTimeout, name, cmd)
	return Response{OK: true, Logs: out}
}

// opRedeploy clears the stored deploy hash (so the next sync treats the spec as
// new) and runs a sync immediately. Mirrors `quadsync redeploy` + `sync`.
func opRedeploy(cfg Config, name Username) Response {
	hashFile := filepath.Join(cfg.StateDir, "hashes", string(name))
	if err := os.Remove(hashFile); err != nil && !os.IsNotExist(err) {
		return errResp(fmt.Errorf("removing hash: %w", err))
	}
	if err := Sync(cfg); err != nil {
		return errResp(fmt.Errorf("sync after redeploy: %w", err))
	}
	return Response{OK: true, Message: "redeployed " + string(name)}
}

// doRepull forces a fresh image pull: resolve the image, stop the service,
// remove the container and image, then start (which re-pulls). Ports the logic
// of test-on-host.sh's repull helper into the daemon.
func doRepull(name Username) error {
	svc := string(name) + ".service"
	image := resolveImage(name)

	_ = runUserM(name, "stop", svc) // best-effort; may already be stopped
	_, _ = runAsUser(defaultTimeout, name, fmt.Sprintf("cd ~ 2>/dev/null; podman rm -f %s", string(name)))
	if image != "" {
		_, _ = runAsUser(defaultTimeout, name, fmt.Sprintf("cd ~ 2>/dev/null; podman rmi -f %s", shellQuote(image)))
	} else {
		_, _ = runAsUser(defaultTimeout, name, "cd ~ 2>/dev/null; podman image prune -af")
	}
	return runUserM(name, "start", svc)
}

func resolveImage(name Username) string {
	if img, _, _ := podmanInspect(name); img != "" {
		return img
	}
	return imageFromQuadlet(name)
}
