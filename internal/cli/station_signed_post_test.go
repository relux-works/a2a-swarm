//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/relux-works/a2a-swarm/internal/daemon"
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/signedpost"
	"github.com/relux-works/swarma-envelope/record"
)

type signedStation struct {
	f        *daemonFixture
	root     string
	now      time.Time
	body     string
	to       []string
	raw, att []byte
}

func newSignedStation(t *testing.T) *signedStation {
	t.Helper()
	f := newDaemonFixture(t)
	root := strings.TrimSpace(f.run("alice", "", "topic", "new", f.rooms["alice"], "alpha", "A").ok(t).stdout)
	now := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
	f.daemon.Now = func() time.Time { return now }
	id := signedPostIdentity(t)
	s := &signedStation{f: f, root: root, now: now, body: "exact text", to: []string{"@alice:" + testServer, "@bob:" + testServer}, att: id.Attestation}
	sender := signedpost.Sender{Signer: &postFakeSigner{identity: id}, Audience: signedPostAudience, Qualified: func(signedpost.Identity) bool { return true }, Now: func() time.Time { return now }, Rand: bytes.NewReader(make([]byte, 16))}
	signed, err := sender.Sign(context.Background(), s.draft())
	if err != nil {
		t.Fatal("cannot build valid signed fixture")
	}
	s.raw = append(append([]byte(" \n\t"), signed.Envelope...), '\r', '\n')
	return s
}

func (s *signedStation) draft() signedpost.Draft {
	return signedpost.Draft{Room: s.f.rooms["alice"], Recipients: s.to, Kind: "notice", Topic: s.root, Text: []byte(s.body)}
}

func (s *signedStation) policy(t *testing.T, raw []byte) {
	t.Helper()
	selector, _ := json.Marshal(map[string]string{"authors_path": "/etc/a2a/station-authors.json"})
	if err := os.WriteFile(filepath.Join(s.f.home, "station-authors.json"), selector, 0600); err != nil {
		t.Fatal(err)
	}
	s.f.daemon.loadStationAuthors = func(string) (daemon.StationAuthors, error) { return daemon.ParseStationAuthors(raw) }
}

func stationMap(uid uint32) []byte {
	return []byte(fmt.Sprintf(`{"version":"a2a-station-authors/1","authors":[{"uid":%d,"author_key":%q}]}`, uid, signedPostKey))
}

func (s *signedStation) args(t *testing.T, raw, att []byte, to []string, body string) []string {
	t.Helper()
	args := []string{"post", s.f.rooms["alice"], "--topic", s.root, "--to", strings.Join(to, ","), body}
	if raw != nil {
		args = append(args, "--envelope=@"+envelopeFile(t, raw))
	}
	if att != nil {
		args = append(args, "--author-attestation", "@"+envelopeFile(t, att))
	}
	return args
}

func stationTree(t *testing.T, roots ...string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				out[path] = sha256.Sum256([]byte("directory"))
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[path] = sha256.Sum256(data)
			return nil
		})
		if err != nil {
			t.Fatal("cannot snapshot fixture state")
		}
	}
	return out
}

func (s *signedStation) refused(t *testing.T, name, code string, args []string) {
	t.Helper()
	before := stationTree(t, s.f.home, s.f.stations["alice"], s.f.stations["bob"])
	calls := len(s.f.envs["alice"].hs.urls()) + len(s.f.envs["bob"].hs.urls())
	r := s.f.run(name, "", args...)
	wantExit := ExitError
	if len(args) > 0 && args[0] == "hook" {
		wantExit = ExitOK
	}
	if r.code != wantExit || r.stdout != "" || r.stderr != "a2a: daemon: "+code+"\n" {
		t.Fatalf("refusal differs: exit=%d, stderr=%q, want=%s", r.code, r.stderr, code)
	}
	if calls != len(s.f.envs["alice"].hs.urls())+len(s.f.envs["bob"].hs.urls()) {
		t.Fatal("refusal contacted carrier")
	}
	if !reflect.DeepEqual(before, stationTree(t, s.f.home, s.f.stations["alice"], s.f.stations["bob"])) {
		t.Fatal("refusal changed fixture state")
	}
	if log, _ := s.f.log.snapshot(); strings.Contains(log, signedPostKey) || strings.Contains(log, s.body) || strings.Contains(log, string(s.raw)) {
		t.Fatal("signed input reached daemon log")
	}
}

