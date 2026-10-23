package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func (s *session) subscriptionPolicy() (config.SubscriptionPolicy, error) {
	policy, err := config.LoadSubscriptionPolicy(s.paths, s.name)
	if err != nil {
		return policy, err
	}
	for room, trim := range policy.Trims {
		if !strings.HasPrefix(room, "!") || len(trim.Dropped) == 0 || strings.TrimSpace(trim.Reason) == "" {
			return policy, fmt.Errorf("invalid recorded subscription trim")
		}
		if _, err := time.Parse(time.RFC3339Nano, trim.RecordedAt); err != nil {
			return policy, fmt.Errorf("invalid recorded subscription trim timestamp")
		}
		for _, kind := range trim.Dropped {
			if !slices.Contains(onboardKinds, kind) {
				return policy, fmt.Errorf("invalid recorded subscription trim kind")
			}
		}
	}
	return policy, nil
}

func withoutTrim(kinds []string, trim config.SubscriptionTrim) []string {
	return slices.DeleteFunc(slices.Clone(kinds), func(kind string) bool { return slices.Contains(trim.Dropped, kind) })
}

func (s *session) ensureOnboardSub(ctx context.Context, sel SubSelector, now time.Time) (sub Sub, created bool, err error) {
	err = config.WithRuntimeLock(s.paths, s.name, "subscription-policy", time.Now().Add(nativeRPCTimeout), func() error {
		policy, err := s.subscriptionPolicy()
		if err != nil {
			return err
		}
		if len(sel.Kinds) > 0 {
			sel.Kinds = withoutTrim(sel.Kinds, policy.Trims[sel.Room])
			if len(sel.Kinds) == 0 {
				return nil
			}
		}
		sub, created, err = s.ensureSub(ctx, sel, now)
		return err
	})
	return
}

// trim records an explicit local exception after the identity has deliberately
// narrowed its subscriptions. It never deletes or rewrites server selectors.
func cmdSubsTrim(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("subs trim")
	room := fs.String("room", "", "concrete joined room")
	drop := fs.String("drop", "", "standard kinds deliberately omitted")
	reason := fs.String("reason", "", "reason for the deliberate trim")
	clear := fs.Bool("clear", false, "remove the recorded exception")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *room == "" || (*clear && (*drop != "" || *reason != "")) || (!*clear && (*drop == "" || strings.TrimSpace(*reason) == "")) {
		return usagef("usage: a2a subs trim --room R (--drop KINDS --reason TEXT | --clear)")
	}
	dropped := splitList(*drop)
	for _, kind := range dropped {
		if !slices.Contains(onboardKinds, kind) {
			return usagef("--drop accepts only system,owner-needed,decision")
		}
	}
	if !*clear && len(dropped) == 0 {
		return usagef("--drop requires at least one standard kind")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	id, label, err := s.resolveRoom(ctx, *room)
	if err != nil {
		return err
	}
	joined, err := s.client.JoinedRooms(ctx)
	if err != nil {
		return err
	}
	if !slices.Contains(joined, id) {
		return fmt.Errorf("subscription trim requires a joined room")
	}
	err = config.WithRuntimeLock(s.paths, s.name, "subscription-policy", time.Now().Add(nativeRPCTimeout), func() error {
		policy, err := s.subscriptionPolicy()
		if err != nil {
			return err
		}
		if *clear {
			delete(policy.Trims, id)
		} else {
			policy.Trims[id] = config.SubscriptionTrim{Dropped: dropped, Reason: strings.TrimSpace(*reason), RecordedAt: a.Now().UTC().Format(time.RFC3339Nano)}
		}
		return config.SaveSubscriptionPolicy(s.paths, s.name, policy)
	})
	if err != nil {
		return err
	}
	if *clear {
		a.printf("%s\ttrim cleared; subscriptions unchanged; subs sync may restore the baseline\n", label)
	} else {
		a.printf("%s\ttrim recorded: %s; subscriptions unchanged\n", label, strings.Join(dropped, ","))
	}
	return nil
}
