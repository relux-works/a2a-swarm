package cli

import (
	"context"
	"sync"
)

// daemonOwnerEpoch is deliberately memory-only. Disk ownership is necessary,
// but a new process also requires a successfully committed fresh owner hook.
// Per-identity cancellable serialization orders disk commits and publication.
type daemonOwnerEpoch struct {
	mu     sync.Mutex
	owners map[string]string
	hooks  map[string]chan struct{}
}

func newDaemonOwnerEpoch() *daemonOwnerEpoch {
	return &daemonOwnerEpoch{owners: map[string]string{}, hooks: map[string]chan struct{}{}}
}
func (e *daemonOwnerEpoch) owner(id string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.owners[id]
}
func (e *daemonOwnerEpoch) hook(ctx context.Context, id, session, event string, exit bool, commit func() error) error {
	e.mu.Lock()
	gate := e.hooks[id]
	if gate == nil {
		gate = make(chan struct{}, 1)
		e.hooks[id] = gate
	}
	e.mu.Unlock()
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-gate }()
	if err := commit(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if event == "SessionStart" {
		e.owners[id] = session
	}
	if (event == "SessionEnd" || event == "Stop" && exit) && e.owners[id] == session {
		delete(e.owners, id)
	}
	return nil
}
