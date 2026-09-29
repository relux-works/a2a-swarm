package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Re-login on a rejected access token.
//
// Safety design: a command is retried only if every HTTP request it sent with
// the session's token was answered 401 by the homeserver. A 401 means the
// server refused the request before executing it, so nothing was mutated and
// running the whole command again cannot duplicate a post, an invite or a
// state change. Every other outcome (any non-401 status, or a transport error
// where the request might have reached the server) counts as "possibly
// executed" and disables the retry: the command then fails with the plain
// authentication error, exactly as before. The retry happens at most once, and
// stdin consumed by the first attempt is replayed to the second.

// probe counts requests that may have been executed by the server.
type probe struct {
	base http.RoundTripper
	app  *App
}

func (p probe) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := p.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		p.app.served.Add(1)
	}
	return resp, err
}

// probedHTTP returns a copy of a.HTTP whose transport feeds a.served.
func (a *App) probedHTTP() *http.Client {
	hc := *a.HTTP
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	hc.Transport = probe{base: base, app: a}
	return &hc
}

// runWithRelogin runs one command and, when it failed only because the stored
// access token was rejected, logs in again with the stored password, rewrites
// the credential file, and runs the command a second (and last) time.
func (a *App) runWithRelogin(ctx context.Context, c command, args []string) error {
	orig := a.Stdin
	var consumed bytes.Buffer
	if orig != nil {
		a.Stdin = io.TeeReader(orig, &consumed)
	}
	a.sess = nil
	a.served.Store(0)
	err := c.run(ctx, a, args)
	// Human accounts are never re-logged in by the tool (their file lives in humans/).
	if err == nil || a.human || !errors.Is(err, matrix.ErrAuth) || a.sess == nil || a.served.Load() != 0 {
		return err
	}
	if lerr := a.relogin(ctx, a.sess, err); lerr != nil {
		return lerr
	}
	if orig != nil {
		a.Stdin = io.MultiReader(bytes.NewReader(consumed.Bytes()), orig)
	}
	a.sess = nil
	return c.run(ctx, a, args)
}

// relogin replaces the rejected token of s with a fresh one from
// m.login.password, keeping the device id, under the credential file lock.
// If another process already replaced the token, its token is used as is.
func (a *App) relogin(ctx context.Context, s *session, cause error) error {
	path := s.paths.AgentFile(s.name)
	rejected := s.creds.AccessToken
	loginFailed := func(why string) error {
		return fmt.Errorf("authentication failed: %v; re-login of %s %s; %s cannot be repaired automatically: re-registration or an admin password reset is needed", cause, s.name, why, path)
	}
	c := matrix.New(s.client.BaseURL, "")
	c.HTTP = a.HTTP
	loggedIn := false
	creds, err := config.UpdateCredentials(s.paths, s.name, func(cr *config.Credentials) error {
		a.secrets.Add(cr.AccessToken, cr.Password)
		if cr.AccessToken != rejected {
			return config.ErrUnchanged // another process already logged in again
		}
		if cr.Password == "" {
			return loginFailed("is impossible (no stored password)")
		}
		resp, err := c.Login(ctx, cr.UserID, cr.Password, cr.DeviceID)
		a.secrets.Add(resp.AccessToken)
		if err != nil {
			return loginFailed(fmt.Sprintf("with the stored password failed (%v)", err))
		}
		if resp.AccessToken == "" {
			return loginFailed("returned no access token")
		}
		cr.AccessToken = resp.AccessToken
		if resp.DeviceID != "" {
			cr.DeviceID = resp.DeviceID
		}
		loggedIn = true
		return nil
	})
	if err != nil {
		return err
	}
	if loggedIn {
		fmt.Fprintf(a.Stderr, "a2a: access token for %s was rejected; logged in again (device %s) and updated %s\n", s.name, sanitizeLine(creds.DeviceID), path)
	} else {
		fmt.Fprintf(a.Stderr, "a2a: access token for %s was rejected; %s already holds a newer token, retrying with it\n", s.name, path)
	}
	return nil
}
