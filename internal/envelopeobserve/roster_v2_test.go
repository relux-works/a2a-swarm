package envelopeobserve

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/swarma-envelope/record"
)

const sharedRosterPublisher = "@bridge:example.invalid"

type rosterV2TestRow struct {
	AuthorKey  string   `json:"author_key"`
	Author     string   `json:"author"`
	Publishers []string `json:"publishers"`
}

func rosterV2Marshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func rosterV2Document(t *testing.T, rows any, facts any) []byte {
	t.Helper()
	return rosterV2Marshal(t, map[string]any{"version": rosterVersionV2, "authors": rows, "facts": facts})
}

func rosterV2PublicKey(t *testing.T) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return "ed25519:" + base64.RawURLEncoding.EncodeToString(public)
}

func rosterV2Fixtures(t *testing.T) ([]rosterV2TestRow, map[string]json.RawMessage) {
	t.Helper()
	rows := []rosterV2TestRow{}
	facts := map[string]json.RawMessage{}
	for i, name := range []string{"PA1", "P-BIP-author"} {
		v := loadVector(t, name)
		o, ev, _, _, _ := fromVector(t, v)
		rows = append(rows, rosterV2TestRow{AuthorKey: o.authors[ev.Sender], Author: []string{"alpha", "beta"}[i], Publishers: []string{sharedRosterPublisher}})
		for _, field := range []string{"audience", "service_root", "agent_revocation_root", "operator_root", "operator_owner", "attestations", "operator_attestations", "active_generations", "revocations", "time", "watermark_available", "watermark", "id_journal_available", "seen", "signer_facts", "commit_signing_key", "enrollment_publisher"} {
			value, present := v.Fixture[field]
			if !present {
				continue
			}
			if previous, exists := facts[field]; exists && !bytes.Equal(previous, value) {
				if field != "attestations" && field != "active_generations" {
					t.Fatalf("incompatible public fixture field %s", field)
				}
				var merged, more map[string]json.RawMessage
				if json.Unmarshal(previous, &merged) != nil || json.Unmarshal(value, &more) != nil {
					t.Fatal("invalid public fixture map")
				}
				for key, entry := range more {
					if old, exists := merged[key]; exists && !bytes.Equal(old, entry) {
						t.Fatalf("conflicting public fixture field %s", field)
					}
					merged[key] = entry
				}
				value = rosterV2Marshal(t, merged)
			}
			facts[field] = value
		}
	}
	return rows, facts
}

func rosterV2Observer(t *testing.T, rows []rosterV2TestRow, facts any) *Observer {
	t.Helper()
	o, err := ParseRoster(rosterV2Document(t, rows, facts))
	if err != nil {
		t.Fatalf("valid version 2 roster unavailable: %v", err)
	}
	return o
}

// TestRosterV2SharedPublisher proves two signed authors share one carrier while publisher admission remains exact and local to the signed key.
func TestRosterV2SharedPublisher(t *testing.T) {
	rows, facts := rosterV2Fixtures(t)
	rows[0].Publishers = append(rows[0].Publishers, "@outside:example.invalid")
	o := rosterV2Observer(t, rows, facts)
	for _, tc := range []struct{ name, vector, sender, want string }{
		{"first-control", "PA1", sharedRosterPublisher, "verified: alpha"},
		{"second-control", "P-BIP-author", sharedRosterPublisher, "verified: beta"},
		{"other-carrier-control", "PA1", "@outside:example.invalid", "verified: alpha"},
		{"publisher-not-for-this-key", "P-BIP-author", "@outside:example.invalid", "invalid: publisher_mismatch"},
		{"unlisted-publisher", "PA1", "@other:example.invalid", "invalid: publisher_mismatch"},
		{"publisher-case-is-exact", "PA1", "@Bridge:example.invalid", "invalid: publisher_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ev, room, to, now := fromVector(t, loadVector(t, tc.vector))
			ev.Sender = tc.sender
			var content map[string]any
			if json.Unmarshal(ev.Content, &content) != nil {
				t.Fatal("invalid fixture carrier")
			}
			content["author"] = "forged-label"
			content["displayname"] = "forged-label"
			ev.Content = rosterV2Marshal(t, content)
			if got := o.Observe(ev, room, to, now); got != tc.want {
				t.Fatalf("signed-key/publisher label %q, want %q", got, tc.want)
			}
		})
	}
	_, ev, room, to, now := fromVector(t, loadVector(t, "P-BIP-author"))
	ev.Sender = sharedRosterPublisher
	unknown := rosterV2Observer(t, rows[:1], facts)
	if got := unknown.Observe(ev, room, to, now); got != UnknownAuthor {
		t.Fatalf("unknown signed key label %q, want %q", got, UnknownAuthor)
	}
}

