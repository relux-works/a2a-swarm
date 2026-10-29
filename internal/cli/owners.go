package cli

import (
	"context"
	"errors"
	"slices"
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
		a.printf("%s %s\n", state, sanitizeLine(id))
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

// ownerSelector is the unrestricted all-rooms sender subscription that
// delivers every ordinary message from one owner account.
func ownerSelector(owner string) SubSelector { return SubSelector{From: owner} }

// ensureOwnerSubs adds the owner subscriptions for every configured owner,
// idempotently, and prints each one. An existing row is reused only when it
// already covers the owner (ownerRowLive); a product-, room- or kind-filtered,
// time-limited or not-yet-active row is left in place and a covering row is
// added beside it. Recorded room trims do not apply: they narrow room kinds,
// not senders.
func (s *session) ensureOwnerSubs(ctx context.Context, a *App) error {
	r, err := config.LoadOwners(s.paths)
	if err != nil {
		return err
	}
	a.printf("owner subscriptions:\n")
	if r == nil || len(r.Owners) == 0 {
		a.printf("none: no owners configured (a2a subs owners add @owner:server)\n")
		return nil
	}
	return config.WithRuntimeLock(s.paths, s.name, "subscription-policy", time.Now().Add(nativeRPCTimeout), func() error {
		policy, err := s.subscriptionPolicy()
		if err != nil {
			return err
		}
		for _, owner := range r.Owners {
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
			a.printf("%s %s\t%s\n", state, sub.ID, sub.summary("all", s.server))
		}
		return nil
	})
}

type ownerCoverage struct {
	Owner  string `json:"owner"`
	Status string `json:"status"`
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
