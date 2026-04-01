# quadsync

A CLI tool that deploys [Podman Quadlet](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html) containers from a Git repository onto a Linux host. It creates per-container Linux users, applies INI-based transforms, and manages rootless systemd services.

Designed for Fedora CoreOS and other systemd-based environments.

## How it works

quadsync syncs `.container` files from a Git repo and deploys each one as a rootless Podman service under its own Linux user (the filename minus `.container` becomes the username and service name).

**Sync workflow:**

1. Clone or fetch the configured Git repository
2. Validate raw `.container` files (filename, `[Container]` section, `Image=`)
3. Load transform files from the transform directory
4. Build desired state — root-level `.container` files are used as-is; files in subdirectories get merged with matching transforms
5. Validate merged output (catches transforms that break a valid spec, e.g. removing `Image=`)
6. For each container: create the Linux user if needed, skip if the content hash is unchanged, write the quadlet file, daemon-reload, and restart the service
7. Clean up removed containers: stop the service, remove the quadlet, delete the user

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
QUADSYNC_GIT_URL=https://github.com/you/your-containers.git
QUADSYNC_GIT_BRANCH=main
QUADSYNC_AGE_KEY=/etc/quadsync/keys/age.txt
QUADSYNC_TRANSFORM_DIR=/etc/quadsync/transforms
QUADSYNC_STATE_DIR=/var/lib/quadsync
QUADSYNC_USER_GROUP=cusers
```

Only `QUADSYNC_GIT_URL` is required. `QUADSYNC_AGE_KEY` is optional and only needed if your repo contains SOPS-encrypted `.container` files.

## Usage

```
quadsync sync              Full reconcile (git-sync, merge, deploy)
quadsync check <dir>       Validate .container files
quadsync augment <file>    Print merged result to stdout
quadsync edit <file>       Edit a .container file, decrypting and re-encrypting if needed
quadsync redeploy <name>   Force redeployment on next sync
```

**sync** — performs the full reconciliation loop. Intended to run as a systemd timer or CI trigger.

**check** — validates `.container` files in a directory. Checks that filenames are valid Linux usernames (`[a-z][a-z0-9-]*`, max 32 chars) and that each file has a `[Container]` section with `Image=`. Useful as a CI pre-merge check. Note: `sync` also runs these checks on both the raw inputs and the merged output, so invalid specs are caught before deployment even if `check` isn't run separately.

**augment** — previews the result of merging a `.container` file with its matching transform, printing the merged output to stdout.

**edit** — opens a `.container` file in `$EDITOR` using a temporary scratch file. For plaintext files without a `[Secrets]` section, quadsync pre-populates the scratch buffer with an empty `[Secrets]` section and a comment showing the supported formats. After the editor exits, quadsync inspects the edited result: files with a `[Secrets]` section are written back with only the `[Secrets]` values encrypted, and files without one are written back plaintext. Already-encrypted files stay encrypted, but non-secret fields remain readable.

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

## Secrets

quadsync can read SOPS-encrypted `.container` files, decrypt them with an age private key, create Podman secrets for the target service user, and inject the matching `Secret=` directives into the generated quadlet.

Generate an age keypair with `age-keygen` and use the public recipient for SOPS encryption:

```bash
mkdir -p /etc/quadsync/keys
age-keygen -o /etc/quadsync/keys/age.txt
chmod 600 /etc/quadsync/keys/age.txt
grep '^# public key:' /etc/quadsync/keys/age.txt
```

Configure the age private key path in `/etc/quadsync/config.env`:

```env
QUADSYNC_AGE_KEY=/etc/quadsync/keys/age.txt
```

The private key stays in `QUADSYNC_AGE_KEY`. The `# public key: age1...` line is the recipient you use in `sops` or `.sops.yaml`.

Then define secrets in a container spec with a `[Secrets]` section:

```ini
[Container]
Image=registry.example.com/planning-webapp:latest
ContainerName=planning-webapp

[Secrets]
DATABASE_URL=env:postgres://admin:s3cret@db.internal:5432/planning
API_KEY=env:sk-live-abc123xyz
TLS_CERT=file:/run/secrets/tls.cert:aGVsbG8gd29ybGQ=
```

Secret value formats:

- `env:<value>` injects a Podman secret as an environment variable with the same name as the key
- `file:<target>:<base64-value>` mounts a Podman secret at `<target>`; the file payload must be base64-encoded in the spec

Encrypt the file with SOPS using your age recipient:

```bash
sops --encrypt --in-place \
  --age age1yourrecipienthere \
  --input-type ini \
  --output-type ini \
  app.container
```

If you use SOPS regularly, add a `.sops.yaml` to the container repo so you do not have to pass `--age` each time:

```yaml
creation_rules:
  - path_regex: \.container$
    age: age1yourrecipienthere
```

Then you can encrypt in place with:

```bash
sops -e -i app.container
```

During `quadsync sync`:

1. The `.container` file is decrypted if it contains a `[sops]` section.
2. Entries from `[Secrets]` are converted into Podman secrets named `<container>-<secret-name>` in lowercase with underscores changed to dashes.
3. `[Secrets]` and `[sops]` are stripped from the deployed quadlet.
4. `Secret=` lines are added to the `[Container]` section before the service is restarted.

If a file is SOPS-encrypted and `QUADSYNC_AGE_KEY` is not configured, sync fails for that deployment.

For `quadsync edit`, `QUADSYNC_AGE_KEY` is required if the edited result is going to be encrypted: that includes files that already start encrypted and files where you add a `[Secrets]` section during editing. When `edit` is going to write encrypted output, it also refuses to run if a `.sops.yaml` or `.sops.yml` is present anywhere above the target file, because its native re-encryption path does not try to preserve repo-specific SOPS policy.

## Requirements

- Linux with systemd
- Podman (with Quadlet support)
- Git
- Root access (for user creation and systemd management)
