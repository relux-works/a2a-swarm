package cli

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/config"
)

const envSecretStore = "A2A_SECRET_STORE"

func (a *App) selectedStore() (string, error) {
	c, e := a.selectedConnection()
	if e != nil {
		return "", e
	}
	if c != nil {
		if runtime.GOOS != "darwin" && a.keychain == nil && a.connectionSecretStore == nil {
			return "", fmt.Errorf("keychain secret store requires macOS")
		}
		return c.Store.Backend, nil
	}
	switch a.Getenv(envSecretStore) {
	case "", "file":
		return "file", nil
	case "keychain":
		if runtime.GOOS != "darwin" && a.keychain == nil {
			return "", fmt.Errorf("keychain secret store requires macOS")
		}
		return "keychain", nil
	default:
		return "", fmt.Errorf("unknown A2A_SECRET_STORE; use file or keychain")
	}
}

func (a *App) keychainStore(p config.Paths) *keychainStore {
	if a.keychain != nil {
		return a.keychain(p)
	}
	return &keychainStore{paths: p}
}

func validateCredentialIdentity(c config.Credentials, name string) error {
	prefix := "@" + name + ":"
	if !strings.HasPrefix(c.UserID, prefix) || len(c.UserID) == len(prefix) || strings.ContainsAny(c.UserID, "\x00\r\n\t \"\\") {
		return fmt.Errorf("credential user_id does not match selected identity %s", name)
	}
	return nil
}

func (a *App) loadCredentials(ctx context.Context, p config.Paths, name string) (config.Credentials, error) {
	if c, e := a.selectedConnection(); e != nil {
		return config.Credentials{}, e
	} else if c != nil {
		creds, e := a.namedMetadata(p, name)
		if e != nil {
			return config.Credentials{}, e
		}
		creds.AccessToken, e = a.readConnectionSecret(ctx, p, config.SecretReference{Service: c.Store.AccessTokenService, Account: creds.UserID})
		if e != nil {
			return config.Credentials{}, e
		}
		if creds.AccessToken == "" {
			return config.Credentials{}, fmt.Errorf("missing selected access token")
		}
		return creds, nil
	}
	store, err := a.selectedStore()
	if err != nil {
		return config.Credentials{}, err
	}
	c, err := config.LoadCredentialMetadata(p, name, a.human)
	if err != nil {
		return config.Credentials{}, err
	}
	a.secrets.Add(c.AccessToken, c.Password)
	if err := validateCredentialIdentity(c, name); err != nil {
		return config.Credentials{}, err
	}
	if store == "keychain" {
		if a.human {
			return config.Credentials{}, errHumanKeychain
		}
		c.AccessToken = "" // A file token is never a fallback, even on lookup failure.
		c.Password = ""
		c.AccessToken, err = a.keychainStore(p).read(ctx, c.UserID)
		if err != nil {
			return config.Credentials{}, err
		}
	}
	if c.AccessToken == "" {
		return config.Credentials{}, fmt.Errorf("missing access token for %s", name)
	}
	c.Password = ""
	return c, nil
}

// reconcileCredentials clears the selected identity's Keychain write fence
// after its writer died, once the keychain answers (keychain_fence.go). It
// works for legacy and named profiles and prints no secret.
func (a *App) reconcileCredentials(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return usagef("usage: a2a --as <identity> creds reconcile")
	}
	if a.human {
		return errHumanKeychain
	}
	name, err := a.identity()
	if err != nil {
		return err
	}
	p, err := a.paths()
	if err != nil {
		return err
	}
	k := a.keychainStore(p)
	var uid string
	if c, e := a.selectedConnection(); e != nil {
		return e
	} else if c != nil {
		creds, e := a.namedMetadata(p, name)
		if e != nil {
			return e
		}
		k.service = c.Store.AccessTokenService
		uid = creds.UserID
	} else {
		store, err := a.selectedStore()
		if err != nil {
			return err
		}
		if store != "keychain" {
			return fmt.Errorf("creds reconcile requires A2A_SECRET_STORE=keychain")
		}
		c, err := config.LoadCredentialMetadata(p, name, false)
		if err != nil {
			return err
		}
		a.secrets.Add(c.AccessToken, c.Password)
		if err := validateCredentialIdentity(c, name); err != nil {
			return err
		}
		uid = c.UserID
	}
	outcome, err := k.reconcile(ctx, uid)
	if err != nil {
		return err
	}
	a.printf("reconciled %s: %s\n", name, outcome)
	return nil
}

func (a *App) migrateCredentials(ctx context.Context, args []string) error {
	if c, e := a.selectedConnection(); e != nil {
		return e
	} else if c != nil {
		return fmt.Errorf("named credentials require an explicit migration packet; legacy rewrite refused")
	}
	if len(args) != 0 {
		return usagef("usage: a2a --as <identity> creds migrate (explicit selected identity only)")
	}
	name, err := a.identity()
	if err != nil {
		return err
	}
	store, err := a.selectedStore()
	if err != nil {
		return err
	}
	if store != "keychain" {
		return fmt.Errorf("creds migrate requires A2A_SECRET_STORE=keychain")
	}
	if a.human {
		return errHumanKeychain
	}
	p, err := a.paths()
	if err != nil {
		return err
	}
	err = config.RewriteCredentialMetadata(p, name, a.human, func(c *config.Credentials) error {
		a.secrets.Add(c.AccessToken, c.Password)
		if err := validateCredentialIdentity(*c, name); err != nil {
			return err
		}
		k := a.keychainStore(p)
		if c.AccessToken != "" {
			if err := k.write(ctx, c.UserID, c.AccessToken); err != nil {
				return err
			}
		} else {
			if _, err := k.read(ctx, c.UserID); err != nil {
				return err
			}
		}
		if a.beforeCredentialScrub != nil {
			if err := a.beforeCredentialScrub(); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("credential migration interrupted")
		}
		c.AccessToken, c.Password = "", ""
		return nil
	})
	if err != nil {
		return err
	}
	a.printf("migrated %s to keychain; credential file contains metadata only\n", name)
	return nil
}
