package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/signedpost"
)

func configurePostSigner(a *App, signer *postFakeSigner) {
	a.postSigner, a.postAudience = signer, signedPostAudience
	a.postQualified = func(signedpost.Identity) bool { return true }
}

// Claim: --sign with --operation-id refuses before HTTP, signer calls or a journal record, beside durable-only and signed-only sends.
func TestPostSignOperationIDRefused(t *testing.T) {
	for _, mode := range []string{"sign-and-operation", "operation-only", "sign-only"} {
		t.Run(mode, func(t *testing.T) {
			w := operationWorldNew(t)
			a, out, diag := w.app()
			signer := &postFakeSigner{identity: signedPostIdentity(t)}
			configurePostSigner(a, signer)
			args := operationArgs()
			if mode == "sign-only" {
				args = append(args[:4:4], args[6:]...)
			}
			if mode != "operation-only" {
				args = append(args, "--sign")
			}
			code := a.Run(context.Background(), args)
			record := filepath.Join(w.paths.Base, "post-operations", "agents", "alice", "op.json")
			if mode == "sign-and-operation" {
				if code != ExitUsage || !strings.Contains(diag.String(), "durable post requires") {
					t.Fatalf("exit %d, want usage refusal: %s", code, diag)
				}
				if w.calls.Load() != 0 || w.sends.Load() != 0 || w.store.reads.Load() != 0 || out.Len() != 0 || signer.identityCalls != 0 || signer.signCalls != 0 {
					t.Fatal("refusal reached HTTP, provider, signer or stdout")
				}
				if _, err := os.Stat(record); !os.IsNotExist(err) {
					t.Fatal("refusal created an operation record")
				}
				return
			}
			if code != ExitOK || w.sends.Load() != 1 || out.Len() == 0 {
				t.Fatalf("control exit %d sends %d: %s", code, w.sends.Load(), diag)
			}
			var content map[string]any
			if err := json.Unmarshal(w.requests[0], &content); err != nil {
				t.Fatal(err)
			}
			if mode == "operation-only" {
				if w.record().Outcome != "acknowledged" || signer.identityCalls != 0 || signer.signCalls != 0 || content[matrix.EnvelopeContentKey] != nil || content[signedpost.AttestationContentKey] != nil {
					t.Fatal("durable control did not take the unsigned durable path")
				}
			} else {
				if signer.identityCalls != 1 || signer.signCalls != 1 || content[matrix.EnvelopeContentKey] == nil || content[signedpost.AttestationContentKey] == nil {
					t.Fatal("signed control did not sign")
				}
				if _, err := os.Stat(record); !os.IsNotExist(err) {
					t.Fatal("signed control created an operation record")
				}
			}
		})
	}
}

// Claim: a lost first PUT acknowledgement resends byte-identical content under the same transaction id with one signature, beside a first-PUT success.
func TestPostSignResendKeepsTransactionAndBytes(t *testing.T) {
	for _, loseACK := range []bool{false, true} {
		t.Run(map[bool]string{false: "first-put-succeeds", true: "first-ack-lost"}[loseACK], func(t *testing.T) {
			w := operationWorldNew(t)
			a, _, diag := w.app()
			signer := &postFakeSigner{identity: signedPostIdentity(t)}
			configurePostSigner(a, signer)
			a.HTTP = &http.Client{Transport: operationTransport(func(req *http.Request) (*http.Response, error) {
				resp, err := w.transport(req)
				if loseACK && req.Method == http.MethodPut && w.sends.Load() == 1 && err == nil {
					resp.Body.Close()
					return nil, errors.New("synthetic acknowledgement lost")
				}
				return resp, err
			})}
			if code := a.Run(context.Background(), []string{"post", "!room:example.invalid", "--topic", "$root", "--sign", "synthetic body"}); code != ExitOK {
				t.Fatalf("exit %d: %s", code, diag)
			}
			want := 1
			if loseACK {
				want = 2
			}
			if len(w.requests) != want || len(w.transactions) != want || signer.identityCalls != 1 || signer.signCalls != 1 || len(w.applied) != 1 {
				t.Fatalf("PUTs %d transactions %d identity %d sign %d applied %d", len(w.requests), len(w.transactions), signer.identityCalls, signer.signCalls, len(w.applied))
			}
			for i := range w.requests {
				if !bytes.Equal(w.requests[0], w.requests[i]) || w.transactions[0] != w.transactions[i] {
					t.Fatal("resend changed transaction id or content bytes")
				}
			}
		})
	}
}

