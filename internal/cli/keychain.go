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
	backend           keychainBackend
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
		return fmt.Errorf("keychain cooldown active for %s; retry after %s", uid, time.Unix(0, until).UTC().Format(time.RFC3339))
	}
	return nil
}

func (k *keychainStore) recordCooldown(uid string) error {
	return k.recordMarker(k.identityPath(uid) + ".cooldown")
}

func (k *keychainStore) recordMarker(path string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".cooldown-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err == nil {
		_, err = fmt.Fprintln(f, k.clock().Add(k.cooldownDuration()).UnixNano())
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

// run applies the identity cooldown, host lock, host cooldown and status
// preflight to EVERY Keychain operation, including write lookup, relookup and
// readback, then runs op in this process under the hard deadline. A timeout or
// an interaction refusal records the identity cooldown. Backend error text
// never escapes into CLI errors.
func (k *keychainStore) run(ctx context.Context, uid string, op func(context.Context, keychainBackend, string) ([]byte, error)) ([]byte, error) {
	if err := k.checkCooldown(uid); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("keychain operation interrupted for %s", uid)
	}
	host, err := k.hostPath()
	if err != nil {
		return nil, fmt.Errorf("keychain host binding unavailable for %s", uid)
	}
	unlock, err := keychainLock(host+".lock", time.Now().Add(k.timeoutDuration()))
	if err != nil {
		return nil, fmt.Errorf("keychain host lock unavailable for %s", uid)
	}
	defer unlock()
	if err := k.checkMarker(host+".cooldown", uid); err != nil {
		return nil, err
	}
	if err := k.preflight(ctx); err != nil {
		// Persist host first: other identities must not probe a known locked host.
		herr := k.recordMarker(host + ".cooldown")
		ierr := k.recordCooldown(uid)
		if herr != nil || ierr != nil {
			return nil, fmt.Errorf("keychain preflight failed for %s; cannot persist cooldown; repair private state", uid)
		}
		return nil, fmt.Errorf("keychain preflight failed for %s; cooldown recorded", uid)
	}
	target, err := k.targetPath()
	if err != nil {
		return nil, fmt.Errorf("keychain target unavailable for %s", uid)
	}
	ctx, cancel := context.WithTimeout(ctx, k.timeoutDuration())
	defer cancel()
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	backend := k.keychainBackend()
	go func() {
		data, err := op(ctx, backend, target)
		done <- result{data, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
		// An in-process Security.framework call cannot be killed. It runs with
		// UI disallowed, so a late result is discarded and the attempt counts
		// as a timeout.
		r.err = ctx.Err()
	}
	if ctx.Err() != nil || errors.Is(r.err, errKeychainInteraction) {
		if e := k.recordCooldown(uid); e != nil {
			return nil, fmt.Errorf("keychain interaction failed for %s; cannot persist cooldown; stop and repair private state", uid)
		}
		return nil, fmt.Errorf("keychain interaction failed or timed out for %s; cooldown recorded", uid)
	}
	switch {
	case r.err == nil:
		return r.data, nil
	case errors.Is(r.err, errKeychainMissing):
		return nil, errKeychainMissing
	case errors.Is(r.err, errKeychainDuplicate):
		return nil, errKeychainDuplicate
	}
	return nil, fmt.Errorf("keychain operation failed for %s", uid)
}

func (k *keychainStore) lookup(ctx context.Context, uid string) (string, error) {
	data, err := k.run(ctx, uid, func(ctx context.Context, b keychainBackend, target string) ([]byte, error) {
		return b.find(ctx, target, k.selectedService(), uid)
	})
	if err != nil {
		return "", err
	}
	token := string(data)
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

// controlBytes reports a C0 control byte or DEL, which no identity or token
// contains.
func controlBytes(s string) bool {
	for _, r := range s {
		if r < 32 || r == 127 {
			return true
		}
	}
	return false
}

func (k *keychainStore) write(ctx context.Context, uid, token string) error {
	if token == "" {
		return fmt.Errorf("refusing empty token for %s", uid)
	}
	if controlBytes(uid) {
		return fmt.Errorf("invalid keychain identity")
	}
	if controlBytes(token) {
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
		put := func(update bool) error {
			_, err := k.run(ctx, uid, func(ctx context.Context, b keychainBackend, target string) ([]byte, error) {
				if update {
					return nil, b.update(ctx, target, k.selectedService(), uid, []byte(token))
				}
				return nil, b.add(ctx, target, k.selectedService(), uid, []byte(token))
			})
			return err
		}
		err = put(existing)
		if !existing && errors.Is(err, errKeychainDuplicate) && !k.createOnly {
			// One bounded recovery, no retry loop. A denied lookup is not absence.
			if _, err = k.lookup(ctx, uid); err != nil {
				return err
			}
			err = put(true)
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
