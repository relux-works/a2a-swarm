//go:build linux

package keymakersigner

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/signedpost"
	"github.com/relux-works/swarma-envelope/record"
	"github.com/relux-works/swarma-keymaker/messageauthor"
)

type observedSigner struct {
	*Client
	preimages [][]byte
	ids       []string
}

func (s *observedSigner) Sign(ctx context.Context, p []byte) ([]byte, []byte, error) {
	s.preimages = append(s.preimages, bytes.Clone(p))
	key, signature, err := s.Client.Sign(ctx, p)
	s.mu.Lock()
	s.ids = append(s.ids, s.Client.ids[signedpost.RequestID(p)].id)
	s.mu.Unlock()
	return key, signature, err
}

func renewedAttestation(t *testing.T, original signedpost.Identity) signedpost.Identity {
	t.Helper()
	v, err := record.Parse(original.Attestation, signedpost.MaxAttestationBytes, 16)
	if err != nil {
		t.Fatal("public attestation fixture")
	}
	o := v.(map[string]any)
	body := o["body"].(map[string]any)
	// Qualification is deliberately a test stub: only the raw record's local
	// content-ID binding is exercised, not an issuer or currentness claim.
	body["fixture_refresh"] = "public control"
	id, err := record.ContentID(signedpost.AttestationType, body)
	if err != nil {
		t.Fatal(err)
	}
	o["id"] = id
	raw, err := record.Canonical(o)
	if err != nil {
		t.Fatal(err)
	}
	return signedpost.Identity{Key: original.Key, AttestationID: id, Attestation: raw}
}

func sessionDraft() signedpost.Draft {
	return signedpost.Draft{Room: "!room:test.invalid", Recipients: []string{"@reader:test.invalid"}, Kind: "notice", Topic: "$root", Text: []byte("exact fixture text")}
}

func sessionMethods(t *testing.T, h *fakeHolder, want [][]messageauthor.Method) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	got := make([][]messageauthor.Method, h.connections)
	identities := map[string]bool{}
	for i, q := range h.requests {
		got[h.requestConnections[i]-1] = append(got[h.requestConnections[i]-1], q.Method)
		if q.ID == "00000000000000000000000000000001" {
			t.Fatal("reserved hello ID in a request")
		}
		if q.Method == messageauthor.IdentityMethod {
			if identities[q.ID] {
				t.Fatal("identity request ID reused across connections")
			}
			identities[q.ID] = true
		}
	}
	for _, q := range h.requests {
		if q.Method == messageauthor.SignMethod && identities[q.ID] {
			t.Fatal("sign reused an identity request ID")
		}
	}
	if !reflect.DeepEqual(got, want) || h.hellos != len(want) {
		t.Fatalf("connection method order: got %v, want %v; canonical hellos=%d", got, want, h.hellos)
	}
}

func inputBody(t *testing.T, p []byte) map[string]any {
	t.Helper()
	_, payload, ok := bytes.Cut(p, []byte{'\n'})
	if !ok {
		t.Fatal("preimage domain separator")
	}
	v, err := record.Parse(payload, signedpost.MaxPreimageBytes, 12)
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[string]any)["body"].(map[string]any)
}

