# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

quadsync is a Go CLI tool that deploys Podman Quadlet containers from a Git repository onto a Linux host. It creates per-container Linux users, applies INI-based transforms, and manages rootless systemd services. Designed for Fedora CoreOS / systemd-based environments.

## Build & Test Commands

```bash
go build .                          # Build
go test .                           # Run all tests
go test -v .                        # Verbose test output
go test -run TestMerge .            # Run tests matching pattern
go vet ./...                        # Lint
```

Release build (static ARM64 binary):
```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags='-s -w' -o quadsync-linux-arm64 .
```

## Architecture

All code is in the `main` package (flat structure, no sub-packages). Zero external dependencies — pure standard library.

**Files and responsibilities:**

- `main.go` — CLI entry point with three subcommands: `sync`, `check`, `augment`
- `reconcile.go` — Orchestrates the full sync workflow: git clone/fetch → load transforms → build desired state → deploy/cleanup. Also holds `Config` and env file parsing.
- `merge.go` — INI transform merging. Two merge rules: `Key=Value` sets defaults (spec takes precedence), `+Key=Value` prepends before spec values.
- `ini.go` — INI parser/serializer that preserves comments, blank lines, and ordering for round-trip fidelity.
- `system.go` — Shell-out wrappers for git, useradd/userdel, loginctl, and systemctl operations. Uses `systemctl --user -M <user>@` for per-user service management.
- `check.go` — Validates `.container` files: filename must be a valid Linux username (`[a-z][a-z0-9-]*`, max 32), must have `[Container]` section with `Image=`.

**Sync workflow (reconcile.go:Sync):**
1. Git clone or fetch+reset the configured repo
2. Load transform files from `/etc/quadsync/transforms/`
3. Build desired state: root-level `.container` files used as-is; subdirectory `.container` files get merged with the matching transform; sibling `.service`/`.timer` files are attached as sidecars by filename prefix
4. For each container: create user if needed → skip if content hash unchanged → prune stale sidecars → write files → daemon-reload → enable timers → restart service
5. Clean up removed containers: disable timers → stop service → remove quadlets and user units → delete user

**File layout in the per-container user's home:**
- `~/.config/containers/systemd/` — Podman quadlet files (`.container`, `.pod`, `.volume`, etc.)
- `~/.config/systemd/user/` — plain systemd unit files (`.service`, `.timer`) — used for sidecar timers since Podman's quadlet generator doesn't process those extensions

**Sidecar timers/services:** A `.service` or `.timer` file in the repo named `<container-stem>-<label>.{service,timer}` is associated with `<container-stem>.container` and deployed under the same user. Longest-prefix match wins (so `webapp-web-refresh.timer` attaches to the `webapp-web` pod member, not the `webapp` pod). Sidecars are deployed verbatim — no transforms, no `{{.Name}}` substitution.

**Configuration** is read from `/etc/quadsync/config.env`:
- `QUADSYNC_GIT_URL` (required), `QUADSYNC_GIT_BRANCH` (default: "main"), `QUADSYNC_TRANSFORM_DIR`, `QUADSYNC_STATE_DIR`, `QUADSYNC_USER_GROUP`

**Container naming convention:** The `.container` filename (minus extension) becomes the Linux username and systemd service name. This is why `check` validates filenames as valid usernames.

## Testing

Tests exist for the pure-logic modules only (`ini_test.go`, `merge_test.go`). System interactions in `system.go` and `reconcile.go` are untested as they require root and real systemd. When adding new logic, follow the existing pattern of testing pure functions separately from system calls.
