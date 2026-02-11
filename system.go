package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Timeout classes for external commands.
const (
	shortTimeout   = 30 * time.Second  // id, getent, rev-parse
	defaultTimeout = 60 * time.Second  // useradd, userdel, loginctl, chown, git reset
	gitNetTimeout  = 2 * time.Minute   // git clone, git fetch (network-bound)
	systemdTimeout = 90 * time.Second  // systemctl --user operations (container stop can be slow)
)

// run executes a command with a timeout and returns combined output.
func run(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("command timed out after %s: %s %s", timeout, name, strings.Join(args, " "))
	}
	if err != nil {
		return string(out), fmt.Errorf("running %s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// gitClone clones a repo.
func gitClone(url, dest, branch string) error {
	_, err := run(gitNetTimeout, "git", "clone", "--branch", branch, "--single-branch", "--depth=1", url, dest)
	return err
}

// gitFetch fetches and returns whether there are new changes.
func gitFetch(repoDir, branch string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitNetTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "fetch", "origin", branch)
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return false, fmt.Errorf("git fetch timed out after %s", gitNetTimeout)
		}
		return false, fmt.Errorf("git fetch: %w\n%s", err, out)
	}

	// Compare HEAD with FETCH_HEAD
	ctx2, cancel2 := context.WithTimeout(context.Background(), shortTimeout)
	defer cancel2()
	cmd = exec.CommandContext(ctx2, "git", "rev-parse", "HEAD")
	cmd.Dir = repoDir
	headOut, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git rev-parse HEAD: %w", err)
	}

	cmd = exec.CommandContext(ctx2, "git", "rev-parse", "FETCH_HEAD")
	cmd.Dir = repoDir
	fetchOut, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git rev-parse FETCH_HEAD: %w", err)
	}

	return strings.TrimSpace(string(headOut)) != strings.TrimSpace(string(fetchOut)), nil
}

// gitResetHard resets repo to origin/branch.
func gitResetHard(repoDir, branch string) error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "reset", "--hard", "origin/"+branch)
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("git reset timed out after %s", defaultTimeout)
		}
		return fmt.Errorf("git reset: %w\n%s", err, out)
	}
	return nil
}

// createUser creates a user in the given group. Uses a regular (non-system)
// user so that useradd auto-allocates subuid/subgid ranges for rootless Podman.
func createUser(name, group string) error {
	_, err := run(defaultTimeout, "useradd", "--create-home", "-s", "/sbin/nologin", "-G", group, name)
	if err != nil {
		return fmt.Errorf("creating user %s: %w", name, err)
	}
	_, err = run(defaultTimeout, "loginctl", "enable-linger", name)
	if err != nil {
		return fmt.Errorf("enabling linger for %s: %w", name, err)
	}
	return nil
}

// waitForUserManager ensures a user's systemd instance is ready.
// Explicitly starts user@<uid>.service (no-op if already running) and
// verifies the D-Bus socket exists before returning.
func waitForUserManager(name string) error {
	uidStr, err := run(shortTimeout, "id", "-u", name)
	if err != nil {
		return fmt.Errorf("looking up uid for %s: %w", name, err)
	}
	uid := strings.TrimSpace(uidStr)
	if _, err := run(systemdTimeout, "systemctl", "start", "user@"+uid+".service"); err != nil {
		return fmt.Errorf("starting user manager for %s: %w", name, err)
	}
	busSocket := fmt.Sprintf("/run/user/%s/bus", uid)
	if _, err := os.Stat(busSocket); err != nil {
		return fmt.Errorf("user bus socket missing for %s after manager start: %s", name, busSocket)
	}
	return nil
}

// deleteUser stops services and removes a user.
func deleteUser(name string) error {
	// Disable linger so logind won't restart the user manager.
	if _, err := run(defaultTimeout, "loginctl", "disable-linger", name); err != nil {
		log.Printf("warning: disable-linger %s: %v", name, err)
	}
	// Terminate all sessions, stop the user manager, clean up /run/user/<uid>.
	// This is synchronous — it waits for the user runtime to be fully torn down.
	if _, err := run(systemdTimeout, "loginctl", "terminate-user", name); err != nil {
		log.Printf("warning: terminate-user %s: %v", name, err)
	}
	// Remove user and home, retrying on transient "busy" from kernel-level
	// cleanup that outlasts the logind teardown.
	return userdelRetry(name)
}

