package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// AgentStatus is self-written Matrix telemetry. Private runtime endpoints,
// thread history, paths and credentials are deliberately absent.
type AgentStatus struct {
	Name           string    `json:"name"`
	State          string    `json:"state"`
	Detail         string    `json:"detail,omitempty"`
	Since          time.Time `json:"since"`
	HeartbeatAt    time.Time `json:"heartbeat_at"`
	HeartbeatEvery string    `json:"heartbeat_every"`
	Doorbell       string    `json:"doorbell"`
	Host           string    `json:"host"`
	Runtime        string    `json:"runtime"`
	Hooks          string    `json:"hooks"`
	Product        string    `json:"product,omitempty"`
	Wake           string    `json:"wake,omitempty"`
	Version        int       `json:"version"`
	A2AVersion     string    `json:"a2a_version"`
}

// Alert transitions have a stable Matrix transaction ID before submission,
// so a failed acknowledgement can be retried without creating another event.
type LivenessAlert struct {
	Value   string                `json:"value"`
	Pending *LivenessAlertMessage `json:"pending,omitempty"`
}
type LivenessAlertMessage struct {
	Txn     string         `json:"txn"`
	Room    string         `json:"room"`
	Content map[string]any `json:"content"`
}

// WithRuntimeLock separates native control from the mailbox lock: a wake may
// run inbox immediately, while bind/gate operations cannot race a submission.
// The consumer lock is held for the supervised consumer's entire lifetime.
func WithRuntimeLock(p Paths, name, kind string, deadline time.Time, fn func() error) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if kind != "control" && kind != "consumer" {
		return errors.New("invalid runtime lock")
	}
	switch runtime.GOOS {
	case "darwin", "linux", "freebsd", "openbsd", "netbsd":
	default:
		if kind == "consumer" {
			return errors.New("native runtime consumer requires Unix file locking")
		}
		// Global status and hook controls retain the platform's existing state
		// locking behavior. No native producer is supported here to fence.
		return fn()
	}
	path := filepath.Join(p.Base, "state", name+".runtime-"+kind+".lock")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := checkSecretFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	unlock, err := lockFileBy(path, deadline)
	if err != nil {
		return fmt.Errorf("runtime %s lock: %w", kind, err)
	}
	defer unlock()
	return fn()
}
