package config

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrIdentityMigrated is a closed refusal, independent of credentials or routing.
var ErrIdentityMigrated = errors.New("identity_migrated")

// Unreadable/malformed State cannot prove absence of a migration fence. A
// consumer may retry the read without performing any effect or reopening.
var ErrMigrationStateUnavailable = errors.New("identity migration state unavailable")

// IdentityMigration remains at the source until an explicit verified rollback.
// Unknown/malformed phases are fenced too; this is never a lease or a timeout.
type IdentityMigration struct {
	Phase       string    `json:"phase"`
	CourierBase string    `json:"courier_base"`
	At          time.Time `json:"at"`
	// Durable intent distinguishes a proven pre-cutover abort from a lost
	// private target after source removal, including a recreated Keychain item.
	CutoverStarted bool `json:"cutover_started,omitempty"`
}

// CheckIdentityMigration is an atomic-file read, deliberately without lock
// acquisition: callers may already hold the state or consumer lock. Mutating
// effects recheck the loaded State under its lock; consumers hold their lifetime
// lock before this check, excluding the entire migration operation.
func CheckIdentityMigration(p Paths, name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if p.AgentStateBase != "" && p.AgentStateBase != p.Base {
		agent := p
		agent.Base, agent.AgentStateBase = p.AgentStateBase, ""
		if err := CheckIdentityMigration(agent, name); err != nil {
			return err
		}
	}
	s, err := LoadState(p, name)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrMigrationStateUnavailable, err)
	}
	if s.Migration != nil {
		return ErrIdentityMigrated
	}
	return nil
}

var ErrCourierMigration = errors.New("courier migration refused; source remains fenced if migration started")
var ErrCourierSourceSecrets = fmt.Errorf("%w: source_credentials_contain_secrets; complete creds migrate or repair source credentials", ErrCourierMigration)
var ErrCourierPostPending = fmt.Errorf("%w: post_operation_pending; finish the active post and inspect post-operation status before migration", ErrCourierMigration)

// CourierSecretStore is injected by the CLI. Errors and values from a provider
// never enter migration diagnostics or the nonsecret journal. Restore must be
// idempotent and verify readback; Delete must accept an already missing item.
type CourierSecretStore interface {
	Read(context.Context, Credentials) (Credentials, error)
	Delete(context.Context, Credentials) error
	Restore(context.Context, Credentials) error
}

type CourierMigrationOptions struct {
	Binding          *ConnectionBinding
	To               string
	Resume, Rollback bool
	Keychain         bool
	Secrets          CourierSecretStore
	Now              time.Time
	// Package-private fault seam; no flag/environment can inject filesystem code.
	step func(string) error
}

// WithIdentityEffect guards a non-State effect against migration under the
// existing state/lease lock. fn must not acquire a state or consumer lock.
// It does not save State or manufacture a compatible-writer seal.
func WithIdentityEffect(p Paths, name string, deadline time.Time, fn func() error) error {
	_, err := withStateLock(p, name, func(path string) (func(), error) { return lockFileBy(path, deadline) }, func(s State) (State, error) {
		if s.Migration != nil {
			return s, ErrIdentityMigrated
		}
		return s, fn()
	})
	return err
}
