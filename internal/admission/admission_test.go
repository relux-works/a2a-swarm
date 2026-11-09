package admission_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/admission"
	"github.com/relux-works/a2a-swarm/internal/admission/testdata/policy"
)

// Proves default none accepts both directions and validates the closed decision contract.
func TestNoneAndNormalization(t *testing.T) {
	c, err := admission.Open("none", admission.Config{Identity: "a", StateDir: filepath.Join(t.TempDir(), "admission", "a")})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, inbound := range []bool{true, false} {
		var d admission.Decision
		if inbound {
			d, err = c.Inbound(context.Background(), admission.Inbound{})
		} else {
			d, err = c.Outbound(context.Background(), admission.Outbound{})
		}
		if err != nil || d.Verdict != admission.Live || d.Retryable || d.Reason != "allowed" {
			t.Fatalf("none: %+v %v", d, err)
		}
	}
	for _, d := range []admission.Decision{{Verdict: 99, Reason: "allowed"}, {Verdict: admission.Live, Retryable: true, Reason: "allowed"}, {Verdict: admission.Exchange, Retryable: true, Reason: "exchange"}, {Verdict: admission.Refused, Reason: policy.Leak}} {
		got := admission.Normalize(d, nil)
		if got.Verdict != admission.Refused || got.Retryable || got.Reason != "policy_invalid" {
			t.Fatalf("invalid: %+v", got)
		}
	}
}

// Proves a separate package's init registration is selectable, and capability/startup errors stay closed.
func TestRegistrationStartup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "admission", "a")
	cfg := admission.Config{Identity: "a", StateDir: dir}
	c, err := admission.Open(policy.Name, cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	f := policy.Get(dir)
	if f.Config != cfg || f.Closes != 1 {
		t.Fatal("factory lifecycle/config mismatch")
	}
	for _, name := range []string{"unknown-" + policy.Leak, policy.NostrName, "fixture-425-error"} {
		if c, err := admission.Open(name, cfg); err == nil || c != nil || strings.Contains(err.Error(), policy.Leak) {
			t.Fatalf("startup did not refuse safely: %v", err)
		}
	}
	cfg.Nostr = true
	if c, err := admission.Open(policy.NostrName, cfg); err != nil {
		t.Fatal(err)
	} else {
		c.Close()
	}
}

// Proves duplicate/empty registration panics and run-time registration cannot alter the table.
func TestRegistrationPanics(t *testing.T) {
	for _, name := range []string{"none", "", "outside-init-425"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("registration %q did not panic", name)
				}
			}()
			admission.Register(name, func(admission.Config) (admission.Checker, error) { return nil, nil })
		}()
	}
	if _, ok := admission.Lookup("outside-init-425"); ok {
		t.Fatal("runtime registration mutated table")
	}
}
