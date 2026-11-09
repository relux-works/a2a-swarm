package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func seedStateJSON(t *testing.T, p Paths, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p.StateFile("a")), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.StateFile("a"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func readStateJSON(t *testing.T, p Paths) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(p.StateFile("a"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func sameStateJSON(a, b []byte) bool {
	var x, y any
	da, db := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	da.UseNumber()
	db.UseNumber()
	return da.Decode(&x) == nil && db.Decode(&y) == nil && reflect.DeepEqual(x, y)
}

// Claims ordinary persistence updates the known cursor both with and without
// future top-level data, retaining the complete opaque value. This is storage
// compatibility only, not future enrollment authority or nested typed preservation.
func TestStateJSONOrdinaryUpdate(t *testing.T) {
	const future = `{"attempt":{"generation":9007199254740993,"steps":[true,null,"synthetic"]}}`
	for _, opaque := range []bool{false, true} {
		name := "known-only"
		if opaque {
			name = "future-top-level"
		}
		t.Run(name, func(t *testing.T) {
			p := testPaths(t)
			body := `{"next_batch":"before"}`
			if opaque {
				body = `{"next_batch":"before","future_enrollment":` + future + `}`
			}
			seedStateJSON(t, p, body)
			if _, err := UpdateState(p, "a", func(s *State) error { s.NextBatch = "after"; return nil }); err != nil {
				t.Fatal(err)
			}
			got := readStateJSON(t, p)
			if string(got["next_batch"]) != `"after"` {
				t.Fatal("intended known cursor update did not persist")
			}
			t.Log("known cursor update persisted")
			if opaque && !sameStateJSON(got["future_enrollment"], []byte(future)) {
				t.Fatal("ordinary known-field update lost or changed opaque top-level member")
			}
		})
	}
}

// Claims each existing persistence route retains complete top-level values over
// repeated known-field mutations, with exact large-number information and 0600
// mode. Private synthetic files do not exercise a live hook or future schema.
func TestStateJSONPersistenceRoutes(t *testing.T) {
	values := []string{
		`{"nested":{"a":[null,true,false,9007199254740993],"b":"<>&\u263a"}}`,
		`[1,{"nested":["a",null]},false]`, `"opaque <>& ☺"`, `null`, `true`, `false`,
		`9007199254740993`, `-9007199254740993123456789`, `1.234567890123456789e+120`,
	}
	routes := map[string]func(Paths, func(*State) error) error{
		"direct": func(p Paths, f func(*State) error) error {
			s, e := LoadState(p, "a")
			if e != nil {
				return e
			}
			if e = f(&s); e != nil {
				return e
			}
			return SaveState(p, "a", s)
		},
		"update": func(p Paths, f func(*State) error) error { _, e := UpdateState(p, "a", f); return e },
		"bounded-hook": func(p Paths, f func(*State) error) error {
			_, e := UpdateStateBy(p, "a", time.Now().Add(time.Second), f)
			return e
		},
		"until-bounded": func(p Paths, f func(*State) error) error {
			_, e := UpdateStateUntil(p, "a", time.Now().Add(time.Second), f)
			return e
		},
		"until-zero": func(p Paths, f func(*State) error) error { _, e := UpdateStateUntil(p, "a", time.Time{}, f); return e },
	}
	for route, update := range routes {
		t.Run(route, func(t *testing.T) {
			for i, value := range values {
				t.Run(fmt.Sprint(i), func(t *testing.T) {
					p := testPaths(t)
					seedStateJSON(t, p, `{"next_batch":"start","threads":["existing"],"future":`+value+`}`)
					for n := 0; n < 3; n++ {
						cursor := fmt.Sprint(n)
						if err := update(p, func(s *State) error { s.NextBatch = cursor; return nil }); err != nil {
							t.Fatal(err)
						}
						got := readStateJSON(t, p)
						if !sameStateJSON(got["future"], []byte(value)) {
							t.Fatal("opaque JSON information changed")
						}
						if string(got["next_batch"]) != `"`+cursor+`"` || !sameStateJSON(got["threads"], []byte(`["existing"]`)) {
							t.Fatal("known or unrelated field lost")
						}
						st, err := os.Stat(p.StateFile("a"))
						if err != nil {
							t.Fatal(err)
						}
						if st.Mode().Perm() != 0600 {
							t.Fatal("state mode changed")
						}
					}
				})
			}
		})
	}
}

// Claims accepted case variants update the typed values; clearing pointer,
// scalar, raw, slice and map fields cannot resurrect their original opaque
// spellings. Unicode long-s is a real encoding/json folded-name neighbor.
func TestStateJSONKnownClearAndCaseFold(t *testing.T) {
	p := testPaths(t)
	seedStateJSON(t, p, `{"LEaSe":{"holder":"synthetic","expires":"2026-01-01T00:00:00Z"},"NEXT_BATCH":"old","HOOKS_PAUSED":true,"PENDING":{"old":true},"ACKED":["old"],"SUBSCRIPTIONS":{"* ":["request"]},"liveness_ſource":"old","future":null}`)
	s, err := LoadState(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if s.Lease == nil || s.NextBatch != "old" || !s.HooksPaused || s.LivenessSource != "old" || len(s.LegacyPending) == 0 || len(s.LegacyAcked) == 0 || len(s.LegacySubscriptions) == 0 {
		t.Fatal("accepted case-fold input did not load")
	}
	s.Lease = nil
	s.NextBatch = ""
	s.HooksPaused = false
	s.LivenessSource = ""
	s.LegacyPending = nil
	s.LegacyAcked = nil
	s.LegacySubscriptions = nil
	if err := SaveState(p, "a", s); err != nil {
		t.Fatal(err)
	}
	got := readStateJSON(t, p)
	for _, key := range []string{"LEaSe", "lease", "NEXT_BATCH", "next_batch", "HOOKS_PAUSED", "hooks_paused", "PENDING", "pending", "ACKED", "acked", "SUBSCRIPTIONS", "subscriptions", "liveness_ſource", "liveness_source"} {
		if _, ok := got[key]; ok {
			t.Fatalf("cleared/aliased field resurrected: %s", key)
		}
	}
	if string(got["future"]) != "null" {
		t.Fatal("opaque neighbor lost")
	}
	if _, err := UpdateState(p, "a", func(s *State) error {
		s.Lease = &Lease{Holder: "new"}
		s.HooksPaused = true
		s.NextBatch = "new"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := LoadState(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Lease == nil || updated.Lease.Holder != "new" || updated.NextBatch != "new" || !updated.HooksPaused {
		t.Fatal("positive known updates failed")
	}
	if _, err := UpdateState(p, "a", func(s *State) error { s.Lease = nil; s.HooksPaused = false; s.NextBatch = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	got = readStateJSON(t, p)
	for _, key := range []string{"lease", "hooks_paused", "next_batch"} {
		if _, ok := got[key]; ok {
			t.Fatal("repeated clear resurrected field")
		}
	}
}

// Claims every current flat exported State field's folded spelling is handled
// as typed JSON, including fields added later. The reference is the
// standard typed decoder/encoder, not the opaque classifier's verdict.
func TestStateJSONCurrentSchemaCaseVariants(t *testing.T) {
	typ := reflect.TypeFor[State]()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		t.Run(name, func(t *testing.T) {
			if name == "migration" {
				// Migration is typed authority: null must not reopen, and even a valid
				// case-folded record cannot pass through an ordinary save.
				p := testPaths(t)
				seedStateJSON(t, p, `{"MIGRATION":null}`)
				if _, err := LoadState(p, "a"); err == nil {
					t.Fatal("null migration accepted")
				}
				seedStateJSON(t, p, `{"MIGRATION":{"phase":"in_progress","courier_base":"/fixture","at":"2026-01-02T03:04:05Z"}}`)
				st, err := LoadState(p, "a")
				if err != nil || st.Migration == nil {
					t.Fatal("case-folded migration not typed", err)
				}
				if err = SaveState(p, "a", st); !errors.Is(err, ErrIdentityMigrated) {
					t.Fatal("ordinary save crossed migration fence", err)
				}
				return
			}
			body := `{"` + strings.ToUpper(name) + `":null}`
			if name == "host_enrollment" {
				// Enrollment null is refused by LoadState; use a valid typed
				// value to exercise the same alias/canonicalization contract.
				_, attachment, _ := hostFixture()
				raw, err := json.Marshal(HostEnrollment{Version: 1, Attachment: attachment})
				if err != nil {
					t.Fatal(err)
				}
				body = `{"` + strings.ToUpper(name) + `":` + string(raw) + `}`
			}
			if name == "monitor" {
				raw, err := json.Marshal(monitorRecord())
				if err != nil {
					t.Fatal(err)
				}
				body = `{"MONITOR":` + string(raw) + `}`
			}
			if name == "monitor_owner" || name == "monitor_config" {
				// These authority members also refuse null. Exercise folded
				// top-level spelling with valid closed-schema payloads.
				var value any = MonitorConfig{OwnerTTL: DefaultOwnerTTL}
				if name == "monitor_owner" {
					now := time.Now().UTC()
					value = MonitorOwner{Runtime: "muse", SessionIDHash: ownerHash("fixture"), FirstSeen: now, LastHookAt: now}
				}
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				body = `{"` + strings.ToUpper(name) + `":` + string(raw) + `}`
			}
			var reference State
			if err := json.Unmarshal([]byte(body), &reference); err != nil {
				t.Fatal(err)
			}
			expected, err := json.Marshal(reference)
			if err != nil {
				t.Fatal(err)
			}
			p := testPaths(t)
			seedStateJSON(t, p, body)
			s, err := LoadState(p, "a")
			if err != nil {
				t.Fatal(err)
			}
			if err := SaveState(p, "a", s); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(p.StateFile("a"))
			if err != nil {
				t.Fatal(err)
			}
			// Every compatible save now adds fresh writer consistency metadata.
			// Compare the typed payload separately from that dynamic seal/time.
			var gm, em map[string]json.RawMessage
			if json.Unmarshal(got, &gm) != nil || json.Unmarshal(expected, &em) != nil {
				t.Fatal("decode comparison")
			}
			delete(gm, "writer")
			delete(em, "writer")
			got, _ = json.Marshal(gm)
			expected, _ = json.Marshal(em)
			if !sameStateJSON(got, expected) {
				t.Fatal("known-field alias survived beyond typed schema")
			}
		})
	}
}

// Claims invalid JSON/known types refuse before the callback and leave seeded
// disk bytes untouched without including opaque sentinel values in errors.
// A valid neighbor is updated so an implementation that refuses all is rejected.
func TestStateJSONInvalidAndAbort(t *testing.T) {
	for name, body := range map[string]string{
		"syntax":         `{"future":"OPAQUE_SENTINEL",`,
		"known-type":     `{"future":"OPAQUE_SENTINEL","next_batch":{}}`,
		"top-level-type": `["OPAQUE_SENTINEL"]`,
	} {
		t.Run(name, func(t *testing.T) {
			p := testPaths(t)
			seedStateJSON(t, p, body)
			called := false
			_, err := UpdateState(p, "a", func(s *State) error { called = true; s.NextBatch = "bad"; return nil })
			if err == nil || called || !strings.Contains(err.Error(), "not valid state JSON") || strings.Contains(err.Error(), "OPAQUE_SENTINEL") {
				t.Fatal("wrong decode refusal or sentinel diagnostic")
			}
			if name == "known-type" {
				var typeErr *json.UnmarshalTypeError
				if !errors.As(err, &typeErr) {
					t.Fatal("lost known-type diagnostic")
				}
			}
			if name == "syntax" {
				var syntaxErr *json.SyntaxError
				if !errors.As(err, &syntaxErr) {
					t.Fatal("lost syntax diagnostic")
				}
			}
			got, e := os.ReadFile(p.StateFile("a"))
			if e != nil {
				t.Fatal(e)
			}
			if string(got) != body {
				t.Fatal("invalid input overwritten")
			}
		})
	}
	p := testPaths(t)
	const original = `{"next_batch":"before","future":{"secret":"OPAQUE_SENTINEL"}}`
	seedStateJSON(t, p, original)
	refusal := errors.New("synthetic callback refusal")
	_, err := UpdateState(p, "a", func(s *State) error { s.NextBatch = "refused"; s.Lease = &Lease{Holder: "refused"}; return refusal })
	if !errors.Is(err, refusal) {
		t.Fatal("callback refusal lost")
	}
	got, e := os.ReadFile(p.StateFile("a"))
	if e != nil {
		t.Fatal(e)
	}
	if string(got) != original {
		t.Fatal("callback abort wrote state")
	}
	if _, err := UpdateState(p, "a", func(s *State) error { s.NextBatch = "accepted"; return nil }); err != nil {
		t.Fatal(err)
	}
	if string(readStateJSON(t, p)["next_batch"]) != `"accepted"` {
		t.Fatal("valid neighbor refused")
	}
}

// Claims loading owns its raw bytes and saving State value copies never mutates
// retained opaque data. Concurrent saves use distinct files, not unlocked
// concurrent writes to one State file; serialized updates have a separate test.
func TestStateJSONCopyAndBufferIsolation(t *testing.T) {
	data := []byte(`{"next_batch":"start","future":{"n":9007199254740993}}`)
	var original State
	if err := unmarshalState(data, &original); err != nil {
		t.Fatal(err)
	}
	for i := range data {
		data[i] = 'x'
	}
	p := testPaths(t)
	var wg sync.WaitGroup
	const workers = 2
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			copy := original
			copy.NextBatch = fmt.Sprint(i)
			errs <- SaveState(p, fmt.Sprint(i), copy)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < workers; i++ {
		s, err := LoadState(p, fmt.Sprint(i))
		if err != nil {
			t.Fatal(err)
		}
		if s.NextBatch != fmt.Sprint(i) || !sameStateJSON(s.opaque["future"], []byte(`{"n":9007199254740993}`)) {
			t.Fatal("value copies interfered")
		}
	}
	if original.NextBatch != "start" || !sameStateJSON(original.opaque["future"], []byte(`{"n":9007199254740993}`)) {
		t.Fatal("saving copy mutated retained state")
	}
	// A second decode into the same private collector must not keep absent opaque keys.
	if err := unmarshalState([]byte(`{"next_batch":"fresh"}`), &original); err != nil {
		t.Fatal(err)
	}
	if len(original.opaque) != 0 {
		t.Fatal("collector resurrected stale opaque member")
	}
}

// Claims the existing serialized UpdateState route preserves one future value
// alongside all independent known updates. This is not a new lock/durability
// protocol or evidence about dormant old binaries.
func TestStateJSONConcurrentUpdates(t *testing.T) {
	p := testPaths(t)
	const future = `{"generation":9007199254740993}`
	seedStateJSON(t, p, `{"future":`+future+`}`)
	const workers = 2
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			_, err := UpdateState(p, "a", func(s *State) error { s.AddThread(fmt.Sprint(i)); return nil })
			errs <- err
		}(i)
	}
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	s, err := LoadState(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Threads) != workers {
		t.Fatal("known concurrent update lost")
	}
	if !sameStateJSON(readStateJSON(t, p)["future"], []byte(future)) {
		t.Fatal("serialized update lost opaque data")
	}
}

// Claims several unknown names survive together, including Go field names that
// are not accepted aliases of tagged JSON fields. Empty object/null inputs retain
// their existing typed-State semantics; no new duplicate-key policy is asserted.
func TestStateJSONUnknownNamesAndEmpty(t *testing.T) {
	p := testPaths(t)
	const body = `{"NextBatch":"opaque-neighbor","opaque":false,"future_object":{"x":1},"future_array":[],"future_null":null,"future_scalar":9007199254740993,"next_batch":"typed"}`
	seedStateJSON(t, p, body)
	if _, err := UpdateState(p, "a", func(s *State) error {
		if s.NextBatch != "typed" {
			t.Fatal("typed cursor not decoded")
		}
		s.NextBatch = "updated"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var expected map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &expected); err != nil {
		t.Fatal(err)
	}
	got := readStateJSON(t, p)
	for key, value := range expected {
		if key == "next_batch" {
			continue
		}
		if !sameStateJSON(got[key], value) {
			t.Fatalf("unknown key lost: %s", key)
		}
	}
	if string(got["next_batch"]) != `"updated"` {
		t.Fatal("unknown neighbor shadowed typed cursor")
	}
	for _, empty := range []string{`{}`, `null`} {
		t.Run(empty, func(t *testing.T) {
			p := testPaths(t)
			seedStateJSON(t, p, empty)
			if _, err := UpdateState(p, "a", func(s *State) error { s.NextBatch = "from-empty"; return nil }); err != nil {
				t.Fatal(err)
			}
			if string(readStateJSON(t, p)["next_batch"]) != `"from-empty"` {
				t.Fatal("valid empty state refused")
			}
		})
	}
}

// Claims a marshal failure on a known RawMessage still leaves a valid existing
// file byte-for-byte intact and omits unknown sentinel content from diagnostics.
// A corrected known value then succeeds without losing the opaque neighbor.
func TestStateJSONSaveFailurePreservesDisk(t *testing.T) {
	p := testPaths(t)
	const body = `{"future":"OPAQUE_SENTINEL","next_batch":"valid"}`
	seedStateJSON(t, p, body)
	s, err := LoadState(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	s.LegacyPending = json.RawMessage(`{`)
	err = SaveState(p, "a", s)
	if err == nil || strings.Contains(err.Error(), "OPAQUE_SENTINEL") {
		t.Fatal("wrong marshal failure diagnostic")
	}
	var marshalErr *json.MarshalerError
	if !errors.As(err, &marshalErr) {
		t.Fatal("expected known RawMessage marshal error")
	}
	got, e := os.ReadFile(p.StateFile("a"))
	if e != nil {
		t.Fatal(e)
	}
	if string(got) != body {
		t.Fatal("marshal failure changed disk")
	}
	s.LegacyPending = json.RawMessage(`{"valid":true}`)
	if err := SaveState(p, "a", s); err != nil {
		t.Fatal(err)
	}
	gotMap := readStateJSON(t, p)
	if string(gotMap["future"]) != `"OPAQUE_SENTINEL"` || !sameStateJSON(gotMap["pending"], s.LegacyPending) {
		t.Fatal("valid neighbor failed")
	}
}

// Claims opaque preservation coexists with the base's typed enrollment and immutable custody fences.
func TestStateJSONHostEnrollmentComposition(t *testing.T) {
	for _, key := range []string{"host_enrollment", "HOST_ENROLLMENT", "hoſt_enrollment"} {
		t.Run(key, func(t *testing.T) {
			_, attachment, _ := hostFixture()
			enrollment := HostEnrollment{Version: 1, Attachment: attachment}
			raw, err := json.Marshal(enrollment)
			if err != nil {
				t.Fatal(err)
			}
			p := testPaths(t)
			const future = `{"attempt":[9007199254740993,null,{"next":true}]}`
			seedStateJSON(t, p, `{"`+key+`":`+string(raw)+`,"future":`+future+`,"next_batch":"before"}`)
			s, err := LoadState(p, "a")
			if err != nil || s.HostEnrollment == nil || !s.HostEnrollment.same(enrollment) {
				t.Fatalf("typed enrollment not recognized: %v", err)
			}
			if len(s.opaque) != 1 || !sameStateJSON(s.opaque["future"], []byte(future)) {
				t.Fatal("current enrollment field retained as opaque or future lost")
			}
			for i := 0; i < 2; i++ {
				if _, err := UpdateStateBy(p, "a", time.Now().Add(time.Second), func(s *State) error {
					s.NextBatch = fmt.Sprint(i)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			got := readStateJSON(t, p)
			if !sameStateJSON(got["future"], []byte(future)) || !sameStateJSON(got["host_enrollment"], raw) || string(got["next_batch"]) != `"1"` {
				t.Fatal("composed update lost typed or opaque state")
			}
			if key != "host_enrollment" && got[key] != nil {
				t.Fatal("enrollment case alias resurrected")
			}
			before, err := os.ReadFile(p.StateFile("a"))
			if err != nil {
				t.Fatal(err)
			}
			for _, drop := range []bool{true, false} {
				_, err := UpdateState(p, "a", func(s *State) error {
					s.NextBatch = "refused"
					if drop {
						s.HostEnrollment = nil
					} else {
						e := *s.HostEnrollment
						e.Attachment.Reference = "replacement"
						s.HostEnrollment = &e
					}
					return nil
				})
				if !errors.Is(err, ErrHostEnrollment) {
					t.Fatalf("prior enrollment fence changed: %v", err)
				}
				after, err := os.ReadFile(p.StateFile("a"))
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("refused custody mutation changed disk")
				}
			}
		})
	}
}

// Optional publication-fence corruption does not reject the delivery cursor or
// hook acknowledgement. A well-formed active fence is preserved as the control.
func TestOwnStatusWriteDecodePreservesDelivery(t *testing.T) {
	for _, value := range []string{`{"token":"writer","until":"2026-11-01T12:00:00Z"}`, `{"until":"invalid"}`, `{"until":7}`, `"invalid"`, `[]`} {
		t.Run(value, func(t *testing.T) {
			var st State
			raw := `{"next_batch":"cursor","hook_sessions":[{"id":"session","rung":["$mail"]}],"own_status_write":` + value + `}`
			if err := unmarshalState([]byte(raw), &st); err != nil {
				t.Fatal("publication fence blocked delivery state", err)
			}
			if st.NextBatch != "cursor" || len(st.HookSession("session").Rung) != 1 || st.HookSession("session").Rung[0] != "$mail" {
				t.Fatal("publication fence changed delivery state")
			}
			if st.OwnStatusWrite == nil || (st.OwnStatusWrite.Token == "writer") != (value == `{"token":"writer","until":"2026-11-01T12:00:00Z"}`) {
				t.Fatal("wrong fence decode")
			}
		})
	}
}
