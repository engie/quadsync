# Secrets Review Findings (54175ab)

## High

- [ ] **#1 Shell injection via secret key names** — `system.go:249` passes unsanitized key-derived names into a `/bin/sh -c` command string. Keys like `; rm -rf /` survive `podmanSecretName` with shell metacharacters intact. Fix: validate key names against `^[A-Za-z_][A-Za-z0-9_]*$` in `parseSecrets`.

- [ ] **#2 Pod member secrets created under wrong name** — `reconcile.go:243` calls `createPodmanSecrets` with `state.ServiceName` (e.g., `webapp-pod`) but `transformContainerFile` injected `Secret=` directives using each member's filename stem. The quadlet references names that were never created; pod members with secrets can't start.

## Medium

- [ ] **#3 compositeHash doesn't include secrets** — `reconcile.go:721-733` hashes only `state.Files`. If a secret value rotates but key names/targets stay the same, the quadlet is identical, sync skips the service, and `createPodmanSecrets` is never called. Stale secret material stays deployed.

- [ ] **#4 Edit can't drop encryption after removing all secrets** — `edit.go:316` sets `shouldEncrypt := isEncrypted || editedHasSecrets`, so an originally-encrypted file stays on the re-encryption path even after the user deletes all secrets. `applySecretsOnlyEncryptionPolicy` then errors with "no secret entries." Fix: `shouldEncrypt := editedHasSecrets`.

## Low

- [ ] **#5 Temp file not cleaned on signal/crash** — `edit.go:302` relies on `defer cleanup()` which doesn't run on SIGKILL or goroutine panic. Plaintext secrets persist in `/dev/shm` or `/tmp`. Consider a signal handler for SIGINT/SIGTERM.

- [ ] **#6 Massive dependency surface from sops/v3** — `go.mod` goes from 0 to 130+ transitive deps (AWS, Azure, GCP, Vault SDKs) for age-only usage. Significant supply chain surface increase.

## Info

- [ ] **#7 No atomic write / backup on edit** — `edit.go:375` overwrites the original file directly. A failed or unexpected re-encryption loses the source of truth. Write to `.new` + rename, or keep a `.bak`.

- [ ] **#8 Process-wide os.Setenv for SOPS_AGE_KEY_FILE** — Safe in the current single-threaded CLI but a latent hazard if the code becomes library-reusable.