// TestRosterV2VerificationBoundaries proves admitted carriers cannot promote unsigned, bad-signature or delivery-mismatched events and preserve the public verifier refusal codes.
func TestRosterV2VerificationBoundaries(t *testing.T) {
	rows, facts := rosterV2Fixtures(t)
	o := rosterV2Observer(t, rows, facts)
	for _, tc := range []struct{ name, want string }{
		{"PA1", "verified: alpha"}, {"PC2", "verified: alpha"},
		{"P-BIP-author", "verified: beta"}, {"P-BIP-high-s", "verified: beta"},
		{"C01", "invalid: message_room_mismatch"}, {"C02", "invalid: message_recipient_mismatch"},
		{"C04", "invalid: message_text_mismatch"}, {"C05", "invalid: message_text_mismatch"},
		{"C11", "invalid: message_text_mismatch"}, {"E12", "invalid: envelope_signature_invalid"},
		{"N-BIP-author-key", "invalid: signer_unknown"},
		{"N-BIP-bad-signature", "invalid: envelope_signature_invalid"},
		{"N-BIP-body-binding", "invalid: message_text_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ev, room, to, now := fromVector(t, loadVector(t, tc.name))
			ev.Sender = sharedRosterPublisher
			if got := o.Observe(ev, room, to, now); got != tc.want {
				t.Fatalf("verification label %q, want %q", got, tc.want)
			}
			if tc.name == "E12" || tc.name == "N-BIP-bad-signature" {
				ev.Sender = "@outside:example.invalid"
				if got := o.Observe(ev, room, to, now); got != tc.want {
					t.Fatalf("publisher check masked signature refusal: %q, want %q", got, tc.want)
				}
			}
		})
	}
	_, ev, room, to, now := fromVector(t, loadVector(t, "PA1"))
	ev.Sender = sharedRosterPublisher
	ev.Content = json.RawMessage(`{"msgtype":"m.text","body":"hello"}`)
	if got := o.Observe(ev, room, to, now); got != Unsigned {
		t.Fatalf("unsigned label %q, want %q", got, Unsigned)
	}
}

