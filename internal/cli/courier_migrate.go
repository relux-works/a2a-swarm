package cli

import (
	"context"
	"errors"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// cmdCourierMigrate handles migration arguments after the Courier dispatcher.
func cmdCourierMigrate(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("courier migrate")
	to := fs.String("to", "", "absolute private Courier base")
	resume := fs.Bool("resume", false, "explicitly resume a fenced transaction")
	rollback := fs.Bool("rollback", false, "restore a fenced transaction")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *to == "" || *resume && *rollback || a.human || a.as != "" && a.as != pos[0] {
		return usagef("courier migrate <identity> --to <courier-base> [--resume|--rollback]")
	}
	name := pos[0]
	if err = config.ValidateName(name); err != nil {
		return err
	}
	p, err := a.pathsFor(name)
	if err != nil {
		return config.ErrCourierMigration
	}
	store, err := a.selectedStore()
	if err != nil {
		return config.ErrCourierMigration
	}
	var binding *config.ConnectionBinding
	if c, e := a.selectedConnection(); e != nil {
		return config.ErrCourierMigration
	} else if c != nil {
		b, e := c.Binding(name, false)
		if e != nil {
			return config.ErrCourierMigration
		}
		binding = &b
	}
	var secrets config.CourierSecretStore
	if store == "keychain" {
		if a.courierSecrets != nil {
			secrets = a.courierSecrets
		} else {
			k := a.keychainStore(p)
			if c, e := a.selectedConnection(); e != nil {
				return config.ErrCourierMigration
			} else if c != nil {
				k.service = c.Store.AccessTokenService
			}
			secrets = courierKeychainStore{k: k}
		}
	}
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	err = config.MigrateCourier(ctx, p, name, config.CourierMigrationOptions{Binding: binding, To: *to, Resume: *resume, Rollback: *rollback, Keychain: store == "keychain", Secrets: secrets, Now: now})
	if err != nil {
		return err
	}
	if *rollback {
		a.printf("courier migration rollback verified\n")
	} else {
		a.printf("courier migration verified; source identity fenced\n")
	}
	return nil
}

type courierKeychainStore struct{ k *keychainStore }

func (s courierKeychainStore) Read(ctx context.Context, c config.Credentials) (config.Credentials, error) {
	token, e := s.k.read(ctx, c.UserID)
	if e != nil {
		return config.Credentials{}, config.ErrCourierMigration
	}
	c.AccessToken = token
	c.Password = ""
	return c, nil
}
func (s courierKeychainStore) Delete(ctx context.Context, c config.Credentials) error {
	return s.k.locked(c.UserID, func() error {
		// Never delete a changed/replaced item. An interrupted successful deletion
		// resumes idempotently using the already verified Courier file.
		got, e := s.k.lookup(ctx, c.UserID)
		if errors.Is(e, errKeychainMissing) {
			return nil
		}
		if e != nil || got != c.AccessToken {
			return config.ErrCourierMigration
		}
		target, e := s.k.targetPath()
		if e != nil {
			return config.ErrCourierMigration
		}
		_, e = s.k.invoke(ctx, c.UserID, "", "delete-generic-password", "-s", s.k.selectedService(), "-a", c.UserID, target)
		if e != nil {
			return config.ErrCourierMigration
		}
		_, e = s.k.lookup(ctx, c.UserID)
		if !errors.Is(e, errKeychainMissing) {
			return config.ErrCourierMigration
		}
		return nil
	})
}
func (s courierKeychainStore) Restore(ctx context.Context, c config.Credentials) error {
	// A pre-existing different value is never overwritten by rollback.
	got, e := s.k.read(ctx, c.UserID)
	if e == nil {
		if got != c.AccessToken {
			return config.ErrCourierMigration
		}
		return nil
	}
	k := *s.k
	k.createOnly = true
	if e = k.write(ctx, c.UserID, c.AccessToken); e != nil {
		return config.ErrCourierMigration
	}
	got, e = k.read(ctx, c.UserID)
	if e != nil || got != c.AccessToken {
		return config.ErrCourierMigration
	}
	return nil
}