func (s *signedStation) control(t *testing.T) {
	t.Helper()
	r := s.f.run("alice", "", s.args(t, s.raw, s.att, s.to, s.body)...).ok(t)
	ev, ok := s.f.envs["alice"].hs.eventByID(strings.TrimSpace(r.stdout))
	var content map[string]any
	if !ok || json.Unmarshal(ev.Content, &content) != nil {
		t.Fatal("valid post not carried")
	}
	if content["body"] != s.body || content[matrix.EnvelopeContentKey] != base64.RawURLEncoding.EncodeToString(s.raw) || content[signedpost.AttestationContentKey] != base64.RawURLEncoding.EncodeToString(s.att) {
		t.Fatal("carrier changed signed input bytes")
	}
}

// stationResign uses public deterministic test operands, not production keys.
func stationResign(t *testing.T, raw []byte, operand byte, edit func(map[string]any)) ([]byte, string) {
	t.Helper()
	v, err := record.Parse(raw, 65536, 16)
	if err != nil {
		t.Fatal("fixture parse failed")
	}
	e := v.(map[string]any)
	body := e["body"].(map[string]any)
	edit(body)
	private, public := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{operand}, 32))
	key := "bip340:" + base64.RawURLEncoding.EncodeToString(schnorr.SerializePubKey(public))
	body["author"] = key
	preimage, err := record.SigningPreimage(signedpost.MessageType, key, signedpost.SignatureRole, body)
	if err != nil {
		t.Fatal("fixture preimage failed")
	}
	digest := sha256.Sum256(preimage)
	sig, err := schnorr.Sign(private, digest[:])
	if err != nil {
		t.Fatal("fixture signing failed")
	}
	e["id"], err = record.ContentID(signedpost.MessageType, body)
	if err != nil {
		t.Fatal("fixture id failed")
	}
	e["signatures"] = []any{map[string]any{"key": key, "role": signedpost.SignatureRole, "sig": base64.RawURLEncoding.EncodeToString(sig.Serialize())}}
	out, err := record.Canonical(e)
	if err != nil {
		t.Fatal("fixture assembly failed")
	}
	return out, key
}

