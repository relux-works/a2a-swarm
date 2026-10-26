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
			return errors.New("probe requires macOS")
		}
		executable, err := os.Executable()
		if err != nil {
			return errors.New("probe unavailable")
		}
		path, err := resolvedProbePath(executable)
		if err != nil {
			return errors.New("probe unavailable")
		}
		cmd = exec.CommandContext(ctx, path, args...)
	}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	var out, stderr limitedSecretBuffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := boundSecurityProcess(cmd); err != nil {
		return errors.New("probe unavailable")
	}
	err := runBoundSecurityProcess(cmd)
	// Native locked evidence requires the exact pair and completed pipes.
	var exit *exec.ExitError
	if ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 3 && !errors.Is(err, exec.ErrWaitDelay) && !errors.Is(err, errSecurityProcessIO) && !errors.Is(err, errSecurityProcessCleanup) && !out.overflow && !stderr.overflow && stderr.Len() == 0 && out.String() == "locked\n" {
		return fixedSecretFailure("keychain status unavailable or locked", "probe", "native-locked", nil)
	}
	// Exact token/status pair only. Helper text never escapes into CLI errors.
	if ctx.Err() != nil || err != nil || out.overflow || stderr.overflow || stderr.Len() != 0 || out.String() != "unlocked\n" {
		return observedSecretFailure("keychain status unavailable or locked", "probe", err, ctx.Err())
	}
	return nil
}
