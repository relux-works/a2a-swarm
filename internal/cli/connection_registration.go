package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Private in-process seam: no environment/flag can select a fake secret store.
type connectionSecretStore interface {
	Read(context.Context, config.Paths, config.SecretReference) (string, error)
	Create(context.Context, config.Paths, config.SecretReference, string) error
}

func (a *App) readConnectionSecret(ctx context.Context, p config.Paths, r config.SecretReference) (string, error) {
	if store, e := a.selectedStore(); e != nil {
		return "", e
	} else if store != "keychain" {
		return "", errors.New("file connections cannot read Keychain references")
	}
	if a.connectionSecretStore != nil {
		return a.connectionSecretStore.Read(ctx, p, r)
	}
	k := a.keychainStore(p)
	k.service = r.Service
	var result string
	err := k.locked(r.Account, func() error { var e error; result, e = k.lookup(ctx, r.Account); return e })
	return result, err
}
func (a *App) createConnectionSecret(ctx context.Context, p config.Paths, r config.SecretReference, v string) error {
	if store, e := a.selectedStore(); e != nil {
		return e
	} else if store != "keychain" {
		return errors.New("file connections cannot create Keychain references")
	}
	name, _, ok := strings.Cut(strings.TrimPrefix(r.Account, "@"), ":")
	if !strings.HasPrefix(r.Account, "@") || !ok || config.ValidateName(name) != nil {
		return errors.New("invalid selected credential identity")
	}
	guard, e := config.LockCredentialMutation(p, name, false)
	if e != nil {
		return e
	}
	defer guard.Close()
	return a.createConnectionSecretLocked(ctx, p, r, v)
}

// The named registration transaction already owns the credential guard over
// both password/token writes, journal effects and metadata publication.
func (a *App) createConnectionSecretLocked(ctx context.Context, p config.Paths, r config.SecretReference, v string) error {
	if store, e := a.selectedStore(); e != nil {
		return e
	} else if store != "keychain" {
		return errors.New("file connections cannot create Keychain references")
	}
	if a.connectionSecretStore != nil {
		return a.connectionSecretStore.Create(ctx, p, r, v)
	}
	k := a.keychainStore(p)
	k.service = r.Service
	k.createOnly = true
	return k.writeLocked(ctx, r.Account, v)
}

// RegistrationJournal is private recovery evidence, without secret values.
// A retained journal always blocks a fresh attempt. Recovery is an explicit
// owner operation; source onboarding never retries an ambiguous remote creation.
type registrationJournal struct {
	Binding    config.ConnectionBinding `json:"binding"`
	Stage      string                   `json:"stage"`
	HTTPStatus int                      `json:"http_status,omitempty"`
	ErrCode    string                   `json:"errcode,omitempty"`
}

// Server-controlled error codes may contain private data. Retain only this
// closed registration vocabulary; never persist the response body or message.
func registrationRejectionCode(code string) string {
	switch code {
	case "M_FORBIDDEN", "M_USER_IN_USE", "M_INVALID_USERNAME", "M_EXCLUSIVE",
		"M_LIMIT_EXCEEDED", "M_BAD_JSON", "M_NOT_JSON", "M_MISSING_PARAM",
		"M_INVALID_PARAM", "M_UNRECOGNIZED", "M_UNAUTHORIZED",
		"M_UNKNOWN_TOKEN", "M_MISSING_TOKEN":
		return code
	}
	return ""
}

func updateRegistrationJournal(path string, j registrationJournal) error {
	data, e := json.Marshal(j)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".registration-*")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	if e = f.Chmod(0600); e != nil {
		return e
	}
	if _, e = f.Write(data); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(tmp, path)
}
func (a *App) registerNamedAgent(ctx context.Context, name string) (config.Credentials, string, error) {
	return a.registerNamedAgentUsing(ctx, name, nil)
}