// Claim: each signed-post binding, time, signature and attestation refusal has zero carrier/state effects beside exact-byte valid posts.
func TestSignedStationPosts(t *testing.T) {
	s := newSignedStation(t)
	s.policy(t, stationMap(uint32(os.Geteuid())))
	originalAttestation := s.att
	s.att = append(append([]byte{}, s.att...), bytes.Repeat([]byte(" "), signedpost.MaxAttestationBytes-len(s.att))...)
	s.control(t)
	s.att = originalAttestation
	type trial struct {
		raw, att []byte
		to       []string
		text     string
	}
	cases := []struct {
		name, code string
		edit       func(*testing.T, *trial)
	}{
		{"unsigned", "signed_post_required", func(t *testing.T, p *trial) { p.raw, p.att = nil, nil }},
		{"other-key", "signed_author_mismatch", func(t *testing.T, p *trial) {
			var key string
			p.raw, key = stationResign(t, p.raw, 4, func(map[string]any) {})
			if signedpost.Verify(p.raw, base64.RawURLEncoding.EncodeToString(p.att), key, s.draft(), s.now) != nil {
				t.Fatal("other-key control is not valid")
			}
		}},
		{"room", "signed_room_mismatch", func(t *testing.T, p *trial) {
			p.raw, _ = stationResign(t, p.raw, 3, func(b map[string]any) { b["room"] = "!other:test.invalid" })
		}},
		{"recipient-added", "signed_recipients_mismatch", func(t *testing.T, p *trial) { p.to = append(p.to, "@extra:test.invalid") }},
		{"recipient-dropped", "signed_recipients_mismatch", func(t *testing.T, p *trial) { p.to = p.to[:1] }},
		{"recipient-order", "signed_recipients_mismatch", func(t *testing.T, p *trial) { p.to[0], p.to[1] = p.to[1], p.to[0] }},
		{"text-newline", "signed_text_mismatch", func(t *testing.T, p *trial) { p.text += "\n" }},
		{"expired", "signed_post_expired", func(t *testing.T, p *trial) {
			p.raw, _ = stationResign(t, p.raw, 3, func(b map[string]any) {
				b["iat"] = s.now.Add(-121 * time.Second).Format(time.RFC3339)
				b["exp"] = s.now.Add(-time.Second).Format(time.RFC3339)
			})
		}},
		{"expiry-boundary", "signed_post_expired", func(t *testing.T, p *trial) {
			p.raw, _ = stationResign(t, p.raw, 3, func(b map[string]any) {
				b["iat"] = s.now.Add(-120 * time.Second).Format(time.RFC3339)
				b["exp"] = s.now.Format(time.RFC3339)
			})
		}},
		{"lifetime", "signed_lifetime_invalid", func(t *testing.T, p *trial) {
			p.raw, _ = stationResign(t, p.raw, 3, func(b map[string]any) { b["exp"] = s.now.Add(121 * time.Second).Format(time.RFC3339) })
		}},
		{"future", "signed_lifetime_invalid", func(t *testing.T, p *trial) {
			p.raw, _ = stationResign(t, p.raw, 3, func(b map[string]any) { b["iat"] = s.now.Add(time.Second).Format(time.RFC3339) })
		}},
		{"attestation-absent", "signed_attestation_missing", func(t *testing.T, p *trial) { p.att = nil }},
		{"attestation-malformed", "signed_attestation_invalid", func(t *testing.T, p *trial) { p.att = []byte("{}") }},
		{"attestation-large", "signed_attestation_too_large", func(t *testing.T, p *trial) { p.att = bytes.Repeat([]byte("x"), 8193) }},
		{"attestation-id", "signed_attestation_mismatch", func(t *testing.T, p *trial) {
			p.raw, _ = stationResign(t, p.raw, 3, func(b map[string]any) { b["author_attestation"] = "sha256:" + strings.Repeat("0", 64) })
		}},
		{"metadata", "signed_metadata_mismatch", func(t *testing.T, p *trial) {
			p.raw, _ = stationResign(t, p.raw, 3, func(b map[string]any) { b["arguments"].(map[string]any)["kind"] = "question" })
		}},
		{"malformed", "signed_envelope_invalid", func(t *testing.T, p *trial) { p.raw = []byte("{}") }},
		{"signature", "signed_signature_invalid", func(t *testing.T, p *trial) {
			v, _ := record.Parse(p.raw, 65536, 16)
			e := v.(map[string]any)
			sig := e["signatures"].([]any)[0].(map[string]any)
			b, _ := base64.RawURLEncoding.DecodeString(sig["sig"].(string))
			b[0] ^= 1
			sig["sig"] = base64.RawURLEncoding.EncodeToString(b)
			p.raw, _ = record.Canonical(e)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s.control(t)
			p := trial{raw: append([]byte{}, s.raw...), att: append([]byte{}, s.att...), to: append([]string{}, s.to...), text: s.body}
			tc.edit(t, &p)
			s.refused(t, "alice", tc.code, s.args(t, p.raw, p.att, p.to, p.text))
			s.control(t)
		})
	}
}

// Claim: all admitted event/key publishers without a signed form are refused without carrier or state effects beside signed post controls.
func TestStationPublishingGate(t *testing.T) {
	s := newSignedStation(t)
	s.policy(t, stationMap(uint32(os.Geteuid())))
	attachment := envelopeFile(t, []byte("attachment"))
	cases := [][]string{
		{"topic", "new", s.f.rooms["alice"], "beta", "B"},
		{"attach", s.f.rooms["alice"], "--topic", s.root, attachment},
		{"attach", s.f.rooms["alice"], "--topic", s.root, "--pin", attachment},
		{"hook", "stop", "--runtime", "codex"},
		{"hook", "session-start", "--runtime", "codex"},
		{"hook", "prompt", "--runtime", "muse"},
		{"hook", "session-end", "--runtime", "codex"},
		{"wait", "--status", "idle", "--heartbeat", "1m", "--runtime", "none", "--hooks", "none"},
		{"who", "--stale-after", "1m", "--alert=true", "--alert-room", s.f.rooms["alice"], "--keeper", "alice"},
		{"e2ee", "init"},
	}
	for i, args := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			s.control(t)
			plan, err := daemon.Authorize(args, "alice")
			if err != nil || !plan.Publishes {
				t.Fatal("publisher not classified")
			}
			s.refused(t, "alice", "signed_command_refused", args)
			s.control(t)
		})
	}
	for _, args := range [][]string{{"whoami"}, {"who", "--stale-after", "1m", "--alert=false"}, {"wait", "--timeout", "1s"}, {"watch"}, {"e2ee", "identity", "show"}, {"read", s.f.rooms["alice"]}, {"inbox"}, {"files", s.f.rooms["alice"]}, {"topics", s.f.rooms["alice"]}, {"bind", "status"}, {"status", "admission", "--json"}, {"post-operation", "status", "example"}, {"daemon", "status"}} {
		plan, err := daemon.Authorize(args, "alice")
		if err != nil || plan.Publishes {
			t.Fatal("nonpublisher classification differs")
		}
	}
}

