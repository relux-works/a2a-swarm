package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// A mutation the hard deadline abandoned may still complete later: the
// in-process Security call cannot be cancelled, and its request may already be
// queued in securityd, even after this process exits. The identity and host
// locks are released when run returns, so they cannot exclude it. Before every
// add or update the store therefore records a durable fence for the identity:
// the recording process, the boot session and a SHA-256 digest of the value
// being written. Only the goroutine that ran the native call removes it, after
// that call has actually returned. While the fence exists every operation for
// the identity refuses, whatever the cooldown says.
//
// A fence whose recording process is gone never expires on its own.
// `a2a creds reconcile` clears it only when the recorder is confirmed gone AND
// the abandoned write can no longer land: either its intended value is
// observed in the item (the write completed), or the host has restarted since
// the fence was recorded (securityd and its queue are gone). An unknown
// liveness, a different or missing value, or an unanswered lookup keeps it.
type keychainFence struct {
	PID    int    `json:"pid"`
	Birth  string `json:"birth"`
	Op     string `json:"op"`
	At     int64  `json:"at"`
	Boot   int64  `json:"boot"`
	Digest string `json:"digest"`
}

type ownerState int

const (
	ownerUnknown ownerState = iota
	ownerAlive
	ownerGone
)

var errKeychainFenced = errors.New("keychain write outstanding")

func (k *keychainStore) fencePath(uid string) string { return k.identityPath(uid) + ".pending" }

func tokenDigest(secret []byte) string {
	sum := sha256.Sum256(secret)
	return hex.EncodeToString(sum[:])
}

func (k *keychainStore) processBirth(ctx context.Context, pid int) (string, error) {
	if k.birth != nil {
		return k.birth(ctx, pid)
	}
	return processIdentity(ctx, pid)
}

func (k *keychainStore) ownerLiveness(ctx context.Context, fence *keychainFence) ownerState {
	if k.liveness != nil {
		return k.liveness(ctx, fence.PID, fence.Birth)
	}
	return processLiveness(ctx, fence.PID, fence.Birth)
}

func (k *keychainStore) bootSession() (int64, error) {
	if k.boot != nil {
		return k.boot()
	}
	return keychainBootSession()
}

// readFence returns nil when no fence exists. A fence that cannot be read or
// parsed refuses, like every other private state file.
func (k *keychainStore) readFence(uid string) (*keychainFence, error) {
	f, err := openKeychainState(k.fencePath(uid))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("keychain write fence unreadable for %s", uid)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1025))
	var fence keychainFence
	if err != nil || len(data) > 1024 || json.Unmarshal(data, &fence) != nil || fence.PID <= 0 || fence.Birth == "" ||
		(fence.Op != "add" && fence.Op != "update") || len(fence.Digest) != 64 {
		return nil, fmt.Errorf("keychain write fence invalid for %s", uid)
	}
	return &fence, nil
}

// checkFence refuses every operation while a write for the identity is
// unresolved.
func (k *keychainStore) checkFence(ctx context.Context, uid string) error {
	fence, err := k.readFence(uid)
	if err != nil || fence == nil {
		return err
	}
	switch k.ownerLiveness(ctx, fence) {
	case ownerAlive:
		return fmt.Errorf("%w for %s in pid %d; refusing until it completes", errKeychainFenced, uid, fence.PID)
	case ownerGone:
		return fmt.Errorf("%w for %s: pid %d ended before its write completed; run `a2a creds reconcile`", errKeychainFenced, uid, fence.PID)
	}
	return fmt.Errorf("%w for %s: the state of pid %d is unknown; refusing", errKeychainFenced, uid, fence.PID)
}

func (k *keychainStore) writeFence(ctx context.Context, uid, op string, intended []byte) (*keychainFence, error) {
	birth, err := k.processBirth(ctx, os.Getpid())
	if err != nil {
		return nil, err
	}
	boot, err := k.bootSession()
	if err != nil {
		boot = 0 // without a boot session, a restart never counts as quiescence
	}
	fence := &keychainFence{PID: os.Getpid(), Birth: birth, Op: op, At: k.clock().UnixNano(), Boot: boot, Digest: tokenDigest(intended)}
	data, err := json.Marshal(fence)
	if err != nil {
		return nil, err
	}
	path := k.fencePath(uid)
	f, err := os.CreateTemp(filepath.Dir(path), ".pending-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(append(data, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err == nil {
		err = syncDir(filepath.Dir(path))
	}
	if err != nil {
		return nil, err
	}
	return fence, nil
}

// clearFence removes the fence only while it still is the one this write
// recorded.
func (k *keychainStore) clearFence(uid string, fence *keychainFence) error {
	current, err := k.readFence(uid)
	if err != nil {
		return err
	}
	if current == nil || *current != *fence {
		return nil
	}
	return k.removeFence(uid)
}

func (k *keychainStore) removeFence(uid string) error {
	if err := os.Remove(k.fencePath(uid)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Dir(k.fencePath(uid)))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// reconcile clears the fence of an abandoned write only when its recorder is
// confirmed gone and the write can no longer land (see keychainFence). It
// prints no secret and compares only digests.
func (k *keychainStore) reconcile(ctx context.Context, uid string) (string, error) {
	var outcome string
	err := k.locked(uid, func() error {
		fence, err := k.readFence(uid)
		if err != nil {
			return err
		}
		if fence == nil {
			outcome = "no unresolved keychain write"
			return nil
		}
		switch k.ownerLiveness(ctx, fence) {
		case ownerAlive:
			return fmt.Errorf("%w for %s in pid %d; not reconciled", errKeychainFenced, uid, fence.PID)
		case ownerUnknown:
			return fmt.Errorf("%w for %s: the state of pid %d is unknown; fence kept", errKeychainFenced, uid, fence.PID)
		}
		if boot, err := k.bootSession(); err == nil && fence.Boot != 0 && boot != fence.Boot {
			outcome = "host restarted since the abandoned write; fence cleared"
			return k.removeFence(uid)
		}
		k.reconciling = true
		defer func() { k.reconciling = false }()
		token, err := k.lookup(ctx, uid)
		switch {
		case err == nil && tokenDigest([]byte(token)) == fence.Digest:
			outcome = "abandoned write observed completed; fence cleared"
			return k.removeFence(uid)
		case err == nil || errors.Is(err, errKeychainMissing):
			return fmt.Errorf("%w for %s: the abandoned write is not observed and the host has not restarted since; fence kept, because the request may still land", errKeychainFenced, uid)
		default:
			return fmt.Errorf("keychain did not answer for %s; fence kept: %w", uid, err)
		}
	})
	return outcome, err
}
