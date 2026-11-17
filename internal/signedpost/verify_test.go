package signedpost

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/relux-works/swarma-envelope/record"
)

// Claim: completed-post verification rejects malformed carried evidence, envelope aliases, altered ids and invalid signatures beside a valid signed control.
func TestCompletedPostVerification(t *testing.T) {
	now := testNow
	s := newSender(&fakeSigner{identity: validIdentity(t)}, &now)
	d := draft("hello", "@receiver:test.invalid")
	signed, err := s.Sign(context.Background(), d)
	if err != nil {
		t.Fatal("fixture signing failed")
	}
	encoded := base64.RawURLEncoding.EncodeToString(signed.Attestation)
	for _, tc := range []struct {
		name string
		raw  []byte
		att  string
		want error
	}{
		{"absent", signed.Envelope, "", MissingAttestation},
		{"padding", signed.Envelope, encoded + "=", InvalidAttestation},
		{"alphabet", signed.Envelope, "%", InvalidAttestation},
		{"large", signed.Envelope, strings.Repeat("A", base64.RawURLEncoding.EncodedLen(8193)), LargeAttestation},
		{"alias", []byte(strings.Replace(string(signed.Envelope), `"version":`, `"Version":`, 1)), encoded, InvalidEnvelope},
		{"duplicate", []byte(strings.Replace(string(signed.Envelope), `"version":`, `"version":"swarm-envelope/2","version":`, 1)), encoded, InvalidEnvelope},
		{"id", []byte(strings.Replace(string(signed.Envelope), signed.ID, otherContentID, 1)), encoded, InvalidEnvelope},
		{"signature", func() []byte {
			e, _ := parseEnvelope(t, signed.Envelope)
			sig := e["signatures"].([]any)[0].(map[string]any)
			b, _ := record.DecodeBase64(sig["sig"].(string), 64)
			b[0] ^= 1
			sig["sig"] = base64.RawURLEncoding.EncodeToString(b)
			raw, _ := record.Canonical(e)
			return raw
		}(), encoded, InvalidSignature},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if Verify(signed.Envelope, encoded, testKey, d, now) != nil {
				t.Fatal("valid completed post refused")
			}
			if got := Verify(tc.raw, tc.att, testKey, d, now); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			if Verify(signed.Envelope, encoded, testKey, d, now) != nil {
				t.Fatal("restored completed post refused")
			}
		})
	}
}
