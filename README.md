# quadsync

A CLI tool that deploys [Podman Quadlet](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html) containers from a Git repository onto a Linux host. It creates per-container Linux users, applies INI-based transforms, and manages rootless systemd services.

Designed for Fedora CoreOS and other systemd-based environments.

## How it works

quadsync syncs `.container` files from a Git repo and deploys each one as a rootless Podman service under its own Linux user (the filename minus `.container` becomes the username and service name).

**Sync workflow:**

1. Clone or fetch the configured Git repository
2. Load transform files from the transform directory
3. Build desired state — root-level `.container` files are used as-is; files in subdirectories get merged with matching transforms
4. For each container: create the Linux user if needed, skip if the content hash is unchanged, write the quadlet file, daemon-reload, and restart the service
5. Clean up removed containers: stop the service, remove the quadlet, delete the user

**Transforms** let you inject host-specific configuration (network settings, volume mounts, etc.) into container specs from subdirectories. Two merge rules:

- `Key=Value` — sets a default (the spec takes precedence if it already defines the key)
- `+Key=Value` — prepends a value before the spec's values (for multi-value keys like `Volume=`)

## Installation

Download a binary from the [releases page](https://github.com/engie/quadsync/releases), or build from source:

```bash
go build .
```

Static cross-compiled binary:

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags='-s -w' -o quadsync-linux-arm64 .
```

## Configuration

quadsync reads `/etc/quadsync/config.env`:

```env
QDEPLOY_GIT_URL=https://github.com/you/your-containers.git
QDEPLOY_GIT_BRANCH=main
QDEPLOY_TRANSFORM_DIR=/etc/quadsync/transforms
QDEPLOY_STATE_DIR=/var/lib/quadsync
QDEPLOY_USER_GROUP=cusers
```

Only `QDEPLOY_GIT_URL` is required; the rest have defaults shown above.

## Usage

```
quadsync sync              Full reconcile (git-sync, merge, deploy)
quadsync check <dir>       Validate .container files
quadsync augment <file>    Print merged result to stdout
quadsync redeploy <name>   Force redeployment on next sync
```

**sync** — performs the full reconciliation loop. Intended to run as a systemd timer or CI trigger.

**check** — validates `.container` files in a directory. Checks that filenames are valid Linux usernames (`[a-z][a-z0-9-]*`, max 32 chars) and that each file has a `[Container]` section with `Image=`. Useful as a CI pre-merge check.

**augment** — previews the result of merging a `.container` file with its matching transform, printing the merged output to stdout.

**redeploy** — clears the stored content hash for a service so that the next `sync` rewrites its quadlet and restarts it, even if the spec hasn't changed. Useful after manual changes to transforms, host config, or to recover a service whose quadlet was deleted.

```bash
quadsync redeploy myapp          # mark for redeployment
quadsync sync                    # apply immediately (or wait for the timer)
```

## Container repo layout

```
repo/
  standalone.container          # deployed as-is (user: standalone)
  webapps/
    myapp.container             # merged with transforms/webapps.container
    otherapp.container           # merged with transforms/webapps.container
```

Transform files live on the host at the transform directory (default `/etc/quadsync/transforms/`):

```
/etc/quadsync/transforms/
  webapps.container             # applied to all files in repo/webapps/
```

## Requirements

- Linux with systemd
- Podman (with Quadlet support)
- Git
- Root access (for user creation and systemd management)