// Claim: a signed post missing --topic exits as usage before HTTP or signer calls with either production or configured wiring, beside a valid-topic control.
func TestPostSignMissingTopicUsage(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "production", true: "configured"}[configured], func(t *testing.T) {
			w := operationWorldNew(t)
			a, out, diag := w.app()
			signer := &postFakeSigner{identity: signedPostIdentity(t)}
			if configured {
				configurePostSigner(a, signer)
			}
			if code := a.Run(context.Background(), []string{"post", "!room:example.invalid", "--sign", "text"}); code != ExitUsage || !strings.Contains(diag.String(), "--topic is required") {
				t.Fatalf("exit %d, want missing-topic usage: %s", code, diag)
			}
			if w.calls.Load() != 0 || signer.identityCalls != 0 || signer.signCalls != 0 || out.Len() != 0 {
				t.Fatal("usage refusal had effects")
			}
			diag.Reset()
			code := a.Run(context.Background(), []string{"post", "!room:example.invalid", "--topic", "$root", "--sign", "text"})
			if configured {
				if code != ExitOK || w.sends.Load() != 1 || signer.identityCalls != 1 || signer.signCalls != 1 {
					t.Fatalf("configured control exit %d: %s", code, diag)
				}
			} else if code != ExitError || !strings.Contains(diag.String(), "signer_unconfigured") || w.calls.Load() != 0 {
				t.Fatalf("production control exit %d: %s", code, diag)
			}
		})
	}
}

// Claim: content at the exact event budget signs and sends, while one byte over refuses event_too_large with zero Sign calls or PUTs.
func TestPostSignEventBudgetBeforeSigning(t *testing.T) {
	// JSON escaping makes this body large without exceeding the input cap.
	// Framed HTML has already been dropped at the existing formatting limit,
	// so each appended ASCII x adds exactly one byte to the wire content.
	seed := strings.Repeat("&", 9000)
	w := operationWorldNew(t)
	a, _, diag := w.app()
	configurePostSigner(a, &postFakeSigner{identity: signedPostIdentity(t)})
	args := func(text string) []string {
		return []string{"post", "!room:example.invalid", "--topic", "$root", "--sign", text}
	}
	if code := a.Run(context.Background(), args(seed)); code != ExitOK || len(w.requests) != 1 {
		t.Fatalf("sizing control exit %d: %s", code, diag)
	}
	var content map[string]any
	if err := json.Unmarshal(w.requests[0], &content); err != nil {
		t.Fatal(err)
	}
	if content["formatted_body"] != nil || len(w.requests[0]) >= matrix.EnvelopeEventContentBudget {
		t.Fatal("sizing control must be below the budget without formatted HTML")
	}
	at := seed + strings.Repeat("x", matrix.EnvelopeEventContentBudget-len(w.requests[0]))
	for _, over := range []bool{false, true} {
		t.Run(map[bool]string{false: "at-bound", true: "one-byte-over"}[over], func(t *testing.T) {
			w := operationWorldNew(t)
			a, out, diag := w.app()
			signer := &postFakeSigner{identity: signedPostIdentity(t)}
			configurePostSigner(a, signer)
			text := at
			if over {
				text += "x"
			}
			code := a.Run(context.Background(), args(text))
			if signer.identityCalls != 1 {
				t.Fatalf("expected one identity lookup for sizing, got %d", signer.identityCalls)
			}
			if over {
				if code != ExitError || !strings.Contains(diag.String(), "event_too_large (61441 bytes") || signer.signCalls != 0 || w.sends.Load() != 0 || out.Len() != 0 {
					t.Fatalf("oversize exit %d sign %d PUTs %d: %s", code, signer.signCalls, w.sends.Load(), diag)
				}
			} else if code != ExitOK || signer.signCalls != 1 || w.sends.Load() != 1 || len(w.requests[0]) != matrix.EnvelopeEventContentBudget {
				t.Fatalf("bound control exit %d sign %d PUTs %d: %s", code, signer.signCalls, w.sends.Load(), diag)
			}
		})
	}
}