func (a *App) registerNamedAgentUsing(ctx context.Context, name string, admin *session) (config.Credentials, string, error) {
	return a.registerNamedAgentDestination(ctx, name, admin, nil)
}

// A fresh-root registration is not a migration/export of an existing source
// identity. Keep the old local identity/migration/pending guards without taking
// a child credential lock or creating any child state in the source root.
func (a *App) checkMemberSource(name string) error {
	p, err := a.pathsFor(name)
	if err != nil || config.CheckIdentityMigration(p, name) != nil {
		return adminRegistrationError("destination", nil)
	}
	for _, path := range []string{p.AgentFile(name), p.HumanFile(name)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return adminRegistrationError("destination", nil)
		}
	}
	entries, err := os.ReadDir(p.Base)
	if err == nil && len(entries) != 0 || err != nil && !errors.Is(err, os.ErrNotExist) {
		return adminRegistrationError("destination", nil)
	}
	return nil
}

// The optional destination changes storage only. Source admission and every
// registration request retain the original administrator session and policy.
func (a *App) registerNamedAgentDestination(ctx context.Context, name string, admin *session, destination *config.MemberRoot) (config.Credentials, string, error) {
	empty := config.Credentials{}
	c, e := a.selectedConnection()
	if e != nil {
		return empty, "", e
	}
	store, e := a.selectedStore()
	if e != nil {
		return empty, "", e
	}
	if a.human {
		return empty, "", errors.New("agent registration cannot use a human identity")
	}
	if admin == nil && !a.authorizeRegistration {
		return empty, "", errors.New("named first registration requires explicit --authorize-registration after owner admission")
	}
	// Retain the existing caller identity guard even for this first-registration path.
	for _, bound := range []string{a.as, a.Getenv(config.EnvAgent)} {
		if admin == nil && bound != "" && bound != name {
			return empty, "", errors.New("registration does not match this session's identity")
		}
	}
	b, e := c.Binding(name, false)
	if e != nil {
		return empty, "", e
	}
	p, e := a.pathsFor(name)
	if e != nil {
		return empty, "", e
	}
	if destination != nil {
		if admin == nil || destination.Validate() != nil {
			return empty, "", adminRegistrationError("destination", nil)
		}
		c, p, b, store = &destination.Connection, destination.Paths, destination.Binding, "file"
	}
	if admin == nil && store == "file" {
		return empty, "", errors.New("file connections require connection register --via-admin-room; registration_token_ref is Keychain-only")
	}
	if admin == nil && c.RegistrationTokenRef == nil {
		return empty, "", errors.New("selected profile has no registration_token_ref")
	}
	var guard *config.CredentialGuard
	if destination != nil {
		guard = destination.Guard()
	} else {
		guard, e = config.LockCredentialMutation(p, name, false)
		if e != nil {
			return empty, "", e
		}
	}
	defer guard.Close()
	if destination == nil {
		for _, path := range []string{p.AgentFile(name), p.HumanFile(name)} {
			if _, e = os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
				return empty, "", errors.New("destination identity metadata exists or cannot be inspected")
			}
		}
		if entries, e := os.ReadDir(p.Base); e == nil && len(entries) > 0 || e != nil && !errors.Is(e, os.ErrNotExist) {
			return empty, "", errors.New("destination state or pending registration exists; owner recovery required")
		}
		if e = os.MkdirAll(p.Base, 0700); e != nil {
			return empty, "", e
		}
	}
	journal := filepath.Join(p.Base, "registration.pending.json")
	if destination == nil {
		f, e := os.OpenFile(journal, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return empty, "", errors.New("registration already pending; owner recovery required")
		}
		f.Close()
	}
	saveJournal := func(j registrationJournal) error {
		if destination == nil {
			return updateRegistrationJournal(journal, j)
		}
		data, err := json.Marshal(j)
		if err != nil {
			return err
		}
		return destination.WriteJournal(data)
	}
	stage := func(s string) error {
		return saveJournal(registrationJournal{Binding: b, Stage: s})
	}
	if e = stage("preflight"); e != nil {
		return empty, "", e
	}
	// Conflicting pre-existing items are never updated, even if their names match.
	var tokenRef, passwordRef config.SecretReference
	if store == "keychain" {
		tokenRef = config.SecretReference{Service: c.Store.AccessTokenService, Account: b.UserID}
		passwordRef = config.SecretReference{Service: c.Store.PasswordService, Account: b.UserID}
		for _, r := range []config.SecretReference{tokenRef, passwordRef} {
			if _, e = a.readConnectionSecret(ctx, p, r); !errors.Is(e, errKeychainMissing) {
				_ = stage("secret-preflight-refused")
				return empty, "", errors.New("selected credential item exists or is unavailable; no registration attempted")
			}
		}
	}
	var bootstrap string
	if admin == nil {
		bootstrap, e = a.readConnectionSecret(ctx, p, *c.RegistrationTokenRef)
		if e != nil || bootstrap == "" {
			_ = stage("bootstrap-unavailable")
			return empty, "", errors.New("selected bootstrap item unavailable; no registration attempted")
		}
		a.secrets.Add(bootstrap)
	}
	var raw [32]byte
	if _, e = rand.Read(raw[:]); e != nil {
		return empty, "", e
	}
	password := base64.RawURLEncoding.EncodeToString(raw[:])
	a.secrets.Add(password)
	// Only Keychain retains a recovery password. File passwords stay in memory;
	// the secret-free journal still fences every failed/ambiguous attempt.
	if store == "keychain" {
		if e = a.createConnectionSecretLocked(ctx, p, passwordRef, password); e != nil {
			_ = stage("password-store-failed")
			return empty, "", errors.New("recovery password could not be stored; no registration attempted")
		}
	}
	if e = ctx.Err(); e != nil {
		return empty, "", e
	}
	if e = stage("remote-outcome-pending"); e != nil {
		return empty, "", e
	}
	if admin != nil {
		cleanup := &adminRegistrationCleanup{session: admin}
		if destination != nil {
			cleanup.beforeIssue = func() error {
				if a.checkMemberSource(name) != nil || destination.Validate() != nil {
					return adminRegistrationError("destination", nil)
				}
				return nil
			}
		}
		defer cleanup.finish()
		bootstrap, e = a.issueAdminRegistrationToken(ctx, admin, cleanup)
		if e != nil {
			_ = stage("admin-issue-refused")
			return empty, "", e
		}
	}
	client := matrix.New(c.URL, "")
	client.HTTP = a.HTTP
	if admin != nil {
		client = client.ForAdminRegistration()
		client.BeforeRequest = admin.checkConnection
		if destination != nil {
			client.BeforeRequest = func() error {
				if err := admin.checkConnection(); err != nil {
					return err
				}
				if a.checkMemberSource(name) != nil || destination.Validate() != nil {
					return adminRegistrationError("destination", nil)
				}
				return nil
			}
		}
	}
	resp, e := client.Register(ctx, name, password, bootstrap, "a2a "+name)
	if e != nil {
		j := registrationJournal{Binding: b, Stage: "remote-outcome-ambiguous"}
		var me *matrix.Error
		if errors.As(e, &me) && me.Status >= 400 && me.Status < 500 {
			j.Stage = "remote-rejected"
			j.HTTPStatus = me.Status
			j.ErrCode = registrationRejectionCode(me.ErrCode)
		}
		if se := saveJournal(j); se != nil {
			return empty, "", errors.New("registration outcome pending; recovery journal update failed")
		}
		if admin != nil {
			if store == "file" {
				return empty, "", fmt.Errorf("%w; owner recovery required; file connection retained no recovery password", adminRegistrationError("register", e))
			}
			return empty, "", adminRegistrationError("register", e)
		}
		diagnostic := j.Stage
		if j.HTTPStatus != 0 {
			diagnostic += fmt.Sprintf(" (HTTP %d", j.HTTPStatus)
			if j.ErrCode != "" {
				diagnostic += "; " + j.ErrCode
			}
			diagnostic += ")"
		}
		return empty, "", fmt.Errorf("registration %s; owner recovery required (no automatic retry)", diagnostic)
	}
	a.secrets.Add(resp.AccessToken)
	if resp.UserID != b.UserID || resp.AccessToken == "" {
		_ = stage("remote-response-mismatch")
		return empty, "", errors.New("registration response mismatched selected identity; owner recovery required")
	}
	if e = stage("remote-created-token-storage-pending"); e != nil {
		return empty, "", errors.New("remote account created; recovery journal unavailable")
	}
	if store == "keychain" {
		if e = a.createConnectionSecretLocked(ctx, p, tokenRef, resp.AccessToken); e != nil {
			_ = stage("remote-created-token-store-failed")
			return empty, "", errors.New("remote account created but token storage failed; recovery password retained in selected store; owner recovery required")
		}
	}
	if e = stage("remote-created-metadata-publication-pending"); e != nil {
		if store == "file" {
			return empty, "", errors.New("remote account created; file token not published; journal update failed; owner recovery required (no automatic retry)")
		}
		return empty, "", errors.New("remote account and token stored; metadata publication pending")
	}
	creds := config.Credentials{UserID: b.UserID, DeviceID: resp.DeviceID, Homeserver: c.URL, ConnectionBinding: &b}
	if store == "file" {
		creds.AccessToken = resp.AccessToken
	}
	publicationError := func(message string) error {
		if destination != nil {
			return fmt.Errorf("%w; remote account created; member publication incomplete; no recovery password retained; owner recovery required", adminRegistrationError("member-publication", nil))
		}
		if store == "file" {
			return errors.New("remote account created; file credential publication incomplete; no recovery password retained; owner recovery required (no automatic retry)")
		}
		return errors.New(message)
	}
	if e = ctx.Err(); e != nil {
		return empty, "", publicationError("remote account and token stored; publication interrupted; owner recovery required")
	}
	if a.beforeNamedPublish != nil {
		if e = a.beforeNamedPublish(); e != nil {
			return empty, "", publicationError("remote account and token stored; publication interrupted; owner recovery required")
		}
	}
	if destination != nil {
		if ctx.Err() != nil || admin.checkConnection() != nil || a.checkMemberSource(name) != nil || destination.Validate() != nil {
			return empty, "", publicationError("member publication interrupted or binding changed")
		}
	}
	if e = guard.PublishBoundCredentials(creds); e != nil {
		return empty, "", publicationError("remote account and token stored; metadata publication pending; owner recovery required")
	}
	if destination != nil {
		if e = stage("remote-created-registry-publication-pending"); e != nil {
			return empty, "", publicationError("member publication pending")
		}
		if a.beforeMemberRegistry != nil {
			if e = a.beforeMemberRegistry(); e != nil {
				return empty, "", publicationError("member registry publication interrupted")
			}
		}
		if ctx.Err() != nil || admin.checkConnection() != nil || a.checkMemberSource(name) != nil || destination.Validate() != nil {
			return empty, "", publicationError("member registry publication interrupted or binding changed")
		}
		if e = destination.PublishRegistry(); e != nil {
			return empty, "", publicationError("member publication pending")
		}
		if e = destination.Verify(); e != nil {
			return empty, "", publicationError("member binding verification failed")
		}
	}
	if e = stage("complete"); e != nil {
		return empty, "", errors.New("credentials published; recovery journal completion pending")
	}
	if destination != nil && destination.Verify() != nil {
		return empty, "", publicationError("member root changed before completion")
	}
	// Keep complete evidence: existing metadata makes subsequent ensure reuse,
	// never re-register. A failed journal unlink cannot invite a duplicate attempt.
	creds.AccessToken = resp.AccessToken
	return creds, p.AgentFile(name), nil
}
