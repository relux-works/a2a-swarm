package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Private in-process seam: no environment/flag can select a fake secret store.
type connectionSecretStore interface {
	Read(context.Context, config.Paths, config.SecretReference) (string, error)
	Create(context.Context, config.Paths, config.SecretReference, string) error
}

func (a *App) readConnectionSecret(ctx context.Context, p config.Paths, r config.SecretReference) (string, error) {
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
	if a.connectionSecretStore != nil {
		return a.connectionSecretStore.Create(ctx, p, r, v)
	}
	k := a.keychainStore(p)
	k.service = r.Service
	k.createOnly = true
	return k.write(ctx, r.Account, v)
}

// RegistrationJournal is private recovery evidence, without secret values.
// A retained journal always blocks a fresh attempt. Recovery is an explicit
// owner operation; source onboarding never retries an ambiguous remote creation.
type registrationEvent struct {
	Stage   string          `json:"stage"`
	Failure *secretEvidence `json:"failure,omitempty"`
}
type registrationJournal struct {
	Binding      config.ConnectionBinding `json:"binding"`
	Stage        string                   `json:"stage"`
	Version      int                      `json:"version,omitempty"`
	Attempt      string                   `json:"attempt,omitempty"`
	History      []registrationEvent      `json:"history,omitempty"`
	LegacySHA256 string                   `json:"legacy_sha256,omitempty"`
	sourceSHA256 string                   // Digest of the exact bytes read; never serialized as history.
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
	if e = os.Rename(tmp, path); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func (a *App) registerNamedAgent(ctx context.Context, name string) (config.Credentials, string, error) {
	empty := config.Credentials{}
	c, e := a.selectedConnection()
	if e != nil {
		return empty, "", e
	}
	if a.human {
		return empty, "", errors.New("agent registration cannot use a human identity")
	}
	if !a.authorizeRegistration {
		return empty, "", errors.New("named first registration requires explicit --authorize-registration after owner admission")
	}
	// Retain the existing caller identity guard even for this first-registration path.
	for _, bound := range []string{a.as, a.Getenv(config.EnvAgent)} {
		if bound != "" && bound != name {
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
	if c.RegistrationTokenRef == nil {
		return empty, "", errors.New("selected profile has no registration_token_ref")
	}
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
	journal := filepath.Join(p.Base, "registration.pending.json")
	f, e := os.OpenFile(journal, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return empty, "", errors.New("registration already pending; owner recovery required")
	}
	f.Close()
	unlock, e := lockRegistration(filepath.Join(p.Base, "registration.lock"), true)
	if e != nil {
		return empty, "", errors.New("registration ownership unavailable")
	}
	defer unlock()
	var id [16]byte
	if _, e = rand.Read(id[:]); e != nil {
		return empty, "", e
	}
	j := registrationJournal{Binding: b, Version: 1, Attempt: hex.EncodeToString(id[:])}
	if e = j.record(journal, "preflight", nil); e != nil {
		return empty, "", e
	}
	return a.finishNamedRegistration(ctx, name, p, c, &j, journal)
}

func (j *registrationJournal) record(path, stage string, failure *secretEvidence) error {
	j.Stage = stage
	j.History = append(j.History, registrationEvent{Stage: stage, Failure: failure})
	return updateRegistrationJournal(path, *j)
}

func (a *App) finishNamedRegistration(ctx context.Context, name string, p config.Paths, c *config.Connection, j *registrationJournal, journal string) (config.Credentials, string, error) {
	empty := config.Credentials{}
	var e error
	b := j.Binding
	stage := func(s string) error { return j.record(journal, s, nil) }

	// Conflicting pre-existing items are never updated, even if their names match.
	tokenRef := config.SecretReference{Service: c.Store.AccessTokenService, Account: b.UserID}
	passwordRef := config.SecretReference{Service: c.Store.PasswordService, Account: b.UserID}
	for _, r := range []config.SecretReference{tokenRef, passwordRef} {
		if _, e = a.readConnectionSecret(ctx, p, r); !errors.Is(e, errKeychainMissing) {
			failure := registrationFailureEvidence(e, false)
			if (j.Stage == "continuation-admitted" || j.Stage == "legacy-continuation-admitted") && registrationCooldownEvidence(&failure) {
				if se := j.record(journal, "secret-preflight-cooldown", &failure); se != nil {
					return empty, "", errors.New("credential cooldown active; recovery journal update failed")
				}
				return empty, "", errors.New("selected credential cooldown active; no registration attempted")
			}
			_ = stage("secret-preflight-refused")
			return empty, "", errors.New("selected credential item exists or is unavailable; no registration attempted")
		}
	}
	bootstrap, e := a.readConnectionSecret(ctx, p, *c.RegistrationTokenRef)
	if e != nil || bootstrap == "" {
		failure := registrationFailureEvidence(e, bootstrap == "")
		if se := j.record(journal, "bootstrap-unavailable", &failure); se != nil {
			return empty, "", errors.New("bootstrap unavailable; recovery journal update failed")
		}
		return empty, "", errors.New("selected bootstrap item unavailable; no registration attempted")
	}
	a.secrets.Add(bootstrap)
	// This durable boundary precedes every password/HTTP effect, including interruption.
	if e = stage("password-storage-pending"); e != nil {
		return empty, "", e
	}
	var raw [32]byte
	if _, e = rand.Read(raw[:]); e != nil {
		return empty, "", e
	}
	password := base64.RawURLEncoding.EncodeToString(raw[:])
	a.secrets.Add(password)
	// Store the intended recovery password before a remote account can exist.
	if e = a.createConnectionSecret(ctx, p, passwordRef, password); e != nil {
		_ = stage("password-store-failed")
		return empty, "", errors.New("recovery password could not be stored; no registration attempted")
	}
	if e = ctx.Err(); e != nil {
		return empty, "", e
	}
	if e = stage("remote-outcome-pending"); e != nil {
		return empty, "", e
	}
	client := matrix.New(c.URL, "")
	client.HTTP = a.HTTP
	resp, e := client.Register(ctx, name, password, bootstrap, "a2a "+name)
	if e != nil {
		state := "remote-outcome-ambiguous"
		var me *matrix.Error
		if errors.As(e, &me) && me.Status >= 400 && me.Status < 500 {
			state = "remote-rejected"
		}
		if se := stage(state); se != nil {
			return empty, "", errors.New("registration outcome pending; recovery journal update failed")
		}
		return empty, "", fmt.Errorf("registration %s; owner recovery required (no automatic retry)", state)
	}
	a.secrets.Add(resp.AccessToken)
	if resp.UserID != b.UserID || resp.AccessToken == "" {
		_ = stage("remote-response-mismatch")
		return empty, "", errors.New("registration response mismatched selected identity; owner recovery required")
	}
	if e = stage("remote-created-token-storage-pending"); e != nil {
		return empty, "", errors.New("remote account created; recovery journal unavailable")
	}
	if e = a.createConnectionSecret(ctx, p, tokenRef, resp.AccessToken); e != nil {
		_ = stage("remote-created-token-store-failed")
		return empty, "", errors.New("remote account created but token storage failed; recovery password retained in selected store; owner recovery required")
	}
	if e = stage("remote-created-metadata-publication-pending"); e != nil {
		return empty, "", errors.New("remote account and token stored; metadata publication pending")
	}
	creds := config.Credentials{UserID: b.UserID, DeviceID: resp.DeviceID, Homeserver: c.URL, ConnectionBinding: &b}
	if e = ctx.Err(); e != nil {
		return empty, "", errors.New("remote account and token stored; publication interrupted; owner recovery required")
	}
	if a.beforeNamedPublish != nil {
		if e = a.beforeNamedPublish(); e != nil {
			return empty, "", errors.New("remote account and token stored; publication interrupted; owner recovery required")
		}
	}
	if e = config.PublishBoundCredentials(p, name, false, creds); e != nil {
		return empty, "", errors.New("remote account and token stored; metadata publication pending; owner recovery required")
	}
	if e = stage("complete"); e != nil {
		return empty, "", errors.New("credentials published; recovery journal completion pending")
	}
	// Keep complete evidence: existing metadata makes subsequent ensure reuse,
	// never re-register. A failed journal unlink cannot invite a duplicate attempt.
	creds.AccessToken = resp.AccessToken
	return creds, p.AgentFile(name), nil
}
