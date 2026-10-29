package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Owner reachability (#136): an ordinary owner message is untyped, so the
// kind baseline never delivers it. The operator lists its owner accounts in
// <config-root>/owners.json; subs sync and onboarding add one all-rooms
// sender subscription per owner, and subs check reports each owner as
// covered or missing. The list selects subscriptions only and grants no
// authority: owner verification keeps the existing full-principal rules.

const ownersUsage = "usage: a2a subs owners [list] [--json] | add @owner:server | remove @owner:server"

func cmdSubsOwners(_ context.Context, a *App, args []string) error {
	p, err := config.ResolvePaths(a.Getenv)
	if err != nil {
		return err
	}
	if len(args) > 0 && (args[0] == "add" || args[0] == "remove") {
		if len(args) != 2 || !config.ValidOwnerID(args[1]) {
			return usagef("%s (a full Matrix user ID, not a localpart or display name)", ownersUsage)
		}
		id := args[1]
		changed := false
		err := config.UpdateOwners(p, func(r *config.OwnerRegistry) error {
			i := slices.Index(r.Owners, id)
			switch {
			case args[0] == "add" && i < 0:
				if len(r.Owners) >= config.MaxOwners {
					return errors.New("owner registry is full")
				}
				r.Owners, changed = append(r.Owners, id), true
			case args[0] == "remove" && i >= 0:
				r.Owners, changed = slices.Delete(r.Owners, i, i+1), true
			}
			return nil
		})
		if err != nil {
			return err
		}
		state := "unchanged"
		if changed && args[0] == "add" {
			state = "added"
		} else if changed {
			state = "removed"
		}
		a.printf("%s %s\t%s\n", state, sanitizeLine(id), ownerProfile(localServerNames(a, p), id))
		if args[0] == "add" {
			a.printf("run a2a subs sync for each identity on this config root to subscribe it\n")
		}
		return nil
	}
	fs := newFlagSet("subs owners")
	asJSON := fs.Bool("json", false, "print the owner registry as one JSON object")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 || len(pos) == 1 && pos[0] != "list" {
		return usagef(ownersUsage)
	}
	r, err := config.LoadOwners(p)
	if err != nil {
		return err
	}
	owners := []string{}
	if r != nil {
		owners = r.Owners
	}
	if *asJSON {
		return a.printJSON(struct {
			Configured bool     `json:"configured"`
			Owners     []string `json:"owners"`
		}{len(owners) > 0, owners})
	}
	if len(owners) == 0 {
		a.printf("no owners configured (a2a subs owners add @owner:server)\n")
	}
	for _, o := range owners {
		a.printf("%s\n", sanitizeLine(o))
	}
	return nil
}

// ownerListFlag collects the repeatable --owner override: full Matrix user
// IDs only, no duplicates.
type ownerListFlag []string

func (o *ownerListFlag) String() string { return strings.Join(*o, ",") }

func (o *ownerListFlag) Set(v string) error {
	if !config.ValidOwnerID(v) {
		return fmt.Errorf("--owner needs a full Matrix user ID (@localpart:server)")
	}
	if slices.Contains(*o, v) {
		return fmt.Errorf("duplicate --owner %s", v)
	}
	*o = append(*o, v)
	return nil
}

// effectiveOwners returns the owner list for one invocation and its source.
// Explicit --owner values REPLACE the configured registry for that
// invocation only; owners.json is neither read nor changed then. Without
// them, owners.json applies; with neither, no list applies.
func effectiveOwners(p config.Paths, override []string) ([]string, string, error) {
	if len(override) > 0 {
		return override, "--owner", nil
	}
	r, err := config.LoadOwners(p)
	if err != nil || r == nil || len(r.Owners) == 0 {
		return nil, "", err
	}
	return r.Owners, "owners.json", nil
}

// localServerNames lists the Matrix server names this config root has a local
// profile for: matrix.env and every connections.json profile. An owner on any
// other server is a federated principal and is accepted all the same. This is
// a configuration fact, not proof of network reachability.
func localServerNames(a *App, p config.Paths) map[string]bool {
	names := map[string]bool{}
	if m, err := config.LoadMatrix(p, a.Getenv); err == nil && m.ServerName != "" {
		names[m.ServerName] = true
	}
	if r, err := config.LoadConnections(p); err == nil && r != nil {
		for _, c := range r.Profiles {
			names[c.ServerName] = true
		}
	}
	return names
}

// ownerProfile describes an owner's server relative to this config root.
func ownerProfile(local map[string]bool, owner string) string {
	if local[config.OwnerServer(owner)] {
		return "configured locally"
	}
	return "federated principal; no local profile"
}

// ownerSelector is the unrestricted all-rooms sender subscription that
// delivers every ordinary message from one owner account.
func ownerSelector(owner string) SubSelector { return SubSelector{From: owner} }

// ensureOwnerSubs adds the owner subscriptions for every configured owner,
// idempotently, and prints each one. An existing row is reused only when it
// already covers the owner (ownerRowLive); a product-, room- or kind-filtered,
// time-limited or not-yet-active row is left in place and a covering row is
// added beside it. Recorded room trims do not apply: they narrow room kinds,
// not senders.
func (s *session) ensureOwnerSubs(ctx context.Context, a *App, override []string) error {
	owners, source, err := effectiveOwners(s.paths, override)
	if err != nil {
		return err
	}
	if len(owners) == 0 {
		a.printf("owner subscriptions: none: no owners configured (a2a subs owners add @owner:server)\n")
		return nil
	}
	a.printf("owner subscriptions (from %s; routing only, they grant no authority):\n", source)
	return config.WithRuntimeLock(s.paths, s.name, "subscription-policy", time.Now().Add(nativeRPCTimeout), func() error {
		policy, err := s.subscriptionPolicy()
		if err != nil {
			return err
		}
		for _, owner := range owners {
			var sub Sub
			created := false
			_, _, err := s.updateSubsWithPolicy(ctx, policy, func(_ *config.State, list *SubList) (bool, error) {
				// Sample the clock here, after the current subscriptions were
				// read under the locks: a time taken earlier could predate a
				// covering row another repair has just written, make it look
				// future-created and add a duplicate.
				now := a.Now()
				for _, cur := range list.Subs {
					if ownerRowLive(cur, owner, now) {
						sub = cur
						return false, nil
					}
				}
				sub = Sub{ID: newSubID(*list), SubSelector: ownerSelector(owner), CreatedAt: now.UTC().Truncate(time.Millisecond).Format(createdAtLayout)}
				list.Subs = append(list.Subs, sub)
				created = true
				return true, nil
			})
			if err != nil {
				return err
			}
			state := "already subscribed"
			if created {
				state = "subscribed"
			}
			a.printf("%s %s\t%s\t%s\n", state, sub.ID, sanitizeLine(owner), sub.summary("all", s.server))
		}
		return nil
	})
}

type ownerCoverage struct {
	Owner   string `json:"owner"`
	Status  string `json:"status"`
	Profile string `json:"profile"`
}

// ownerRowLive reports whether sub is a live, unrestricted, permanent,
// all-rooms sender row for exactly owner. Kind, room, thread, subject,
// product, subtype, one-shot and time-limited rows do not deliver every
// owner message. The activity gate mirrors the consumer (Sub.after): the
// --since event when one is set, otherwise the creation time.
func ownerRowLive(sub Sub, owner string, now time.Time) bool {
	if sub.validate() != nil || sub.expiredAt(now) || sub.Until != "" || sub.From != owner || sub.Room != "" || sub.Topic != "" ||
		sub.Product != "" || sub.Subject != "" || len(sub.subtypeList()) > 0 || len(sub.Kinds) > 0 || sub.UntilKind != "" {
		return false
	}
	if sub.Since != "" {
		return sub.SinceTS <= now.UnixMilli()
	}
	created, err := time.Parse(time.RFC3339Nano, sub.CreatedAt)
	return err != nil || !created.After(now) // an unreadable creation time does not gate the consumer either
}

// ownerCovered reports whether list holds a row that covers owner.
func ownerCovered(list SubList, owner string, now time.Time) bool {
	return slices.ContainsFunc(list.Subs, func(sub Sub) bool { return ownerRowLive(sub, owner, now) })
}
