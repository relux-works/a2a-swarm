//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// Owner-approved readiness cap. A caller cannot extend it with a longer context.
const observerReadinessDeadline = 10 * time.Second

// Readiness is a bounded protocol, not a fixed startup sleep. This is sentinel
// harness code only; it does not access Keychains or request observation rights.
func waitObserverReady(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, observerReadinessDeadline)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return errors.New("observer readiness deadline")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return errors.New("observer log unavailable")
		}
		if bytes.Contains(data, []byte("observer=unavailable")) {
			return errors.New("observer unavailable")
		}
		if bytes.Contains(data, []byte("observer=ready")) {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("observer readiness deadline")
		case <-ticker.C:
		}
	}
}

// Sentinel authorization is harness-only. The real caller obtains this JSON
// directly from native `spawn directives`, never from an environment assertion
// or a worker-created release file. Native directive provenance is the trust
// boundary; this is not a new Matrix attestation verifier or same-UID isolation.
type sentinelDirective struct {
	Kind    string `json:"kind"`
	RunID   string `json:"run_id"`
	Payload string `json:"payload"`
	Actor   struct {
		Kind string `json:"kind"`
	} `json:"actor_context"`
}

func authorizeSentinel(current string, native []byte) error {
	if !regexp.MustCompile(`^RUN-[0-9]{6}-[a-z0-9]{6}$`).MatchString(current) {
		return errors.New("sentinel current run missing or malformed")
	}
	var response struct {
		RunID      string              `json:"run_id"`
		Directives []sentinelDirective `json:"directives"`
	}
	if json.Unmarshal(native, &response) != nil || response.RunID != current {
		return errors.New("sentinel native directive run mismatch or unavailable")
	}
	for _, d := range response.Directives {
		if d.Kind != "nudge" || d.RunID != current || d.Actor.Kind != "operator" {
			continue
		}
		var release struct {
			Run    string `json:"sentinel_release"`
			Keeper string `json:"keeper_verified_event"`
			Before string `json:"before_event"`
		}
		if json.Unmarshal([]byte(d.Payload), &release) != nil {
			continue
		}
		event := regexp.MustCompile(`^\$[A-Za-z0-9_-]{20,}$`)
		if release.Run == current && event.MatchString(release.Keeper) && event.MatchString(release.Before) && release.Keeper != release.Before {
			return nil
		}
	}
	return errors.New("sentinel awaits parent release with keeper verification and fresh BEFORE for this exact run")
}

func claimSentinelAttempt(current string, native []byte, root string) error {
	if err := authorizeSentinel(current, native); err != nil {
		return err
	}
	// An observations-only directory consumes the attempt, even if setup fails.
	// Never reuse it or any old fixture path after an unrelated failure.
	if err := os.Mkdir(root, 0o700); err != nil {
		return errors.New("sentinel attempt already used or unavailable; NO RETRY")
	}
	return nil
}

func TestKeychainSentinelRunGuard(t *testing.T) {
	const run = "RUN-261018-abcdef"
	valid := func(actor, target, keeper, before string) []byte {
		payload, _ := json.Marshal(map[string]string{"sentinel_release": target, "keeper_verified_event": keeper, "before_event": before})
		d := sentinelDirective{Kind: "nudge", RunID: run, Payload: string(payload)}
		d.Actor.Kind = actor
		data, _ := json.Marshal(map[string]any{"run_id": run, "directives": []sentinelDirective{d}})
		return data
	}
	keeper, before := "$keeper_verification_event_123", "$fresh_BEFORE_event_4567890"
	for _, mode := range []string{"valid", "successor", "stale-release", "absent", "malformed", "self-minted", "missing-keeper", "missing-before", "same-event", "reused-fixture"} {
		t.Run(mode, func(t *testing.T) {
			current, data := run, valid("operator", run, keeper, before)
			switch mode {
			case "successor":
				current = "RUN-261018-123abc"
			case "stale-release":
				data = valid("operator", "RUN-261018-123abc", keeper, before)
			case "absent":
				data = nil
			case "malformed":
				current = "../../old"
			case "self-minted":
				data = valid("agent", run, keeper, before)
			case "missing-keeper":
				data = valid("operator", run, "", before)
			case "missing-before":
				data = valid("operator", run, keeper, "")
			case "same-event":
				data = valid("operator", run, before, before)
			}
			root := filepath.Join(t.TempDir(), "attempt")
			if mode == "reused-fixture" {
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			err := claimSentinelAttempt(current, data, root)
			if err == nil {
				calls++ /* fake fixture/list/probe/security setup */
			}
			if mode == "valid" {
				if err != nil || calls != 1 {
					t.Fatal("valid parent release refused", err)
				}
				if claimSentinelAttempt(current, data, root) == nil {
					t.Fatal("second attempt admitted")
				}
			} else if err == nil || calls != 0 {
				t.Fatal("unauthorized setup admitted", mode)
			}
		})
	}
}

func TestKeychainObserverReadiness(t *testing.T) {
	for _, mode := range []string{"immediate", "delayed", "never", "unavailable", "missing", "expired-ready"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "observer.log")
			initial := ""
			if mode == "immediate" || mode == "expired-ready" {
				initial = "observer=ready\n"
			}
			if mode == "unavailable" {
				initial = "observer=unavailable ax\n"
			}
			if mode != "missing" {
				if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "delayed" {
				// Deliberately later than the failed real sentinel's 250ms assumption.
				joined := make(chan struct{})
				defer func() { <-joined }()
				go func() {
					defer close(joined)
					time.Sleep(350 * time.Millisecond)
					if err := os.WriteFile(path, []byte("observer=ready\n"), 0o600); err != nil {
						t.Error(err)
					}
				}()
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if mode == "never" {
				var c context.CancelFunc
				ctx, c = context.WithTimeout(ctx, 100*time.Millisecond)
				defer c()
			}
			if mode == "expired-ready" {
				cancel()
			}
			err := waitObserverReady(ctx, path)
			securityCalls := 0
			if err == nil {
				securityCalls++ /* fake real-setup boundary */
			}
			if mode == "immediate" || mode == "delayed" {
				if err != nil {
					t.Fatal("ready observer refused", err)
				}
			} else if err == nil || securityCalls != 0 {
				t.Fatal("unready observer admitted")
			}
		})
	}
}
