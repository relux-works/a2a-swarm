package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// No current native deletion/recovery contract is available to the CLI. This
// fixed preflight refusal also covers already-pending resume and rollback.
var errDaemonKeychainMigrationDeferred = fmt.Errorf("%w: keychain_daemon_migration_deferred; initial, resume and rollback require the separate native deletion design in #460", config.ErrDaemonMigration)

// cmdDaemonMigrate handles migration arguments after the Daemon dispatcher.
func cmdDaemonMigrate(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("daemon migrate")
	to := fs.String("to", "", "absolute private Daemon base")
	resume := fs.Bool("resume", false, "explicitly resume a fenced transaction")
	rollback := fs.Bool("rollback", false, "restore a fenced transaction")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *to == "" || *resume && *rollback || a.human || a.as != "" && a.as != pos[0] {
		return usagef("daemon migrate <identity> --to <daemon-base> [--resume|--rollback]")
	}
	name := pos[0]
	if err = config.ValidateName(name); err != nil {
		return err
	}
	p, err := a.pathsFor(name)
	if err != nil {
		return config.ErrDaemonMigration
	}
	var binding *config.ConnectionBinding
	c, e := a.selectedConnection()
	if e != nil {
		return config.ErrDaemonMigration
	} else if c != nil {
		b, e := c.Binding(name, false)
		if e != nil {
			return config.ErrDaemonMigration
		}
		binding = &b
	}
	// Selection is already validated by pathsFor/selectedConnection. Refuse
	// Keychain before platform availability, constructors, injected stores or
	// the migration transaction: pending native deletion is not a file fallback.
	if c != nil && c.Store.Backend == "keychain" || c == nil && a.Getenv(envSecretStore) == "keychain" {
		return errDaemonKeychainMigrationDeferred
	}
	if _, err := a.selectedStore(); err != nil {
		return config.ErrDaemonMigration
	}
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	err = config.MigrateDaemon(ctx, p, name, config.DaemonMigrationOptions{Binding: binding, To: *to, Resume: *resume, Rollback: *rollback, Now: now})
	if err != nil {
		return err
	}
	if *rollback {
		a.printf("daemon migration rollback verified\n")
	} else {
		a.printf("daemon migration verified; source identity fenced\n")
	}
	return nil
}
