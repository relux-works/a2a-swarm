package cli

import (
	"context"
	"errors"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

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
	store, err := a.selectedStore()
	if err != nil {
		return config.ErrDaemonMigration
	}
	var binding *config.ConnectionBinding
	if c, e := a.selectedConnection(); e != nil {
		return config.ErrDaemonMigration
	} else if c != nil {
		b, e := c.Binding(name, false)
		if e != nil {
			return config.ErrDaemonMigration
		}
		binding = &b
	}
	var secrets config.DaemonSecretStore
	if store == "keychain" {
		if a.daemonSecrets != nil {
			secrets = a.daemonSecrets
		} else {
			k := a.keychainStore(p)
			if c, e := a.selectedConnection(); e != nil {
				return config.ErrDaemonMigration
			} else if c != nil {
				k.service = c.Store.AccessTokenService
			}
			secrets = daemonKeychainStore{k: k}
		}
	}
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	err = config.MigrateDaemon(ctx, p, name, config.DaemonMigrationOptions{Binding: binding, To: *to, Resume: *resume, Rollback: *rollback, Keychain: store == "keychain", Secrets: secrets, Now: now})
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

type daemonKeychainStore struct{ k *keychainStore }

func (s daemonKeychainStore) Read(ctx context.Context, c config.Credentials) (config.Credentials, error) {
	token, e := s.k.read(ctx, c.UserID)
	if e != nil {
		return config.Credentials{}, config.ErrDaemonMigration
	}
	c.AccessToken = token
	c.Password = ""
	return c, nil
}
func (s daemonKeychainStore) Delete(ctx context.Context, c config.Credentials) error {
	return s.k.locked(c.UserID, func() error {
		// Never delete a changed/replaced item. An interrupted successful deletion
		// resumes idempotently using the already verified Daemon file.
		got, e := s.k.lookup(ctx, c.UserID)
		if errors.Is(e, errKeychainMissing) {
			return nil
		}
		if e != nil || got != c.AccessToken {
			return config.ErrDaemonMigration
		}
		target, e := s.k.targetPath()
		if e != nil {
			return config.ErrDaemonMigration
		}
		_, e = s.k.invoke(ctx, c.UserID, "", "delete-generic-password", "-s", s.k.selectedService(), "-a", c.UserID, target)
		if e != nil {
			return config.ErrDaemonMigration
		}
		_, e = s.k.lookup(ctx, c.UserID)
		if !errors.Is(e, errKeychainMissing) {
			return config.ErrDaemonMigration
		}
		return nil
	})
}
func (s daemonKeychainStore) Restore(ctx context.Context, c config.Credentials) error {
	// A pre-existing different value is never overwritten by rollback.
	got, e := s.k.read(ctx, c.UserID)
	if e == nil {
		if got != c.AccessToken {
			return config.ErrDaemonMigration
		}
		return nil
	}
	k := *s.k
	k.createOnly = true
	if e = k.writeLocked(ctx, c.UserID, c.AccessToken); e != nil {
		return config.ErrDaemonMigration
	}
	got, e = k.read(ctx, c.UserID)
	if e != nil || got != c.AccessToken {
		return config.ErrDaemonMigration
	}
	return nil
}
