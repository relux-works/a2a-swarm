package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// cmdRegister: a2a register <name>
func cmdRegister(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("register")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: a2a register <name>")
	}
	name := pos[0]
	if err := config.ValidateName(name); err != nil {
		return usageError{err.Error()}
	}
	p, mx, err := a.loadMatrix()
	if err != nil {
		return err
	}
	creds, path, err := a.registerAgent(ctx, p, mx, name, nil)
	if err != nil {
		return err
	}
	a.printf("registered %s (device %s)\ncredentials: %s (0600)\n", creds.UserID, creds.DeviceID, path)
	return nil
}

// registerAgent creates the account name and writes agents/<name>.json. The
// credential file is reserved (O_EXCL, 0600) before any network call; check,
// when set, runs after the reservation and before the registration (an error
// removes the reservation and creates nothing).
func (a *App) registerAgent(ctx context.Context, p config.Paths, mx config.Matrix, name string, check func(*matrix.Client) error) (_ config.Credentials, path string, err error) {
	store, err := a.selectedStore()
	if err != nil {
		return config.Credentials{}, "", err
	}
	if mx.URL == "" {
		return config.Credentials{}, "", fmt.Errorf("no homeserver URL: set %s in %s", config.EnvMatrixURL, p.EnvFile())
	}
	if _, err := os.Lstat(p.HumanFile(name)); err == nil {
		return config.Credentials{}, "", fmt.Errorf("%q is a human's identity; register your own agent name instead", name)
	}
	// Reserve the credential file first: an existing file is never overwritten,
	// and no account is created when the file cannot be written.
	f, err := config.CreateCredentialFile(p, name)
	if err != nil {
		return config.Credentials{}, "", err
	}
	reserved := p.AgentFile(name)
	path = reserved
	written := false
	defer func() {
		if !written {
			f.Close()
			os.Remove(reserved)
		}
	}()

	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return config.Credentials{}, "", err
	}
	password := base64.RawURLEncoding.EncodeToString(raw[:])
	a.secrets.Add(password)

	c := matrix.New(mx.URL, "")
	c.HTTP = a.HTTP
	if check != nil {
		if err := check(c); err != nil {
			return config.Credentials{}, "", err
		}
	}
	resp, err := c.Register(ctx, name, password, mx.RegistrationToken, "a2a "+name)
	if err != nil {
		if matrix.IsCode(err, "M_USER_IN_USE") {
			return config.Credentials{}, "", fmt.Errorf("register %s: the user already exists on the homeserver", name)
		}
		return config.Credentials{}, "", fmt.Errorf("register %s: %w", name, err)
	}
	a.secrets.Add(resp.AccessToken)
	if resp.AccessToken == "" || resp.UserID == "" {
		return config.Credentials{}, "", errors.New("register: homeserver returned no access token")
	}
	creds := config.Credentials{
		UserID:      resp.UserID,
		AccessToken: resp.AccessToken,
		DeviceID:    resp.DeviceID,
		Homeserver:  mx.URL,
	}
	if err := validateCredentialIdentity(creds, name); err != nil {
		return config.Credentials{}, "", err
	}
	fileCreds := creds
	if store == "keychain" {
		if err := a.keychainStore(p).write(ctx, creds.UserID, creds.AccessToken); err != nil {
			return config.Credentials{}, "", err
		}
		fileCreds.AccessToken = ""
	}
	if err := config.WriteCredentials(f, fileCreds); err != nil {
		return config.Credentials{}, "", fmt.Errorf("write %s: %w", path, err)
	}
	written = true
	return creds, path, nil
}

