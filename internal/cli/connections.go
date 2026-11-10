package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// selectedConnection is resolved once per invocation; children pin only its
// operational binding, never a hash of the whole mutable registry.
func (a *App) selectedConnection() (*config.Connection, error) {
	if a.connectionResolved {
		return a.connection, a.connectionError
	}
	a.connectionResolved = true
	p, e := config.ResolvePaths(a.Getenv)
	if e != nil {
		a.connectionError = e
		return nil, e
	}
	r, e := config.LoadConnections(p)
	if e != nil {
		a.connectionError = e
		return nil, e
	}
	if r == nil {
		if a.profile != "" || a.connectionFingerprint != "" {
			a.connectionError = errors.New("--profile requires connections.json")
		}
		return nil, a.connectionError
	}
	name := a.profile
	if name == "" {
		name = r.Current
	}
	c, ok := r.Profiles[name]
	if !ok {
		a.connectionError = errors.New("unknown connection profile")
		return nil, a.connectionError
	}
	// Read legacy routing settings only. An inherited registration token is never
	// a fallback or part of named selection.
	legacy, e := config.LoadMatrix(p, func(k string) string {
		if k == config.EnvRegistrationToken {
			return ""
		}
		return a.Getenv(k)
	})
	if e != nil {
		a.connectionError = e
		return nil, e
	}
	if legacy.URL != "" && strings.TrimRight(legacy.URL, "/") != strings.TrimRight(c.URL, "/") || legacy.ServerName != "" && legacy.ServerName != c.ServerName {
		a.connectionError = errors.New("legacy Matrix routing conflicts with selected profile")
		return nil, a.connectionError
	}
	if v := a.Getenv(envSecretStore); v != "" && v != c.Store.Backend {
		a.connectionError = errors.New("A2A_SECRET_STORE conflicts with selected profile")
		return nil, a.connectionError
	}
	a.profile = name
	a.connection = &c
	return &c, nil
}
func (a *App) pathsFor(name string) (result config.Paths, err error) {
	defer func() {
		if a.daemonEngine {
			result.DaemonOwner = a.daemonOwner
		}
	}()
	p, e := config.ResolvePaths(a.Getenv)
	if e != nil {
		return p, e
	}
	c, e := a.selectedConnection()
	if e != nil || c == nil {
		return p, e
	}
	if name != "" {
		b, e := c.Binding(name, a.human)
		if e != nil {
			return p, e
		}
		if a.connectionFingerprint != "" && a.connectionFingerprint != b.Fingerprint() {
			return p, errors.New("selected connection fingerprint changed; explicit migration required")
		}
	}
	return c.Paths(p, name, a.human)
}
func (a *App) namedMetadata(p config.Paths, name string) (config.Credentials, error) {
	c, e := a.selectedConnection()
	if e != nil {
		return config.Credentials{}, e
	}
	b, e := c.Binding(name, a.human)
	if e != nil {
		return config.Credentials{}, e
	}
	if !a.human {
		if _, err := os.Lstat(p.HumanFile(name)); err == nil || !errors.Is(err, os.ErrNotExist) {
			return config.Credentials{}, errors.New("selected name is a human identity or cannot be inspected; agent use refused")
		}
	}
	creds, e := config.LoadCredentialMetadata(p, name, a.human)
	if e != nil {
		return creds, e
	}
	// Unlike legacy decoding, named metadata also refuses unknown/duplicate keys.
	path := p.AgentFile(name)
	if a.human {
		path = p.HumanFile(name)
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		return creds, e
	}
	if e = config.DecodeConnectionJSON(raw, &creds); e != nil {
		return config.Credentials{}, errors.New("invalid named credential metadata")
	}
	if e = config.ValidateBoundCredentials(creds, b); e != nil {
		return config.Credentials{}, e
	}
	return creds, nil
}
func (a *App) connectionPreflight(command string, args []string) error {
	c, e := a.selectedConnection()
	if e != nil || c == nil {
		return e
	}
	// Public management and host metadata do not need a credential backend.
	if command == "connection" || command == "host" || command == "help" {
		return nil
	}
	if c.Store.Backend == "keychain" && runtime.GOOS != "darwin" && a.keychain == nil && a.connectionSecretStore == nil {
		return errors.New("keychain secret store requires macOS")
	}
	if command == "register" || command == "identity" && len(args) > 0 && args[0] == "ensure" {
		return nil
	}
	name, e := a.identity()
	if e != nil {
		return e
	}
	p, e := a.pathsFor(name)
	if e != nil {
		return e
	}
	_, e = a.namedMetadata(p, name)
	return e
}
func (a *App) pinnedArgs(name string) ([]string, error) {
	c, e := a.selectedConnection()
	if e != nil || c == nil {
		return nil, e
	}
	b, e := c.Binding(name, a.human)
	if e != nil {
		return nil, e
	}
	return []string{"--profile", a.profile, "--connection-fingerprint", b.Fingerprint()}, nil
}
func cmdConnection(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 {
		return usagef("usage: a2a connection list|show NAME|add NAME --file FILE|use NAME|register NAME --via-admin-room")
	}
	p, e := config.ResolvePaths(a.Getenv)
	if e != nil {
		return e
	}
	switch args[0] {
	case "register":
		return cmdAdminRoomRegister(ctx, a, args[1:])
	case "list", "show":
		r, e := config.LoadConnections(p)
		if e != nil {
			return e
		}
		if r == nil {
			return errors.New("no connection registry")
		}
		if args[0] == "show" {
			if len(args) != 2 {
				return usagef("connection show NAME")
			}
			c, ok := r.Profiles[args[1]]
			if !ok {
				return errors.New("unknown connection profile")
			}
			return json.NewEncoder(a.Stdout).Encode(c)
		}
		if len(args) != 1 {
			return usagef("connection list")
		}
		names := make([]string, 0, len(r.Profiles))
		for n := range r.Profiles {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			mark := ""
			if n == r.Current {
				mark = "current"
			}
			a.printf("%s\t%s\t%s\t%s\n", n, r.Profiles[n].ID, r.Profiles[n].URL, mark)
		}
		return nil
	case "use":
		if len(args) != 2 {
			return usagef("connection use NAME")
		}
		return config.UpdateConnections(p, func(r *config.ConnectionRegistry) error {
			if _, ok := r.Profiles[args[1]]; !ok {
				return errors.New("unknown connection profile")
			}
			r.Current = args[1]
			return nil
		})
	case "add":
		fs := newFlagSet("connection add")
		file := fs.String("file", "", "public connection entry JSON")
		pos, e := parseInterspersed(fs, args[1:])
		if e != nil {
			return e
		}
		if len(pos) != 1 || *file == "" {
			return usagef("connection add NAME --file FILE")
		}
		data, e := os.ReadFile(*file)
		if e != nil {
			return e
		}
		if len(data) > 16384 {
			return errors.New("connection entry too large")
		}
		var c config.Connection
		if e = config.DecodeConnectionJSON(data, &c); e != nil {
			return e
		}
		if e = c.Validate(); e != nil {
			return e
		}
		return config.UpdateConnections(p, func(r *config.ConnectionRegistry) error {
			if _, ok := r.Profiles[pos[0]]; ok {
				return errors.New("profile already exists; replacement refused")
			}
			r.Profiles[pos[0]] = c
			return nil
		})
	}
	return fmt.Errorf("unknown connection command")
}
