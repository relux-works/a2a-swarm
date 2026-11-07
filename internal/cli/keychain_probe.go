package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
)

const keychainProbeName = "a2a-keychain-status"

func (k *keychainStore) targetPath() (string, error) {
	if k.fixture != "" {
		return k.fixture, nil
	}
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return "", errors.New("home unavailable")
	}
	return filepath.Join(u.HomeDir, "Library", "Keychains", "login.keychain-db"), nil
}

// Resolve the actual a2a executable, including launcher symlinks, never PATH or
// a user-controlled helper environment variable. A missing companion fails shut.
func resolvedProbePath(executable string) (string, error) {
	real, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(real), keychainProbeName), nil
}

func (k *keychainStore) hostPath() (string, error) {
	host := k.hostID // tests only; no config/host initialization or credential reads
	if host == "" {
		var err error
		host, err = keychainHostBinding()
		if err != nil || host == "" {
			return "", errors.New("host unavailable")
		}
	}
	target, err := k.targetPath()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte("keychain-host\x00" + host + "\x00" + target))
	return filepath.Join(k.paths.Base, "keychain-state", "host-"+hex.EncodeToString(sum[:])), nil
}

type keychainPreflightError struct {
	cause string
	local bool // No companion ran: this image's installation/launch is unavailable.
}

func (e *keychainPreflightError) Error() string { return "keychain preflight refused: " + e.cause }

func (k *keychainStore) preflight(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, k.timeoutDuration())
	defer cancel()
	var cmd *exec.Cmd
	args := []string{}
	if k.fixture != "" {
		args = []string{"--fixture", k.fixture}
	}
	if k.probeCommand != nil {
		cmd = k.probeCommand(ctx, args...)
	} else {
		if runtime.GOOS != "darwin" {
			return &keychainPreflightError{cause: "companion_unavailable", local: true}
		}
		executable, err := os.Executable()
		if err != nil {
			return &keychainPreflightError{cause: "companion_unavailable", local: true}
		}
		path, err := resolvedProbePath(executable)
		if err != nil {
			return &keychainPreflightError{cause: "companion_unavailable", local: true}
		}
		cmd = exec.CommandContext(ctx, path, args...)
	}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	var out, stderr limitedSecretBuffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := boundSecurityProcess(cmd); err != nil {
		return &keychainPreflightError{cause: "companion_unavailable", local: true}
	}
	err := runBoundSecurityProcess(cmd)
	// A process that never started cannot establish a Keychain failure. This
	// covers missing/nonexecutable companions without relying on error text.
	if cmd.Process == nil {
		return &keychainPreflightError{cause: "companion_unavailable", local: true}
	}
	cause := ""
	switch {
	case ctx.Err() != nil:
		cause = "preflight_timeout"
	case errors.Is(err, exec.ErrWaitDelay), errors.Is(err, errSecurityProcessIO), errors.Is(err, errSecurityProcessCleanup):
		cause = "preflight_io"
	case !out.overflow && !stderr.overflow && stderr.Len() == 0 && out.String() == "locked\n" && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 3:
		cause = "preflight_locked"
	case !out.overflow && !stderr.overflow && stderr.Len() == 0 && out.String() == "error\n" && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 2:
		cause = "preflight_error"
	case err != nil || out.overflow || stderr.overflow || stderr.Len() != 0 || out.String() != "unlocked\n":
		cause = "preflight_refused"
	}
	if cause != "" {
		return &keychainPreflightError{cause: cause}
	}
	return nil
}