// userdelRetry runs userdel -r with bounded retries for transient errors
// caused by teardown races (processes still exiting, directory still busy).
func userdelRetry(name string) error {
	const maxAttempts = 4
	for i := 0; i < maxAttempts; i++ {
		out, err := run(defaultTimeout, "userdel", "-r", name)
		if err == nil {
			return nil
		}
		if i == maxAttempts-1 || !userdellTransient(out) {
			return err
		}
		log.Printf("userdel %s: transient error, retrying (%d/%d): %s",
			name, i+1, maxAttempts-1, strings.TrimSpace(out))
		time.Sleep(2 * time.Second)
	}
	return nil // unreachable
}

// userdellTransient returns true if userdel output indicates a transient
// teardown race that may resolve on retry.
func userdellTransient(output string) bool {
	return strings.Contains(output, "busy") ||
		strings.Contains(output, "currently used by process")
}

// writeQuadlet writes a .container file to the user's quadlet directory.
func writeQuadlet(username, containerName, content string) error {
	home, err := userHome(username)
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "containers", "systemd")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating quadlet dir: %w", err)
	}
	path := filepath.Join(dir, containerName+".container")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return err
	}
	// chown the entire .config tree to the user — Podman refuses to run
	// if any parent directory is not owned by the container user.
	if _, err := run(defaultTimeout, "chown", "-R", username+":"+username, filepath.Join(home, ".config")); err != nil {
		return fmt.Errorf("chowning .config for %s: %w", username, err)
	}
	return nil
}

// removeQuadlet removes a .container file from the user's quadlet directory.
func removeQuadlet(username, containerName string) error {
	home, err := userHome(username)
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".config", "containers", "systemd", containerName+".container")
	return os.Remove(path)
}

// runUserM runs "systemctl --user -M <user>@" with inherited stdout/stderr.
// Output goes to the journal rather than being captured, because the machinectl
// transport (-M) fails when Go pipes stdout/stderr via CombinedOutput().
func runUserM(username string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), systemdTimeout)
	defer cancel()
	cmdArgs := append([]string{"--user", "-M", username + "@"}, args...)
	cmd := exec.CommandContext(ctx, "systemctl", cmdArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("systemctl --user -M %s@ %s: timed out after %s",
				username, strings.Join(args, " "), systemdTimeout)
		}
		return fmt.Errorf("systemctl --user -M %s@ %s: %w",
			username, strings.Join(args, " "), err)
	}
	return nil
}

// daemonReload runs systemctl --user daemon-reload for a user.
func daemonReload(username string) error {
	return runUserM(username, "daemon-reload")
}

// restartService restarts a user service.
func restartService(username, serviceName string) error {
	return runUserM(username, "restart", serviceName+".service")
}

// stopService stops a user service.
func stopService(username, serviceName string) error {
	return runUserM(username, "stop", serviceName+".service")
}

// managedUsers returns the list of users in the given group.
func managedUsers(group string) ([]string, error) {
	out, err := run(shortTimeout, "getent", "group", group)
	if err != nil {
		// Group might not exist yet or have no members
		if strings.Contains(err.Error(), "exit status 2") {
			return nil, nil
		}
		return nil, err
	}
	parts := strings.Split(strings.TrimSpace(out), ":")
	if len(parts) < 4 || parts[3] == "" {
		return nil, nil
	}
	return strings.Split(parts[3], ","), nil
}

func userHome(username string) (string, error) {
	out, err := exec.Command("getent", "passwd", username).Output()
	if err != nil {
		return "", fmt.Errorf("getent passwd %s: %w", username, err)
	}
	fields := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(fields) < 6 || fields[5] == "" {
		return "", fmt.Errorf("getent passwd %s: missing home directory field", username)
	}
	return fields[5], nil
}
