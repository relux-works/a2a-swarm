package cli

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func cmdIdentityPresence(a *App, args []string) error {
	// ACTOR remains the existing real caller; TARGET is only a public selector.
	// This convention does not authenticate external host/operator admission.
	if a.asSet || a.as != "" || a.human || config.ValidateName(a.Getenv(config.EnvAgent)) != nil {
		return usagef("identity-presence requires the real A2A_AGENT caller and forbids --as and --human")
	}
	fs := newFlagSet("identity-presence")
	target := fs.String("target-agent", "", "explicit agent target (not caller identity)")
	jsonOutput := fs.Bool("json", false, "versioned local presence result")
	seen := map[string]bool{}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			key := strings.SplitN(strings.TrimLeft(arg, "-"), "=", 2)[0]
			if seen[key] {
				return usagef("identity-presence requires unambiguous flags")
			}
			seen[key] = true
		}
	}
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 0 || !*jsonOutput || config.ValidateName(*target) != nil {
		return usagef("usage: a2a [--profile NAME] connection identity-presence --target-agent NAME --json")
	}
	p, err := config.ResolvePaths(a.Getenv)
	result := config.UnavailableAgentPresence()
	if err == nil {
		result = config.InspectAgentPresence(p, *target, a.profile, a.connectionFingerprint, a.Getenv)
	}
	if err := json.NewEncoder(a.Stdout).Encode(result); err != nil {
		return errors.New("local presence output unavailable")
	}
	if result.State == "inconclusive" {
		return errors.New("local presence inconclusive")
	}
	return nil
}
