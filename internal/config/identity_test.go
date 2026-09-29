package config

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// Proves ComposeIdentity joins valid parts with '_' into a valid agent name
// (the longest parts that still fit 64 characters included), and rejects each
// invalid part with an error naming that part, and a composite longer than 64.
func TestComposeIdentity(t *testing.T) {
	name, err := ComposeIdentity("mini", "a2a-swarm", "orch")
	if err != nil || name != "mini_a2a-swarm_orch" {
		t.Fatalf("ComposeIdentity = %q, %v", name, err)
	}
	if err := ValidateName(name); err != nil {
		t.Fatalf("composite rejected by ValidateName: %v", err)
	}
	long, err := ComposeIdentity(strings.Repeat("h", 24), strings.Repeat("p", 14), strings.Repeat("r", 24))
	if err != nil || len(long) != 64 {
		t.Fatalf("64-character composite = %q, %v", long, err)
	}
	for _, c := range []struct{ host, project, role, part string }{
		{"", "p", "r", "host"},
		{"Mini", "p", "r", "host"},
		{"mi_ni", "p", "r", "host"},
		{strings.Repeat("h", 25), "p", "r", "host"},
		{"h", "", "r", "project"},
		{"h", "a.b", "r", "project"},
		{"h", strings.Repeat("p", 41), "r", "project"},
		{"h", "p", "", "role"},
		{"h", "p", "or ch", "role"},
		{"h", "p", "../x", "role"},
		{"h", "p", strings.Repeat("r", 25), "role"},
	} {
		if _, err := ComposeIdentity(c.host, c.project, c.role); err == nil || !strings.Contains(err.Error(), "invalid "+c.part) {
			t.Errorf("ComposeIdentity(%q, %q, %q) = %v, want an invalid %s error", c.host, c.project, c.role, err, c.part)
		}
	}
	if _, err := ComposeIdentity(strings.Repeat("h", 24), strings.Repeat("p", 15), strings.Repeat("r", 24)); err == nil || !strings.Contains(err.Error(), "longer than the 64") {
		t.Fatalf("65-character composite = %v, want a length error", err)
	}
}

// Proves the host slug round-trips through a 0600 file holding slug+newline,
// is ErrNoHost when unset, is not replaced by a different slug without force
// (the file stays), is replaced with force, and an invalid slug is refused.
func TestHostFile(t *testing.T) {
	p := testPaths(t)
	if _, err := LoadHost(p); !errors.Is(err, ErrNoHost) {
		t.Fatalf("LoadHost unset = %v", err)
	}
	if changed, err := SaveHost(p, "mini", false); err != nil || !changed {
		t.Fatalf("SaveHost = %v, %v", changed, err)
	}
	data, _ := os.ReadFile(p.HostFile())
	st, _ := os.Stat(p.HostFile())
	if string(data) != "mini\n" || st.Mode().Perm() != 0o600 {
		t.Fatalf("host file %q mode %v", data, st.Mode())
	}
	if changed, err := SaveHost(p, "mini", false); err != nil || changed {
		t.Fatalf("same slug again = %v, %v", changed, err)
	}
	if _, err := SaveHost(p, "laptop", false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("different slug without force = %v", err)
	}
	if h, _ := LoadHost(p); h != "mini" {
		t.Fatalf("slug changed without force: %q", h)
	}
	if _, err := SaveHost(p, "laptop", true); err != nil {
		t.Fatal(err)
	}
	if h, _ := LoadHost(p); h != "laptop" {
		t.Fatalf("slug with force = %q", h)
	}
	if _, err := SaveHost(p, "Bad_Slug", true); err == nil {
		t.Fatal("invalid slug accepted")
	}
}