// Claim: connection two sends only fresh identity then sign; changed K or T closes without sign and triggers bounded, qualified re-preparation with new P/times/ID beside a stable control.
func TestClientRefreshesChangedIdentity(t *testing.T) {
	for _, mode := range []string{"valid", "key", "attestation", "unstable-key", "unstable-attestation"} {
		t.Run(mode, func(t *testing.T) {
			initial := fixtureIdentity(t)
			next := initial
			if strings.Contains(mode, "key") {
				next.Key = alternateKey
			}
			if strings.Contains(mode, "attestation") {
				next = renewedAttestation(t, initial)
			}
			unstable := strings.HasPrefix(mode, "unstable-")
			h := startHolder(t, holderOptions{identityAt: func(n int) signedpost.Identity {
				if mode == "valid" || n == 1 || unstable && n%2 == 1 {
					return initial
				}
				return next
			}})
			observed := &observedSigner{Client: New(h.endpoint)}
			qualified := 0
			tick := int64(0)
			s := signedpost.Sender{Signer: observed, Audience: "service-a", Qualified: func(signedpost.Identity) bool { qualified++; return true }, Now: func() time.Time { tick++; return time.Unix(1767225600+tick, 0) }}
			sends := 0
			var published signedpost.Signed
			err := s.Publish(context.Background(), sessionDraft(), func(v signedpost.Signed) error { sends++; published = v; return nil })
			attempts := 1
			if mode != "valid" {
				attempts = 2
			}
			if unstable {
				attempts = signedpost.MaxSignAttempts
				if signedpost.Code(err) != "signer_unqualified" || sends != 0 {
					t.Fatal("unstable identity was signed or published", err)
				}
			} else if err != nil || sends != 1 {
				t.Fatal("stable identity did not publish exactly once", err)
			}
			if len(observed.preimages) != attempts || qualified != attempts {
				t.Fatal("re-preparation did not refresh and qualify identity")
			}
			want := make([][]messageauthor.Method, attempts*2)
			for i := range want {
				want[i] = []messageauthor.Method{messageauthor.IdentityMethod}
			}
			if !unstable {
				want[len(want)-1] = append(want[len(want)-1], messageauthor.SignMethod)
			}
			sessionMethods(t, h, want)
			for i, p := range observed.preimages {
				if observed.ids[i] == "" || observed.ids[i] == "00000000000000000000000000000001" {
					t.Fatal("missing or reserved native sign ID")
				}
				if i == 0 {
					continue
				}
				prior, current := inputBody(t, observed.preimages[i-1]), inputBody(t, p)
				if bytes.Equal(observed.preimages[i-1], p) || observed.ids[i-1] == observed.ids[i] {
					t.Fatal("identity change reused P or native sign ID")
				}
				for _, field := range []string{"nonce", "iat", "exp"} {
					if prior[field] == current[field] {
						t.Fatalf("identity change reused %s", field)
					}
				}
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if unstable {
				if h.signAttempts != 0 || h.signatures != 0 {
					t.Fatal("stale K/T reached signing")
				}
			} else {
				if h.signAttempts != 1 || h.signatures != 1 {
					t.Fatal("holder did not sign exactly once")
				}
				last := h.requests[len(h.requests)-1]
				body := inputBody(t, last.Preimage)
				if body["author"] != next.Key || body["author_attestation"] != next.AttestationID || !bytes.Equal(published.Attestation, next.Attestation) {
					t.Fatal("published stale identity")
				}
				if record.VerifySignature(next.Key, h.retained[last.ID].result.Signature, last.Preimage) != nil {
					t.Fatal("renewed signature does not verify")
				}
			}
		})
	}
}

// Claim: lost sign replies and retryable refusals retry on a new hello/identity/sign connection with identical P and sign ID, returning one retained signature beside a no-retry control.
func TestClientRetriesAcrossConnections(t *testing.T) {
	for _, mode := range []string{"valid", "lost-reply", "retryable-refusal"} {
		t.Run(mode, func(t *testing.T) {
			h := startHolder(t, holderOptions{loseFirstSign: mode == "lost-reply", busyFirstSign: mode == "retryable-refusal"})
			observed := &observedSigner{Client: New(h.endpoint)}
			s := signedpost.Sender{Signer: observed, Audience: "service-a", Qualified: func(signedpost.Identity) bool { return true }}
			sends := 0
			err := s.Publish(context.Background(), sessionDraft(), func(signedpost.Signed) error { sends++; return nil })
			if err != nil || sends != 1 {
				t.Fatal("retry did not produce exactly one publication", err)
			}
			want := [][]messageauthor.Method{{messageauthor.IdentityMethod}, {messageauthor.IdentityMethod, messageauthor.SignMethod}}
			if mode != "valid" {
				want = append(want, []messageauthor.Method{messageauthor.IdentityMethod, messageauthor.SignMethod})
			}
			sessionMethods(t, h, want)
			for i := 1; i < len(observed.preimages); i++ {
				if !bytes.Equal(observed.preimages[0], observed.preimages[i]) || observed.ids[0] != observed.ids[i] {
					t.Fatal("retry changed P or retained native sign ID")
				}
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			var first *messageauthor.Request
			attempts := 0
			for i := range h.requests {
				q := &h.requests[i]
				if q.Method != messageauthor.SignMethod {
					continue
				}
				attempts++
				if first == nil {
					first = q
				} else if q.ID != first.ID || !bytes.Equal(q.Preimage, first.Preimage) {
					t.Fatal("wire retry changed ID or P")
				}
			}
			expected := 1
			if mode != "valid" {
				expected = 2
			}
			if attempts != expected || len(observed.preimages) != expected || h.signatures != 1 || len(h.retained) != 1 {
				t.Fatal("holder signed twice or no retry was observed")
			}
		})
	}
}