// Claim: unavailable selected maps close publishing for both stations, and a repaired map immediately permits the signed control.
func TestStationAuthorPolicyAvailability(t *testing.T) {
	s := newSignedStation(t)
	good := stationMap(uint32(os.Geteuid()))
	for _, raw := range [][]byte{
		bytes.Replace(good, []byte(`"uid":`), []byte(`"extra":0,"uid":`), 1),
		bytes.Replace(good, []byte(`"uid":`), []byte(`"UID":`), 1),
		[]byte(fmt.Sprintf(`{"version":"a2a-station-authors/1","authors":[{"uid":%d,"author_key":%q},{"uid":%d,"author_key":%q}]}`, os.Geteuid(), signedPostKey, os.Geteuid(), signedPostKey)),
	} {
		s.policy(t, good)
		s.control(t)
		s.policy(t, raw)
		for _, name := range []string{"alice", "bob"} {
			s.refused(t, name, "station_authors_unavailable", []string{"post", s.f.rooms[name], "--topic", s.root, "unsigned"})
		}
		s.policy(t, good)
		s.control(t)
	}
	for _, mode := range []string{"group-write", "symlink", "missing"} {
		t.Run(mode, func(t *testing.T) {
			s.policy(t, good)
			s.control(t)
			path := filepath.Join(t.TempDir(), "authors.json")
			if err := os.WriteFile(path, good, 0600); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "group-write":
				if err := os.Chmod(path, 0664); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := path
				path += ".link"
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "missing":
				path += ".missing"
			}
			selector, _ := json.Marshal(map[string]string{"authors_path": path})
			if err := os.WriteFile(filepath.Join(s.f.home, "station-authors.json"), selector, 0600); err != nil {
				t.Fatal(err)
			}
			s.f.daemon.loadStationAuthors = nil
			for _, name := range []string{"alice", "bob"} {
				s.refused(t, name, "station_authors_unavailable", []string{"post", s.f.rooms[name], "--topic", s.root, "unsigned"})
			}
			s.policy(t, good)
			s.control(t)
		})
	}
	for _, selector := range []string{`{}`, `{"authors_path":"relative"}`, `{"Authors_path":"/etc/map"}`} {
		s.policy(t, good)
		s.control(t)
		if err := os.WriteFile(filepath.Join(s.f.home, "station-authors.json"), []byte(selector), 0600); err != nil {
			t.Fatal(err)
		}
		s.refused(t, "bob", "station_authors_unavailable", []string{"post", s.f.rooms["bob"], "--topic", s.root, "unsigned"})
	}
	s.policy(t, good)
	s.control(t)
}

// Claim: the connecting kernel UID selects policy independently of station identity, while unmapped and unconfigured stations retain unsigned posting.
func TestStationPeerUIDBinding(t *testing.T) {
	s := newSignedStation(t)
	s.policy(t, stationMap(uint32(os.Geteuid())))
	s.control(t)
	for _, name := range []string{"alice", "bob"} {
		s.refused(t, name, "signed_post_required", []string{"post", s.f.rooms[name], "--topic", s.root, "unsigned"})
	}
	s.policy(t, stationMap(uint32(os.Geteuid())+1))
	unmapped := s.f.run("alice", "", "post", s.f.rooms["alice"], "--topic", s.root, "unsigned").ok(t)
	if err := os.Remove(filepath.Join(s.f.home, "station-authors.json")); err != nil {
		t.Fatal(err)
	}
	unconfigured := s.f.run("alice", "", "post", s.f.rooms["alice"], "--topic", s.root, "unsigned").ok(t)
	first, firstOK := s.f.envs["alice"].hs.eventByID(strings.TrimSpace(unmapped.stdout))
	second, secondOK := s.f.envs["alice"].hs.eventByID(strings.TrimSpace(unconfigured.stdout))
	if !firstOK || !secondOK || !bytes.Equal(first.Content, second.Content) {
		t.Fatal("unmapped posting differs from unconfigured posting")
	}
	s.policy(t, stationMap(uint32(os.Geteuid())))
	s.control(t)
}
