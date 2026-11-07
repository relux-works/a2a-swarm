package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

const keychainService = "works.relux.a2a.relux.works"
const securityDeadline = 5 * time.Second
const securityCooldown = 5 * time.Minute

var errKeychainMissing = errors.New("keychain item missing")
var errKeychainDuplicate = errors.New("keychain item already exists")
var errSecurityProcessIO = errors.New("keychain process I/O incomplete")
var errSecurityProcessCleanup = errors.New("keychain process cleanup failed")

// All seams are private and set only by package tests. The production command
// path and default keychain selection cannot be overridden by environment/flags.
type keychainStore struct {
	service           string
	createOnly        bool
	paths             config.Paths
	command           func(context.Context, ...string) *exec.Cmd
	probeCommand      func(context.Context, ...string) *exec.Cmd
	hostID            string
	fixture           string
	timeout, cooldown time.Duration
	now               func() time.Time
}

func (k *keychainStore) selectedService() string {
	if k.service != "" {
		return k.service
	}
	return keychainService
}

func (k *keychainStore) clock() time.Time {
	if k.now != nil {
		return k.now()
	}
	return time.Now()
}
func (k *keychainStore) timeoutDuration() time.Duration {
	if k.timeout > 0 {
		return k.timeout
	}
	return securityDeadline
}
func (k *keychainStore) cooldownDuration() time.Duration {
	if k.cooldown > 0 {
		return k.cooldown
	}
	return securityCooldown
}

func (k *keychainStore) identityPath(uid string) string {
	sum := sha256.Sum256([]byte("keychain\x00" + k.selectedService() + "\x00" + uid))
	return filepath.Join(k.paths.Base, "keychain-state", hex.EncodeToString(sum[:]))
}

func (k *keychainStore) locked(uid string, fn func() error) error {
	base := k.identityPath(uid)
	dir := filepath.Dir(base)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("keychain state unavailable for %s", uid)
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		return fmt.Errorf("keychain state must be a private directory for %s", uid)
	}
	unlock, err := keychainLock(base+".lock", time.Now().Add(k.timeoutDuration()))
	if err != nil {
		return fmt.Errorf("keychain identity lock unavailable for %s", uid)
	}
	defer unlock()
	return fn()
}

func (k *keychainStore) checkCooldown(uid string) error {
	return k.checkMarker(k.identityPath(uid)+".cooldown", uid)
}

func (k *keychainStore) checkMarker(path, uid string) error {
	f, err := openKeychainState(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("keychain cooldown state unreadable for %s", uid)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil || len(data) > 64 {
		return fmt.Errorf("keychain cooldown state invalid for %s", uid)
	}
	until, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return fmt.Errorf("keychain cooldown state invalid for %s", uid)
	}
	if k.clock().UnixNano() < until {
		return &transientCredentialError{reason: "keychain_cooldown", message: fmt.Sprintf("keychain cooldown active for %s; cause=%s; retry after %s", uid, keychainMarkerReason(path, until), time.Unix(0, until).UTC().Format(time.RFC3339))}
	}
	return nil
}

func (k *keychainStore) recordCooldown(uid string, reason ...string) error {
	return k.recordMarker(k.identityPath(uid)+".cooldown", reason...)
}