// cmdWhoami: a2a whoami
func cmdWhoami(ctx context.Context, a *App, args []string) error {
	if len(args) != 0 {
		return usagef("usage: a2a whoami")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	w, err := s.client.Whoami(ctx)
	if err != nil {
		return err
	}
	a.printf("agent: %s\nuser: %s\ndevice: %s\nhomeserver: %s\n", s.name, w.UserID, w.DeviceID, s.client.BaseURL)
	return nil
}

// cmdRooms: a2a rooms
func cmdRooms(ctx context.Context, a *App, args []string) error {
	if len(args) != 0 {
		return usagef("usage: a2a rooms")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	ids, err := s.client.JoinedRooms(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		alias := s.roomLabel(ctx, id, nil)
		if alias == id {
			alias = "-"
		}
		var n struct {
			Name string `json:"name"`
		}
		name := "-"
		if err := s.client.GetState(ctx, id, "m.room.name", "", &n); err == nil && n.Name != "" {
			name = n.Name
		}
		a.printf("%s\t%s\t%s\n", sanitizeLine(alias), sanitizeLine(name), id)
	}
	return nil
}

// cmdJoin: a2a join <room>
func cmdJoin(ctx context.Context, a *App, args []string) error {
	if len(args) != 1 {
		return usagef("usage: a2a join <room>")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	id, label, err := s.resolveRoom(ctx, args[0])
	if err != nil {
		return err
	}
	if _, err := s.client.Join(ctx, id); err != nil {
		if matrix.IsCode(err, "M_FORBIDDEN") {
			return fmt.Errorf("join %s: forbidden (the room is invite-only; ask a member: a2a invite %s %s)", label, label, s.name)
		}
		return fmt.Errorf("join %s: %w", label, err)
	}
	a.printf("joined %s %s\n", label, id)
	return s.subscribeRoom(ctx, a, id, label, a.Now())
}

// cmdInvite: a2a invite <room> <@user|name>
func cmdInvite(ctx context.Context, a *App, args []string) error {
	if len(args) != 2 {
		return usagef("usage: a2a invite <room> <@user|name>")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	id, label, err := s.resolveRoom(ctx, args[0])
	if err != nil {
		return err
	}
	user, err := normalizeUser(args[1], s.server)
	if err != nil {
		return err
	}
	if err := s.client.Invite(ctx, id, user); err != nil {
		return fmt.Errorf("invite %s to %s: %w", user, label, err)
	}
	a.printf("invited %s to %s\n", user, label)
	return nil
}

// cmdLeave: a2a leave <room> [--force]
// #lobby carries the agent cards, so leaving it needs --force.
func cmdLeave(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("leave")
	force := fs.Bool("force", false, "also leave #lobby")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: a2a leave <room> [--force]")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	norm, err := normalizeRoom(pos[0], s.server)
	if err != nil {
		return err
	}
	lobby, _ := normalizeRoom(LobbyRoom, s.server)
	refuse := usagef("refusing to leave %s: it carries the agent cards; pass --force to leave it anyway", LobbyRoom)
	if !*force && norm == lobby {
		return refuse
	}
	id, label, err := s.resolveRoom(ctx, pos[0])
	if err != nil {
		return err
	}
	if strings.HasPrefix(norm, "!") {
		if !*force {
			// A room id argument may still be the lobby.
			if lobbyID, err := s.client.ResolveAlias(ctx, lobby); err == nil && lobbyID == id {
				return refuse
			}
		}
		label = s.roomLabel(ctx, id, nil)
	}
	if err := s.client.Leave(ctx, id); err != nil {
		return fmt.Errorf("leave %s: %w", label, err)
	}
	a.printf("left %s %s\n", sanitizeLine(label), id)
	return s.unsubscribeRoom(ctx, a, id, sanitizeLine(label))
}

// cmdCreds: a2a creds move-human <name>
func cmdCreds(ctx context.Context, a *App, args []string) error {
	if len(args) > 0 && args[0] == "migrate" {
		return a.migrateCredentials(ctx, args[1:])
	}
	if len(args) != 2 || args[0] != "move-human" {
		return usagef("usage: a2a creds move-human <name>")
	}
	if err := config.ValidateName(args[1]); err != nil {
		return usageError{err.Error()}
	}
	p, err := a.paths()
	if err != nil {
		return err
	}
	dst, err := config.MoveToHumans(p, args[1])
	if err != nil {
		return err
	}
	a.printf("moved %s to %s; use it only as that human: a2a --human --as %s …\n", args[1], dst, args[1])
	return nil
}
