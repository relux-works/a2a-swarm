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
func cmdRegister(ctx context.Context, a *App, args []string) (err error) {
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
	if mx.URL == "" {
		return fmt.Errorf("no homeserver URL: set %s in %s", config.EnvMatrixURL, p.EnvFile())
	}
	// Reserve the credential file first: an existing file is never overwritten,
	// and no account is created when the file cannot be written.
	f, err := config.CreateCredentialFile(p, name)
	if err != nil {
		return err
	}
	path := p.AgentFile(name)
	written := false
	defer func() {
		if !written {
			f.Close()
			os.Remove(path)
		}
	}()

	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	password := base64.RawURLEncoding.EncodeToString(raw[:])
	a.secrets.Add(password)

	c := matrix.New(mx.URL, "")
	c.HTTP = a.HTTP
	resp, err := c.Register(ctx, name, password, mx.RegistrationToken, "a2a "+name)
	if err != nil {
		if matrix.IsCode(err, "M_USER_IN_USE") {
			return fmt.Errorf("register %s: the user already exists on the homeserver", name)
		}
		return fmt.Errorf("register %s: %w", name, err)
	}
	a.secrets.Add(resp.AccessToken)
	if resp.AccessToken == "" || resp.UserID == "" {
		return errors.New("register: homeserver returned no access token")
	}
	creds := config.Credentials{
		UserID:      resp.UserID,
		AccessToken: resp.AccessToken,
		DeviceID:    resp.DeviceID,
		Password:    password,
		Homeserver:  mx.URL,
	}
	if err := config.WriteCredentials(f, creds); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	written = true
	a.printf("registered %s (device %s)\ncredentials: %s (0600)\n", resp.UserID, resp.DeviceID, path)
	return nil
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
	return nil
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
	return nil
}
