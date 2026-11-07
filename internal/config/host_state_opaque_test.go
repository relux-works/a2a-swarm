package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func seedHostOpaque(t *testing.T, p Paths, name string, value json.RawMessage) {
	t.Helper()
	data, err := os.ReadFile(p.StateFile(name))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	fields["future_data"] = value
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p.StateFile(name), data, SecretFileMode); err != nil {
		t.Fatal(err)
	}
}

func requireHostOpaque(t *testing.T, p Paths, name string, want json.RawMessage) {
	t.Helper()
	data, err := os.ReadFile(p.StateFile(name))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields["future_data"]) == 0 {
		t.Fatal("Host save lost opaque top-level data")
	}
	decode := func(b []byte) any {
		var v any
		d := json.NewDecoder(bytes.NewReader(b))
		d.UseNumber()
		if err := d.Decode(&v); err != nil {
			t.Fatal("opaque data missing or invalid")
		}
		return v
	}
	if !reflect.DeepEqual(decode(fields["future_data"]), decode(want)) {
		t.Fatal("Host save lost opaque top-level data")
	}
}

// Proves real prepared/UNKNOWN/accepted writes retain precise opaque DATA next
// to typed custody. The same file keeps its original receipt and unread cursor;
// this fixture does not establish an external Host observation or live authority.
func TestHostStateOpaqueTransaction(t *testing.T) {
	p, _, a, now, b := hostNotificationFixture(t)
	value := json.RawMessage(`{"n":9007199254740993,"nested":[null,true,"<>&"],"host_enrollment":{"version":999}}`)
	seedHostOpaque(t, p, "member", value)
	s, err := LoadState(p, "member")
	if err != nil {
		t.Fatal(err)
	}
	ref := s.HostEnrollment.ReceiptRef
	k, err := s.PrepareHostNotification(a, a.Scope, b, strings.Repeat("a", 64), now, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveState(p, "member", s); err != nil {
		t.Fatal(err)
	}
	requireHostOpaque(t, p, "member", value)
	stale := s
	if err = s.BeginHostNotification(a, a.Scope, k, "original-request", now); err != nil {
		t.Fatal(err)
	}
	if err = SaveState(p, "member", s); err != nil {
		t.Fatal(err)
	}
	requireHostOpaque(t, p, "member", value)
	before, err := os.ReadFile(p.StateFile("member"))
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveState(p, "member", stale); !errors.Is(err, ErrHostEnrollment) {
		t.Fatal("opaque codec admitted stale Host writer")
	}
	after, err := os.ReadFile(p.StateFile("member"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("stale Host writer changed file")
	}
	if err = s.AcceptHostNotification(k, "1", "not-evidenced", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = SaveState(p, "member", s); err != nil {
		t.Fatal(err)
	}
	requireHostOpaque(t, p, "member", value)
	got, err := LoadState(p, "member")
	if err != nil || got.NextBatch != "unread-retained" || got.HostEnrollment.ReceiptRef != ref || got.Notification.Reservation != nil || got.HostEnrollment.Attempts.Entries[0].State != "accepted" {
		t.Fatal("opaque composition changed typed custody or receipt")
	}
}

// Proves ordinary legacy and v1 Host writes retain opaque data while its nested
// enrollment-shaped content cannot select or change typed enrollment.
func TestHostStateOpaqueLegacyAndV1(t *testing.T) {
	for _, enrolled := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "host-v1"}[enrolled], func(t *testing.T) {
			p := testPaths(t)
			scope, a, now := hostFixture()
			s := State{NextBatch: "before"}
			if enrolled {
				s.Lease = &a.Lease
				if err := s.EnrollHost(a, scope, now); err != nil {
					t.Fatal(err)
				}
			}
			if err := SaveState(p, "member", s); err != nil {
				t.Fatal(err)
			}
			value := json.RawMessage(`{"host_enrollment":{"version":2},"n":9007199254740993}`)
			seedHostOpaque(t, p, "member", value)
			got, err := UpdateState(p, "member", func(x *State) error { x.NextBatch = "after"; return nil })
			if err != nil || got.NextBatch != "after" || (got.HostEnrollment != nil) != enrolled {
				t.Fatal("opaque data changed typed admission")
			}
			requireHostOpaque(t, p, "member", value)
			if enrolled && got.HostEnrollment.Version != 1 {
				t.Fatal("opaque data upgraded custody")
			}
		})
	}
}

// Proves the public v2 writer counts retained opaque bytes in its total bound.
// The oversized input is seeded only in this isolated fixture; refusal must
// preserve the complete prior file. A small neighboring value then saves.
func TestHostStateOpaqueCapacity(t *testing.T) {
	p, _, _, _, _ := hostNotificationFixture(t)
	value, err := json.Marshal(strings.Repeat("x", hostStateMaxBytes))
	if err != nil {
		t.Fatal(err)
	}
	seedHostOpaque(t, p, "member", value)
	s, err := LoadState(p, "member")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p.StateFile("member"))
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveState(p, "member", s); !errors.Is(err, ErrHostStateCapacity) {
		t.Fatalf("opaque bytes escaped Host capacity: %v", err)
	}
	after, err := os.ReadFile(p.StateFile("member"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("capacity refusal changed file")
	}
	seedHostOpaque(t, p, "member", json.RawMessage(`9007199254740993`))
	s, err = LoadState(p, "member")
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveState(p, "member", s); err != nil {
		t.Fatal(err)
	}
	requireHostOpaque(t, p, "member", json.RawMessage(`9007199254740993`))
}
