package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// availabilityRouting deliberately does not use selectedConnection/loadMatrix:
// those general paths may consult registration settings or credential metadata.
func (a *App) availabilityRouting() (config.Matrix, error) {
	p, err := config.ResolvePaths(a.Getenv)
	if err != nil {
		return config.Matrix{}, errors.New("availability: cannot resolve routing configuration")
	}
	r, err := config.LoadConnections(p)
	if err != nil {
		return config.Matrix{}, errors.New("availability: invalid connection registry")
	}
	legacy, err := config.LoadMatrixRouting(p, a.Getenv)
	if err != nil {
		return config.Matrix{}, errors.New("availability: cannot read public Matrix routing")
	}
	if r == nil {
		if a.profile != "" {
			return config.Matrix{}, usagef("availability: --profile requires connections.json")
		}
	} else {
		name := a.profile
		if name == "" {
			name = r.Current
		}
		c, ok := r.Profiles[name]
		if !ok {
			return config.Matrix{}, usagef("availability: unknown connection profile")
		}
		if legacy.URL != "" && strings.TrimRight(legacy.URL, "/") != strings.TrimRight(c.URL, "/") || legacy.ServerName != "" && legacy.ServerName != c.ServerName {
			return config.Matrix{}, errors.New("availability: legacy routing conflicts with selected profile")
		}
		legacy = config.Matrix{URL: c.URL, ServerName: c.ServerName}
	}
	if err := config.ValidateMatrixRouting(legacy.URL, legacy.ServerName); err != nil {
		return config.Matrix{}, errors.New("availability: invalid or incomplete selected routing")
	}
	return legacy, nil
}

// availabilityIdentity admits only the modern Matrix registration localpart
// alphabet. Historical/other identifier classes remain unsupported here; this
// does not change global identity handling or lowercase an input.
func availabilityIdentity(input, server string) (string, string, error) {
	local := input
	if strings.HasPrefix(input, "@") {
		var target string
		var ok bool
		local, target, ok = strings.Cut(input[1:], ":")
		if !ok || target != server {
			return "", "", usagef("availability: full MXID must match the exact selected server spelling")
		}
	}
	if local == "" || len("@"+local+":"+server) > 255 {
		return "", "", usagef("availability: empty or overlong Matrix username")
	}
	for _, r := range local {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("._=-/+", r)) {
			return "", "", usagef("availability: unsupported username syntax; use an exact lowercase Matrix registration localpart or full MXID")
		}
	}
	return local, "@" + local + ":" + server, nil
}

func cmdAvailability(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("availability")
	asJSON := fs.Bool("json", false, "print a typed availability observation")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: a2a availability <localpart|@localpart:server> [--json]")
	}
	if a.human || a.connectionFingerprint != "" {
		return usagef("availability: human and authenticated connection fingerprints are unsupported for an anonymous read")
	}
	mx, err := a.availabilityRouting()
	if err != nil {
		return err
	}
	username, userID, err := availabilityIdentity(pos[0], mx.ServerName)
	if err != nil {
		return err
	}
	client := matrix.New(mx.URL, "")
	client.HTTP = a.HTTP
	observation, queryErr := client.Availability(ctx, username)
	if *asJSON {
		out := struct {
			matrix.AvailabilityResult
			UserID        string `json:"user_id"`
			ServerName    string `json:"server_name"`
			HomeserverURL string `json:"homeserver_url"`
		}{observation, userID, mx.ServerName, mx.URL}
		if err := json.NewEncoder(a.Stdout).Encode(out); err != nil {
			return errors.New("availability: cannot write observation")
		}
	} else {
		line := string(observation.Outcome) + "\t" + userID
		if observation.Reason != "" {
			line += "\t" + observation.Reason
		}
		if _, err := fmt.Fprintln(a.Stdout, line); err != nil {
			return errors.New("availability: cannot write observation")
		}
	}
	return queryErr
}
