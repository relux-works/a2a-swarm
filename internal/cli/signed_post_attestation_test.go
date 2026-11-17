package cli

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/relux-works/swarma-envelope/record"
)

// Claim: carried attestation binding rejects tampering, malformed encoding, oversize values and case-alias keys beside intact and absent controls.
func TestPostSignAttestationTamper(t *testing.T) {
	const member = "works.relux.swarm.author_attestation"
	e, room, _ := signedPostWorld(t)
	id := strings.TrimSpace(e.as("author", "post", room, "--topic", "alpha", "--sign", "--to", "recipient", "hello signed").ok(t).stdout)
	a := signedPostIdentity(t).Attestation
	carried, ok := storedContent(t, e, id)[member].(string)
	if !ok || carried != base64.RawURLEncoding.EncodeToString(a) {
		t.Fatal("carried attestation is not byte-identical to the identity reply")
	}
	changed := append([]byte(nil), a...)
	i := bytes.Index(changed, []byte("generation-0"))
	if i < 0 {
		t.Fatal("fixture lacks the byte to flip")
	}
	changed[i+len("generation-")] ^= 1
	parsed, err := record.Parse(changed, 8192, 16)
	if err != nil {
		t.Fatal(err)
	}
	other := parsed.(map[string]any)
	other["id"], err = record.ContentID("attestation/1", other["body"].(map[string]any))
	if err != nil {
		t.Fatal(err)
	}
	otherRaw, err := record.Canonical(other)
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	cases := []struct {
		name   string
		value  any
		absent bool
		alias  bool
		want   string
	}{
		{name: "intact", value: carried, want: "verified: " + signedPostAuthor},
		{name: "absent", absent: true, want: "verified: " + signedPostAuthor},
		{name: "alias-key", alias: true, value: carried, want: "invalid: content_key_alias"},
		{name: "bound-8192", value: encode(append(append([]byte(nil), a...), bytes.Repeat([]byte{' '}, 8192-len(a))...)), want: "verified: " + signedPostAuthor},
		{name: "flipped-byte", value: encode(changed), want: "invalid: attestation_mismatch"},
		{name: "other-content-id", value: encode(otherRaw), want: "invalid: attestation_mismatch"},
		{name: "over-bound-8193", value: encode(append(append([]byte(nil), a...), bytes.Repeat([]byte{' '}, 8193-len(a))...)), want: "invalid: attestation_mismatch"},
		{name: "not-base64url", value: "+/invalid", want: "invalid: attestation_mismatch"},
		{name: "padded", value: carried + "=", want: "invalid: attestation_mismatch"},
		{name: "noncanonical", value: "AB", want: "invalid: attestation_mismatch"},
		{name: "empty", value: "", want: "invalid: attestation_mismatch"},
		{name: "not-string", value: 1, want: "invalid: attestation_mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := storedContent(t, e, id)
			if tc.absent || tc.alias {
				delete(content, member)
				if tc.alias {
					content[strings.ToUpper(member)] = tc.value
				}
			} else {
				content[member] = tc.value
			}
			copyID := e.hs.inject(room, signedPostAuthor, content)
			if got := e.verificationOf(t, "recipient", room, copyID); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
