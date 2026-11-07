package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

type roomCoverage struct {
	Room              string   `json:"room"`
	Label             string   `json:"label"`
	Missing           []string `json:"missing"`
	Skipped           string   `json:"skipped,omitempty"`
	Trimmed           []string `json:"trimmed,omitempty"`
	Status            string   `json:"status"`
	MissingPrincipals []string `json:"missing_principals,omitempty"`
}

type coverageReport struct {
	OK             bool           `json:"ok"`
	Rooms          []roomCoverage `json:"rooms"`
	PrincipalError string         `json:"principal_error,omitempty"`
}

// missingRoomKinds measures current unconditional coverage, not historical
// delivery or future wake/reply guarantees. Restricted/one-shot selectors do
// not provide the baseline even when their kind lists contain all three kinds.
func missingRoomKinds(room string, list SubList, now time.Time) []string {
	missing := []string{}
	for _, kind := range onboardKinds {
		covered := false
		for _, sub := range list.Subs {
			if sub.validate() != nil || sub.expiredAt(now) || sub.Room != "" && sub.Room != room ||
				sub.Topic != "" || sub.Product != "" || sub.From != "" || sub.Subject != "" ||
				len(sub.subtypeList()) > 0 || sub.UntilKind != "" {
				continue
			}
			if sub.CreatedAt != "" {
				created, err := time.Parse(time.RFC3339Nano, sub.CreatedAt)
				if err != nil || created.After(now) {
					continue
				}
			}
			if sub.Since != "" && sub.SinceTS > now.UnixMilli() {
				continue
			}
			if len(sub.Kinds) == 0 || slices.Contains(sub.Kinds, kind) {
				covered = true
				break
			}
		}
		if !covered {
			missing = append(missing, kind)
		}
	}
	return missing
}

func cmdSubsCheck(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("subs check")
	asJSON := fs.Bool("json", false, "print coverage as one JSON object")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef("usage: a2a subs check [--json]")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	// Deliberately avoid updateSubs: an audit must not migrate legacy rows or
	// rewrite server account data or local delivery state.
	list, err := s.getSubs(ctx)
	if err != nil {
		return err
	}
	policy, err := s.subscriptionPolicy()
	if err != nil {
		return err
	}
	configured, principalErr := ownerFiles(s.paths, s.name).principals()
	rooms, err := s.client.JoinedRooms(ctx)
	if err != nil {
		return fmt.Errorf("read joined rooms: %w", err)
	}
	exempt := map[string]string{}
	for _, alias := range []string{SystemRoom, BroadcastRoom} {
		norm, err := normalizeRoom(alias, s.server)
		if err != nil {
			return err
		}
		id, err := s.client.ResolveAlias(ctx, norm)
		if err != nil {
			if errors.Is(err, matrix.ErrUnknownAlias) {
				continue
			}
			return fmt.Errorf("resolve subscription exemption %s: %w", alias, err)
		}
		reason := "projection room"
		if alias == BroadcastRoom {
			reason = "broadcast membership"
		}
		exempt[id] = reason
	}
	slices.Sort(rooms)
	result := coverageReport{OK: true, Rooms: []roomCoverage{}}
	if principalErr != nil {
		configured = nil
		result.OK = false
		result.PrincipalError = principalConfigProblem
	}
	now := a.Now()
	for _, id := range rooms {
		var create struct {
			Type string `json:"type"`
		}
		if err := s.client.GetState(ctx, id, "m.room.create", "", &create); err != nil {
			return fmt.Errorf("read room type: %w", err)
		}
		if create.Type == "m.space" {
			exempt[id] = "space"
		}
		row := roomCoverage{Room: sanitizeLine(id), Label: sanitizeLine(s.roomLabel(ctx, id, nil)), Missing: []string{}, Skipped: exempt[id], Status: "covered"}
		if row.Skipped == "" {
			row.Trimmed = policy.Trims[id].Dropped
			row.Missing = withoutTrim(missingRoomKinds(id, list, now), policy.Trims[id])
			if len(row.Missing) > 0 {
				row.Status = "missing"
			} else if len(row.Trimmed) > 0 {
				row.Status = "trimmed"
			}
			result.OK = result.OK && len(row.Missing) == 0
		} else {
			row.Status = "skipped"
		}
		principals, err := s.roomPrincipals(ctx, id, configured)
		if err != nil {
			return err
		}
		row.MissingPrincipals = missingRoomPrincipals(id, principals, list, now)
		if len(row.MissingPrincipals) > 0 {
			row.Status = "missing"
			result.OK = false
		}
		result.Rooms = append(result.Rooms, row)
	}
	return a.printCoverage(result, *asJSON, len(configured) > 0)
}

func (a *App) printCoverage(result coverageReport, asJSON, principalsConfigured bool) error {
	if asJSON {
		if err := a.printJSON(result); err != nil {
			return err
		}
	} else {
		if result.PrincipalError != "" {
			a.printf("%s\n", result.PrincipalError)
		}
		for _, row := range result.Rooms {
			switch {
			case len(row.MissingPrincipals) > 0:
				a.printf("%s\tmissing principal all-events: %s", row.Label, strings.Join(row.MissingPrincipals, ","))
				if len(row.Missing) > 0 {
					a.printf("; missing kinds: %s", strings.Join(row.Missing, ","))
				}
				a.printf("\n")
			case row.Skipped != "":
				a.printf("%s\tskipped: %s\n", row.Label, row.Skipped)
			case len(row.Missing) > 0:
				a.printf("%s\tmissing: %s\n", row.Label, strings.Join(row.Missing, ","))
			case len(row.Trimmed) > 0:
				a.printf("%s\ttrimmed: %s (recorded)\n", row.Label, strings.Join(row.Trimmed, ","))
			default:
				a.printf("%s\tcovered\n", row.Label)
			}
		}
		if result.OK {
			a.printf("coverage: ok\n")
		}
	}
	if !result.OK {
		if result.PrincipalError != "" {
			return errors.New(result.PrincipalError)
		}
		if !principalsConfigured {
			return fmt.Errorf("subscription coverage gaps; review missing kinds, then run a2a subs sync to backfill")
		}
		return fmt.Errorf("subscription coverage gaps; review missing kinds/principals, then run a2a subs sync to backfill")
	}
	return nil
}
