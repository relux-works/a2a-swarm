// Package admission is the build-time policy seam. It has no CLI or policy dependencies.
package admission

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

type Verdict uint8

const (
	Live Verdict = iota
	Exchange
	Refused
)

type Decision struct {
	Verdict   Verdict
	Retryable bool
	Reason    string
}

// Reasons is a closed vocabulary. A fork extends the contract before adding codes.
func ValidReason(s string) bool {
	switch s {
	case "allowed", "exchange", "denied", "dependency_unavailable", "policy_invalid", "policy_error", "exchange_outbound", "held_age", "held_count", "exchange_capacity", "storage_error":
		return true
	}
	return false
}

func Normalize(d Decision, err error) Decision {
	if err != nil {
		return Decision{Verdict: Refused, Reason: "policy_error"}
	}
	if d.Verdict > Refused || !ValidReason(d.Reason) || d.Retryable && d.Verdict != Refused {
		return Decision{Verdict: Refused, Reason: "policy_invalid"}
	}
	return d
}

type Inbound struct {
	RoomID    string `json:"room"`
	EventID   string `json:"event_id"`
	Sender    string `json:"sender"`
	Kind      string `json:"kind"`
	Timestamp int64  `json:"timestamp"`
	Body      []byte `json:"body"`
}

type Outbound struct {
	Identity      string
	RoomID        string
	TransactionID string
	Kind          string
	Request       []byte
}

type Checker interface {
	Inbound(context.Context, Inbound) (Decision, error)
	Outbound(context.Context, Outbound) (Decision, error)
	Close() error
}
type Config struct {
	Identity string
	StateDir string
	Nostr    bool
}
type Factory func(Config) (Checker, error)

type registration struct {
	factory Factory
	nostr   bool
}

var registry = map[string]registration{"none": {factory: func(Config) (Checker, error) { return none{}, nil }}}
var registryMu sync.RWMutex

// Register is a build-time operation: call it from a package init function.
func Register(name string, f Factory) { register(name, f, false) }

// RegisterNostrOnly declares a capability requirement, independently of the name.
func RegisterNostrOnly(name string, f Factory) { register(name, f, true) }
func register(name string, f Factory, nostr bool) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if name == "" || f == nil {
		panic("admission: registration_invalid")
	}
	if _, exists := registry[name]; exists {
		panic("admission: registration_duplicate")
	}
	callers := make([]uintptr, 16)
	frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers)])
	fromInit := false
	for {
		frame, more := frames.Next()
		if strings.Contains(frame.Function, ".init.") || strings.HasSuffix(frame.Function, ".init") {
			fromInit = true
			break
		}
		if !more {
			break
		}
	}
	if !fromInit {
		panic("admission: registration_outside_init")
	}
	registry[name] = registration{f, nostr}
}
func Lookup(name string) (Factory, bool) {
	registryMu.RLock()
	r, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, false
	}
	return func(c Config) (Checker, error) {
		if r.nostr && !c.Nostr {
			return nil, errors.New("admission: nostr_required")
		}
		return r.factory(c)
	}, true
}

type none struct{}

func (none) Inbound(context.Context, Inbound) (Decision, error) {
	return Decision{Verdict: Live, Reason: "allowed"}, nil
}
func (none) Outbound(context.Context, Outbound) (Decision, error) {
	return Decision{Verdict: Live, Reason: "allowed"}, nil
}
func (none) Close() error { return nil }

// Open receives only host-derived configuration. Errors never wrap policy text.
func Open(name string, c Config) (Checker, error) {
	f, ok := Lookup(name)
	if !ok {
		return nil, errors.New("admission: policy_unknown")
	}
	if err := PrivateDir(c.StateDir); err != nil {
		return nil, err
	}
	checker, err := f(c)
	if err != nil || checker == nil {
		if checker != nil {
			_ = checker.Close()
		}
		return nil, errors.New("admission: policy_startup")
	}
	return checker, nil
}

// PrivateDir rejects links and permissive existing directories rather than repairing them.
func PrivateDir(path string) error {
	// The identity base is host-derived; reject a linked immediate parent.
	parent := filepath.Dir(path)
	if parent != path {
		if info, err := os.Lstat(parent); os.IsNotExist(err) {
			if err := PrivateDir(parent); err != nil {
				return err
			}
		} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrStorage
		}
	}
	if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
		return ErrStorage
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return ErrStorage
	}
	return nil
}
