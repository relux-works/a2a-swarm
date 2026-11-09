package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func cardRoomUnavailable(server string, err error) error {
	return fmt.Errorf("card_room_unavailable for %s: %w", server, err)
}

// cardRoomRef uses only public routing metadata. It never selects a target
// profile's client, credentials, endpoint or secret store.
func (s *session) cardRoomRef(server string) (string, error) {
	if server == s.server {
		c, err := s.app.selectedConnection()
		if err != nil {
			return "", cardRoomUnavailable(server, err)
		}
		if c == nil {
			c = &config.Connection{ServerName: s.server}
		}
		return c.CardRoomRef()
	}
	r, err := config.LoadConnections(config.Paths{Base: s.paths.ConfigRoot()})
	if err != nil {
		return "", cardRoomUnavailable(server, err)
	}
	ref := ""
	if r != nil {
		for _, c := range r.Profiles {
			if c.ServerName != server {
				continue
			}
			next, err := c.CardRoomRef()
			if err != nil {
				return "", cardRoomUnavailable(server, err)
			}
			if ref != "" && ref != next {
				return "", cardRoomUnavailable(server, fmt.Errorf("ambiguous configured mapping"))
			}
			ref = next
		}
	}
	if ref == "" {
		return "", cardRoomUnavailable(server, fmt.Errorf("no configured mapping for target server"))
	}
	return ref, nil
}

// resolveCardRoom resolves exactly one configured reference through the caller's
// own homeserver. No alias fallback, discovery or implicit join is permitted.
func (s *session) resolveCardRoom(ctx context.Context, server string) (string, string, error) {
	ref, err := s.cardRoomRef(server)
	if err != nil {
		return "", "", err
	}
	label := s.shortAlias(ref)
	id := ref
	if strings.HasPrefix(ref, "#") {
		id, err = s.client.ResolveAlias(ctx, ref)
		if err != nil {
			return "", "", cardRoomUnavailable(server, err)
		}
	}
	if !strings.HasPrefix(id, "!") || len(id) < 2 || strings.ContainsAny(id, " \t\r\n\x00") {
		return "", "", cardRoomUnavailable(server, fmt.Errorf("configured room did not resolve to a room ID"))
	}
	return id, label, nil
}

func (s *session) readableCardRoom(ctx context.Context, server, override string) (string, string, error) {
	ref, err := s.cardRoomRef(server)
	if err != nil {
		return "", "", err
	}
	if override != "" {
		// The legacy flag is a consistency assertion, never an alternate store.
		norm, err := normalizeRoom(override, server)
		if err != nil {
			return "", "", err
		}
		if norm != ref {
			return "", "", usagef("--room must match the configured card_room for %s", server)
		}
	}
	id, label, err := s.resolveCardRoom(ctx, server)
	if err != nil {
		return "", "", err
	}
	// A missing room ID and a missing card can both return M_NOT_FOUND. Check
	// room access separately so absence/denial cannot masquerade as no card.
	var create map[string]any
	if err := s.client.GetState(ctx, id, "m.room.create", "", &create); err != nil {
		return "", "", cardRoomUnavailable(server, err)
	}
	return id, label, nil
}