// Proves the lease: the holder refreshes it, another holder is refused while
// it is live (not expired, not dead) and takes it over when it expired or its
// holder is dead; release clears only its own or a lapsed lease, or with force.
func TestLease(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	var s State
	if err := s.Acquire("a", "s1", "h1", 1, now, LeaseTTL, nil); err != nil || !s.Lease.Expires.Equal(now.Add(LeaseTTL)) {
		t.Fatalf("acquire = %v %+v", err, s.Lease)
	}
	if err := s.Acquire("a", "s1", "h1", 1, now.Add(time.Minute), LeaseTTL, nil); err != nil || !s.Lease.Expires.Equal(now.Add(time.Minute+LeaseTTL)) {
		t.Fatalf("refresh = %v %+v", err, s.Lease)
	}
	var held *LeaseHeldError
	err := s.Acquire("a", "s2", "h1", 2, now.Add(2*time.Minute), LeaseTTL, nil)
	if !errors.As(err, &held) || !strings.Contains(err.Error(), "identity a is held by s1 until 2026-09-29T12:11:00Z") || s.Lease.Holder != "s1" {
		t.Fatalf("second holder = %v %+v", err, s.Lease)
	}
	if err := s.Acquire("a", "s2", "h1", 2, now.Add(2*time.Minute), LeaseTTL, func(h string) bool { return h == "s1" }); err != nil || s.Lease.Holder != "s2" {
		t.Fatalf("dead holder not taken over: %v %+v", err, s.Lease)
	}
	if err := s.Acquire("a", "s1", "h1", 1, now.Add(2*time.Minute+LeaseTTL), LeaseTTL, nil); err != nil || s.Lease.Holder != "s1" {
		t.Fatalf("expired lease not taken over: %v %+v", err, s.Lease)
	}
	at := now.Add(3 * time.Minute)
	if ok, err := s.Release("a", "s2", false, at, nil); ok || err == nil || s.Lease == nil {
		t.Fatalf("release by another holder = %v, %v", ok, err)
	}
	if ok, err := s.Release("a", "s2", true, at, nil); !ok || err != nil || s.Lease != nil {
		t.Fatalf("forced release = %v, %v", ok, err)
	}
	_ = s.Acquire("a", "s1", "h1", 1, at, LeaseTTL, nil)
	if ok, err := s.Release("a", "s1", false, at, nil); !ok || err != nil || s.Lease != nil {
		t.Fatalf("own release = %v, %v", ok, err)
	}
	if ok, err := s.Release("a", "s1", false, at, nil); ok || err != nil {
		t.Fatalf("release without a lease = %v, %v", ok, err)
	}
}

// Proves same-host compatibility: a session-less "pid:" process never evicts a
// live session's lease and a session replaces a pid lease on the same host,
// while two known sessions on one host, and holders on different hosts, still
// conflict (the case the lease exists for).
func TestLeaseSameHostPIDCompatible(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	var s State
	if err := s.Acquire("a", "sessA", "h1", 1, now, LeaseTTL, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire("a", "pid:77@h1", "h1", 77, now, LeaseTTL, nil); err != nil || s.Lease.Holder != "sessA" {
		t.Fatalf("pid process on the same host: %v, holder %s", err, s.Lease.Holder)
	}
	if err := s.Acquire("a", "sessB", "h1", 2, now, LeaseTTL, nil); err == nil {
		t.Fatal("a second known session on the same host was not refused")
	}
	if err := s.Acquire("a", "pid:88@h2", "h2", 88, now, LeaseTTL, nil); err == nil {
		t.Fatal("a process on another host was not refused")
	}
	var p State
	_ = p.Acquire("a", "pid:77@h1", "h1", 77, now, LeaseTTL, nil)
	if err := p.Acquire("a", "sessA", "h1", 1, now, LeaseTTL, nil); err != nil || p.Lease.Holder != "sessA" {
		t.Fatalf("session over a pid lease: %v, holder %s", err, p.Lease.Holder)
	}
	if err := p.Acquire("a", "sessX", "h2", 3, now, LeaseTTL, nil); err == nil {
		t.Fatal("a session on another host was not refused")
	}
}