// Keep the timestamp marker readable by older images. A private sidecar carries
// only a closed failure class; legacy/malformed diagnostic data projects unknown.
func keychainMarkerReason(path string, until int64) string {
	f, err := openKeychainState(path + ".reason")
	if err != nil {
		return "unknown"
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil || len(data) > 64 {
		return "unknown"
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 || fields[0] != strconv.FormatInt(until, 10) {
		return "unknown"
	}
	return safeKeychainFailure(fields[1])
}

func safeKeychainFailure(reason string) string {
	switch reason {
	case "preflight_locked", "preflight_error", "preflight_timeout", "preflight_io", "preflight_refused",
		"security_timeout", "security_interaction", "security_io":
		return reason
	default:
		return "unknown"
	}
}

func (k *keychainStore) recordMarker(path string, reason ...string) error {
	cause := "unknown"
	if len(reason) != 0 {
		cause = safeKeychainFailure(reason[0])
	}
	return writeKeychainCooldown(path, cause, k.clock().Add(k.cooldownDuration()))
}

func writeKeychainCooldown(path, cause string, deadline time.Time) error {
	until := strconv.FormatInt(deadline.UnixNano(), 10)
	if err := writeKeychainMarker(path+".reason", until+" "+safeKeychainFailure(cause)); err != nil {
		return err
	}
	return writeKeychainMarker(path, until)
}

func writeKeychainMarker(path, value string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".cooldown-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err == nil {
		_, err = fmt.Fprintln(f, value)
	}
	if err == nil {
		err = f.Sync()
	}
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// limitedSecretBuffer never forwards helper output. Stderr can contain echoed
// stdin, so only numeric statuses classify failures; no helper text escapes.
type limitedSecretBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *limitedSecretBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := 65536 - b.Len()
	if len(p) > left {
		b.overflow = true
		p = p[:left]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func (k *keychainStore) invoke(ctx context.Context, uid, input string, args ...string) (string, error) {
	// This applies to EVERY invocation, including write lookup/relookup/readback.
	if err := k.checkCooldown(uid); err != nil {
		return "", err
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("keychain operation interrupted for %s", uid)
	}
	host, err := k.hostPath()
	if err != nil {
		return "", fmt.Errorf("keychain host binding unavailable for %s", uid)
	}
	unlock, err := keychainLock(host+".lock", time.Now().Add(k.timeoutDuration()))
	if err != nil {
		return "", fmt.Errorf("keychain host lock unavailable for %s", uid)
	}
	defer unlock()
	if err := k.checkMarker(host+".cooldown", uid); err != nil {
		return "", err
	}
	if err := k.preflight(ctx); err != nil {
		var refusal *keychainPreflightError
		if !errors.As(err, &refusal) {
			return "", err
		}
		if refusal.local {
			// Installation/launch failures say nothing about this identity's Keychain.
			return "", &transientCredentialError{reason: "companion_unavailable", message: fmt.Sprintf("keychain preflight failed for %s; companion unavailable; no cooldown recorded", uid)}
		}
		// Persist host first: other identities must not probe a known locked host.
		until := k.clock().Add(k.cooldownDuration())
		herr := writeKeychainCooldown(host+".cooldown", refusal.cause, until)
		ierr := writeKeychainCooldown(k.identityPath(uid)+".cooldown", refusal.cause, until)
		if herr != nil || ierr != nil {
			return "", fmt.Errorf("keychain preflight failed for %s; cannot persist cooldown; repair private state", uid)
		}
		reason := "keychain_unavailable"
		if refusal.cause == "preflight_locked" {
			reason = "keychain_locked"
		}
		return "", &transientCredentialError{reason: reason, message: fmt.Sprintf("keychain preflight failed for %s; cooldown recorded; cause=%s; retry after %s", uid, refusal.cause, until.UTC().Format(time.RFC3339))}
	}
	ctx, cancel := context.WithTimeout(ctx, k.timeoutDuration())
	defer cancel()
	var cmd *exec.Cmd
	if k.command != nil {
		cmd = k.command(ctx, args...)
	} else {
		cmd = exec.CommandContext(ctx, "/usr/bin/security", args...)
	}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	cmd.Stdin = strings.NewReader(input)
	var out, errout limitedSecretBuffer
	cmd.Stdout, cmd.Stderr = &out, &errout
	if err := boundSecurityProcess(cmd); err != nil {
		return "", fmt.Errorf("keychain process unavailable for %s", uid)
	}
	err = runBoundSecurityProcess(cmd)
	if ctx.Err() != nil || interactionFailure(err) {
		cause := "security_interaction"
		switch {
		case ctx.Err() != nil:
			cause = "security_timeout"
		case errors.Is(err, exec.ErrWaitDelay), errors.Is(err, errSecurityProcessIO), errors.Is(err, errSecurityProcessCleanup):
			cause = "security_io"
		}
		until := k.clock().Add(k.cooldownDuration())
		if e := writeKeychainCooldown(k.identityPath(uid)+".cooldown", cause, until); e != nil {
			return "", fmt.Errorf("keychain interaction failed for %s; cannot persist cooldown; stop and repair private state", uid)
		}
		return "", &transientCredentialError{reason: "keychain_unavailable", message: fmt.Sprintf("keychain interaction failed or timed out for %s; cooldown recorded; cause=%s; retry after %s", uid, cause, until.UTC().Format(time.RFC3339))}
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// errSecItemNotFound=-25300; errSecDuplicateItem=-25299. security
			// preserves their low byte as the process exit status.
			switch ee.ExitCode() {
			case 44:
				return "", errKeychainMissing
			case 45:
				return "", errKeychainDuplicate
			}
		}
		return "", fmt.Errorf("keychain operation failed for %s", uid)
	}
	if out.overflow || errout.overflow {
		return "", fmt.Errorf("keychain output limit exceeded for %s", uid)
	}
	return out.String(), nil
}

func interactionFailure(err error) bool {
	if errors.Is(err, exec.ErrWaitDelay) || errors.Is(err, errSecurityProcessIO) || errors.Is(err, errSecurityProcessCleanup) {
		return true
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	// errSecAuthFailed, errSecInteractionNotAllowed, errSecUserCanceled,
	// errSecInteractionRequired. Fail closed on other statuses as well.
	switch ee.ExitCode() {
	case 51, 36, 128, 29, 53:
		return true
	}
	return false
}

func (k *keychainStore) lookup(ctx context.Context, uid string) (string, error) {
	args := []string{"find-generic-password", "-s", k.selectedService(), "-a", uid, "-w"}
	target, err := k.targetPath()
	if err != nil {
		return "", fmt.Errorf("keychain target unavailable for %s", uid)
	}
	args = append(args, target)
	out, err := k.invoke(ctx, uid, "", args...)
	if err != nil {
		return "", err
	}
	// security adds one record terminator; do not normalize token bytes.
	token := strings.TrimSuffix(out, "\n")
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\x00\r\n") {
		return "", fmt.Errorf("keychain returned an invalid or empty token for %s", uid)
	}
	return token, nil
}

func (k *keychainStore) read(ctx context.Context, uid string) (string, error) {
	var token string
	err := k.locked(uid, func() error { var err error; token, err = k.lookup(ctx, uid); return err })
	if errors.Is(err, errKeychainMissing) {
		err = fmt.Errorf("keychain item missing for %s; ask the keeper to rotate credentials", uid)
	}
	return token, err
}

// security's interactive tokenizer supports double-quoted strings with
// backslash escaping. Reject command separators/control bytes, never a shell.
func securityQuote(s string) (string, error) {
	for _, r := range s {
		if r < 32 || r == 127 {
			return "", errors.New("invalid keychain value")
		}
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`, nil
}

func (k *keychainStore) write(ctx context.Context, uid, token string) error {
	if token == "" {
		return fmt.Errorf("refusing empty token for %s", uid)
	}
	account, err := securityQuote(uid)
	if err != nil {
		return fmt.Errorf("invalid keychain identity")
	}
	secret, err := securityQuote(token)
	if err != nil {
		return fmt.Errorf("invalid keychain token for %s", uid)
	}
	return k.locked(uid, func() error {
		_, err := k.lookup(ctx, uid)
		existing := err == nil
		if existing && k.createOnly {
			return errKeychainDuplicate
		}
		if err != nil && !errors.Is(err, errKeychainMissing) {
			return err
		}
		write := func(update bool) error {
			service, e := securityQuote(k.selectedService())
			if e != nil {
				return e
			}
			line := "add-generic-password -s " + service + " -a " + account + " -w " + secret
			if update {
				line += " -U"
			} else {
				line += " -T /usr/bin/security"
			}
			{
				target, e := k.targetPath()
				if e != nil {
					return fmt.Errorf("keychain target unavailable for %s", uid)
				}
				q, e := securityQuote(target)
				if e != nil {
					return e
				}
				line += " " + q
			}
			if len(line)+1 >= 4096 {
				return fmt.Errorf("keychain command exceeds security input limit for %s", uid)
			}
			_, err := k.invoke(ctx, uid, line+"\n", "-i")
			return err
		}
		err = write(existing)
		if !existing && errors.Is(err, errKeychainDuplicate) && !k.createOnly {
			// One bounded recovery, no retry loop. A denied lookup is not absence.
			if _, err = k.lookup(ctx, uid); err != nil {
				return err
			}
			err = write(true)
		}
		if err != nil {
			return err
		}
		got, err := k.lookup(ctx, uid)
		if err != nil {
			return err
		}
		if got != token {
			return fmt.Errorf("keychain readback mismatch for %s", uid)
		}
		return nil
	})
}