// TestRosterV2Schema proves strict closed rows reject malformed neighbors as whole rosters beside valid exact-spelling controls.
func TestRosterV2Schema(t *testing.T) {
	rows, _ := rosterV2Fixtures(t)
	key := rows[0].AuthorKey
	base := func() map[string]any {
		return map[string]any{"version": rosterVersionV2, "authors": []any{map[string]any{"author_key": key, "author": "alpha", "publishers": []any{sharedRosterPublisher}}}, "facts": map[string]any{}}
	}
	change := func(f func(map[string]any, map[string]any)) []byte {
		doc := base()
		f(doc, doc["authors"].([]any)[0].(map[string]any))
		return rosterV2Marshal(t, doc)
	}
	publishers := make([]string, 64)
	for i := range publishers {
		publishers[i] = fmt.Sprintf("@p%02d:example.invalid", i)
	}
	controls := map[string][]byte{
		"shared": rosterV2Document(t, rows, map[string]any{}),
		"historical-exact-mxid": change(func(_ map[string]any, row map[string]any) {
			row["publishers"] = []string{"@Alpha_1=/+.-:Example.invalid:8448"}
		}),
		"maximum-publishers": change(func(_ map[string]any, row map[string]any) { row["publishers"] = publishers }),
		"maximum-label":      change(func(_ map[string]any, row map[string]any) { row["author"] = strings.Repeat("a", 255) }),
		"quoted-label":       change(func(_ map[string]any, row map[string]any) { row["author"] = `alpha"\beta` }),
		"exact-case-publishers": change(func(_ map[string]any, row map[string]any) {
			row["publishers"] = []string{sharedRosterPublisher, "@Bridge:example.invalid"}
		}),
	}
	for name, raw := range controls {
		t.Run("control-"+name, func(t *testing.T) {
			o, err := ParseRoster(raw)
			if err != nil || o == nil || o.byAuthor == nil {
				t.Fatalf("valid roster unavailable: %v", err)
			}
		})
	}
	cases := map[string][]byte{
		"duplicate-key":            rosterV2Document(t, []rosterV2TestRow{rows[0], {AuthorKey: key, Author: "beta", Publishers: []string{"@other:example.invalid"}}}, map[string]any{}),
		"case-member":              change(func(_ map[string]any, row map[string]any) { row["Author"] = row["author"]; delete(row, "author") }),
		"case-extra-member":        change(func(_ map[string]any, row map[string]any) { row["AUTHOR_KEY"] = key }),
		"unknown-member":           change(func(_ map[string]any, row map[string]any) { row["extra"] = true }),
		"source-not-reader-member": change(func(_ map[string]any, row map[string]any) { row["source"] = "console" }),
		"missing-member":           change(func(_ map[string]any, row map[string]any) { delete(row, "author") }),
		"unknown-top-member":       change(func(doc, _ map[string]any) { doc["extra"] = true }),
		"case-top-member":          change(func(doc, _ map[string]any) { doc["Authors"] = doc["authors"]; delete(doc, "authors") }),
		"unsupported-version":      change(func(doc, _ map[string]any) { doc["version"] = "a2a-envelope-roster/3" }),
		"version-one-list":         change(func(doc, _ map[string]any) { doc["version"] = "a2a-envelope-roster/1" }),
		"version-two-map":          change(func(doc, _ map[string]any) { doc["authors"] = map[string]string{sharedRosterPublisher: key} }),
		"empty-authors":            change(func(doc, _ map[string]any) { doc["authors"] = []any{} }),
		"empty-publishers":         change(func(_ map[string]any, row map[string]any) { row["publishers"] = []any{} }),
		"65-publishers": change(func(_ map[string]any, row map[string]any) {
			row["publishers"] = append(append([]string{}, publishers...), "@extra:example.invalid")
		}),
		"publisher-object": change(func(_ map[string]any, row map[string]any) { row["publishers"] = map[string]any{} }),
		"publisher-type":   change(func(_ map[string]any, row map[string]any) { row["publishers"] = []any{1} }),
		"publisher-duplicate": change(func(_ map[string]any, row map[string]any) {
			row["publishers"] = []string{sharedRosterPublisher, sharedRosterPublisher}
		}),
		"author-type":   change(func(_ map[string]any, row map[string]any) { row["author"] = true }),
		"key-type":      change(func(_ map[string]any, row map[string]any) { row["author_key"] = 1 }),
		"key-algorithm": change(func(_ map[string]any, row map[string]any) { row["author_key"] = "p256:AAAA" }),
		"key-case": change(func(_ map[string]any, row map[string]any) {
			row["author_key"] = strings.Replace(key, "ed25519:", "ED25519:", 1)
		}),
		"key-padding":      change(func(_ map[string]any, row map[string]any) { row["author_key"] = key + "=" }),
		"unknown-fact":     change(func(doc, _ map[string]any) { doc["facts"] = map[string]any{"delivery": map[string]any{}} }),
		"facts-not-object": change(func(doc, _ map[string]any) { doc["facts"] = []any{} }),
		"fractional-fact":  change(func(doc, _ map[string]any) { doc["facts"] = map[string]any{"time": map[string]any{"trusted": 1.5}} }),
	}
	for _, label := range []string{"", "alpha beta", "alpha\tbeta", "alpha\nbeta", "alpha\x7f", "ä", strings.Repeat("a", 256)} {
		cases["label-"+fmt.Sprint(len(cases))] = change(func(_ map[string]any, row map[string]any) { row["author"] = label })
	}
	for i, sender := range []string{"bridge:example.invalid", "@:example.invalid", "@bridge:", "@a b:example.invalid", "@ä:example.invalid", "@a\n:example.invalid", "@" + strings.Repeat("a", 256) + ":example.invalid"} {
		cases[fmt.Sprintf("mxid-%d", i)] = change(func(_ map[string]any, row map[string]any) { row["publishers"] = []string{sender} })
	}
	first := `{"author_key":"` + key + `","author":"alpha","publishers":["` + sharedRosterPublisher + `"]}`
	escaped := strings.Replace(strings.Replace(first, key, `\u0065`+key[1:], 1), `"alpha"`, `"beta"`, 1)
	cases["duplicate-escaped-key"] = []byte(`{"version":"a2a-envelope-roster/2","authors":[` + first + `,` + escaped + `],"facts":{}}`)
	cases["duplicate-member"] = []byte(strings.Replace(string(controls["shared"]), `"author":"alpha"`, `"author":"alpha","author":"beta"`, 1))
	cases["duplicate-escaped-member"] = []byte(strings.Replace(string(controls["shared"]), `"author":"alpha"`, `"author":"alpha","\u0061uthor":"beta"`, 1))
	cases["invalid-unicode-scalar"] = []byte(strings.Replace(string(controls["shared"]), `"alpha"`, `"\ud800"`, 1))
	cases["invalid-utf8"] = bytes.Replace(controls["shared"], []byte("alpha"), []byte{0xff}, 1)
	cases["trailing-data"] = append(append([]byte{}, controls["shared"]...), []byte(` {}`)...)
	cases["oversize"] = append(append([]byte{}, controls["shared"]...), bytes.Repeat([]byte(" "), maxRosterBytes)...)
	cases["over-depth"] = change(func(doc, _ map[string]any) {
		doc["facts"] = json.RawMessage(strings.Repeat(`{"x":`, 16) + `{}` + strings.Repeat(`}`, 16))
	})
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if o, err := ParseRoster(raw); err == nil || o != nil {
				t.Fatalf("invalid roster available: error=%v observer_present=%v", err, o != nil)
			}
		})
	}
}

