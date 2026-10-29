package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// A mutation the hard deadline abandoned may still complete later: the
// in-process Security call cannot be cancelled, and its request may already be
// queued in securityd, even after this process exits. The identity and host
// locks are released when run returns, so they cannot exclude it. Before every
// add or update the store therefore records a durable fence for the identity.
// Only the goroutine that ran the native call removes it, after that call has
// actually returned. While the fence exists every operation for the identity
// refuses, whatever the cooldown says. A fence whose recording process is gone
// never expires: `a2a creds reconcile` clears it once the owner is dead and the
// keychain answers a lookup.
type keychainFence struct {
	PID   int    `json:"pid"`
	Birth string `json:"birth"`
	Op    string `json:"op"`
	At    int64  `json:"at"`
}

var errKeychainFenced = errors.New("keychain write outstanding")

func (k *keychainStore) fencePath(uid string) string { return k.identityPath(uid) + ".pending" }

func (k *keychainStore) processBirth(ctx context.Context, pid int) (string, error) {
	if k.birth != nil {
		return k.birth(ctx, pid)
	}
	return tmuxwake.Birth(ctx, pid)
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
	if err != nil || len(data) > 1024 || json.Unmarshal(data, &fence) != nil || fence.PID <= 0 || fence.Birth == "" || (fence.Op != "add" && fence.Op != "update") {
		return nil, fmt.Errorf("keychain write fence invalid for %s", uid)
	}
	return &fence, nil
}

// ownerAlive reports whether the fence's recording process still runs: the
// same pid with the same start time.
func (k *keychainStore) ownerAlive(ctx context.Context, fence *keychainFence) bool {
	birth, err := k.processBirth(ctx, fence.PID)
	return err == nil && birth == fence.Birth
}

// checkFence refuses every operation while a write for the identity is
// unresolved.
func (k *keychainStore) checkFence(ctx context.Context, uid string) error {
	fence, err := k.readFence(uid)
	if err != nil || fence == nil {
		return err
	}
	if k.ownerAlive(ctx, fence) {
		return fmt.Errorf("%w for %s in pid %d; refusing until it completes", errKeychainFenced, uid, fence.PID)
	}
	return fmt.Errorf("%w for %s: pid %d ended before its write completed; run `a2a creds reconcile`", errKeychainFenced, uid, fence.PID)
}

func (k *keychainStore) writeFence(ctx context.Context, uid, op string) (*keychainFence, error) {
	birth, err := k.processBirth(ctx, os.Getpid())
	if err != nil {
		return nil, err
	}
	fence := &keychainFence{PID: os.Getpid(), Birth: birth, Op: op, At: k.clock().UnixNano()}
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

// reconcile clears the fence of a write whose recording process is gone, once
// the keychain answers a lookup for the identity. It never clears the fence of
// a live owner, and it keeps the fence when the lookup fails or times out.
// Limit: a request of the dead writer that securityd still holds and processes
// after this lookup is not excluded; the lookup only shows that the keychain
// answers again.
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
		if k.ownerAlive(ctx, fence) {
			return fmt.Errorf("%w for %s in pid %d; not reconciled", errKeychainFenced, uid, fence.PID)
		}
		k.reconciling = true
		defer func() { k.reconciling = false }()
		_, err = k.lookup(ctx, uid)
		switch {
		case err == nil:
			outcome = "keychain answered; item present"
		case errors.Is(err, errKeychainMissing):
			outcome = "keychain answered; item missing"
		default:
			return fmt.Errorf("keychain did not answer for %s; fence kept: %w", uid, err)
		}
		if err := os.Remove(k.fencePath(uid)); err != nil {
			return fmt.Errorf("keychain write fence not removed for %s", uid)
		}
		return syncDir(filepath.Dir(k.fencePath(uid)))
	})
	return outcome, err
}