// TestRosterV2AuthorBounds proves 65 and 4096 unique author rows load, 4097 do not, and larger outer lists do not relax facts-array limits.
func TestRosterV2AuthorBounds(t *testing.T) {
	rows := make([]rosterV2TestRow, maxRosterEntries+1)
	for i := range rows {
		rows[i] = rosterV2TestRow{AuthorKey: rosterV2PublicKey(t), Author: "alpha", Publishers: []string{sharedRosterPublisher}}
	}
	for _, n := range []int{65, maxRosterEntries} {
		o := rosterV2Observer(t, rows[:n], map[string]any{})
		if len(o.byAuthor) != n || !o.context.AdmittedKeys.IsAvailable() {
			t.Fatalf("loaded %d author rows, want %d with admitted-key evidence", len(o.byAuthor), n)
		}
	}
	if o, err := ParseRoster(rosterV2Document(t, rows, map[string]any{})); err == nil || o != nil {
		t.Fatal("4097-author roster available")
	}
	if _, err := ParseRoster(rosterV2Document(t, rows[:65], map[string]any{"time": map[string]any{"nested": make([]any, 64)}})); err != nil {
		t.Fatalf("64-element facts array control unavailable: %v", err)
	}
	facts := map[string]any{"time": map[string]any{"nested": make([]any, 65)}}
	if o, err := ParseRoster(rosterV2Document(t, rows[:65], facts)); err == nil || o != nil {
		t.Fatal("larger author list relaxed facts array bound")
	}
}

// TestRosterV2ExactKeys proves distinct canonical keys with case-different base64 payloads remain distinct rather than being lowercased.
func TestRosterV2ExactKeys(t *testing.T) {
	rows, _ := rosterV2Fixtures(t)
	key := rows[1].AuthorKey
	for i := strings.IndexByte(key, ':') + 1; i < len(key); i++ {
		c := key[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		} else if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		} else {
			continue
		}
		candidate := key[:i] + string(c) + key[i+1:]
		if record.ValidateKey(candidate) != nil {
			continue
		}
		o := rosterV2Observer(t, []rosterV2TestRow{rows[1], {AuthorKey: candidate, Author: "gamma", Publishers: []string{sharedRosterPublisher}}}, map[string]any{})
		if len(o.byAuthor) != 2 || o.byAuthor[key].Author != "beta" || o.byAuthor[candidate].Author != "gamma" {
			t.Fatal("case-distinct canonical public keys were merged")
		}
		return
	}
	t.Fatal("public BIP-340 fixture has no valid case-different canonical neighbor")
}

// TestRosterV2UnavailableProtection proves the unchanged non-root storage gate rejects an agent-owned version 2 file beside direct parser admission.
func TestRosterV2UnavailableProtection(t *testing.T) {
	rows, _ := rosterV2Fixtures(t)
	raw := rosterV2Document(t, rows, map[string]any{})
	if _, err := ParseRoster(raw); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("requires a non-root reader")
	}
	path := filepath.Join(t.TempDir(), "roster.json")
	if err := os.WriteFile(path, raw, 0o444); err != nil {
		t.Fatal(err)
	}
	if o, err := Load(path); err == nil || o != nil {
		t.Fatal("agent-owned version 2 roster was accepted")
	}
	if o, err := Load("relative-roster.json"); err == nil || o != nil {
		t.Fatal("relative roster selector was accepted")
	}
}
