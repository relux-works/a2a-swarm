package cli

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func leaseApp(birth func(int) (int64, error)) *App {
	return &App{Getenv: func(string) string { return "" }, Now: time.Now, leaseBirth: birth}
}

// exitedPID returns the PID of a child that has already been reaped.
func exitedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// Proves the pid-holder release rule (#109): an exited PID is released with or
// without a recorded start time; a live PID is released only when its current
// start time is known and differs (reuse). A matching, unreadable, missing or
// malformed start time, another host, or a session holder is never released.
// Limit: start times come from a fake reader except for the real exited child.
func TestLeaseDeadPIDHolderBirth(t *testing.T) {
	self := strconv.Itoa(os.Getpid())
	host := machineLabel()
	gone := strconv.Itoa(exitedPID(t))
	const born = int64(1793224189)
	same := func(int) (int64, error) { return born, nil }
	other := func(int) (int64, error) { return born + 1, nil }
	unread := func(int) (int64, error) { return 0, errors.New("ps unavailable") }
	cases := []struct {
		name   string
		holder string
		birth  func(int) (int64, error)
		dead   bool
	}{
		{"live matching birth refuses", "pid:" + self + "@" + host + "#" + strconv.FormatInt(born, 10), same, false},
		{"live reused pid is released", "pid:" + self + "@" + host + "#" + strconv.FormatInt(born, 10), other, true},
		{"unreadable birth is not reuse", "pid:" + self + "@" + host + "#" + strconv.FormatInt(born, 10), unread, false},
		{"legacy holder without birth stays live", "pid:" + self + "@" + host, other, false},
		{"malformed birth stays live", "pid:" + self + "@" + host + "#abc", other, false},
		{"other host is never judged here", "pid:" + self + "@other-host#" + strconv.FormatInt(born, 10), other, false},
		{"session holder keeps its ttl", "sess-1", other, false},
		{"exited pid with birth is released", "pid:" + gone + "@" + host + "#" + strconv.FormatInt(born, 10), same, true},
		{"exited legacy pid is released", "pid:" + gone + "@" + host, unread, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := leaseApp(tc.birth).leaseDead(config.Paths{})(tc.holder); got != tc.dead {
				t.Fatalf("leaseDead(%q) = %v, want %v", tc.holder, got, tc.dead)
			}
		})
	}
}

// Proves a session-less process names itself with its own start time, and falls
// back to the legacy holder form when the start time cannot be read.
func TestLeaseHolderCarriesBirth(t *testing.T) {
	base := "pid:" + strconv.Itoa(os.Getpid()) + "@" + machineLabel()
	if got := leaseApp(func(int) (int64, error) { return 42, nil }).leaseHolder(config.Paths{}, ""); got != base+"#42" {
		t.Fatalf("holder = %q, want %q", got, base+"#42")
	}
	if got := leaseApp(func(int) (int64, error) { return 0, errors.New("no ps") }).leaseHolder(config.Paths{}, ""); got != base {
		t.Fatalf("holder = %q, want legacy %q", got, base)
	}
}

// Proves where the birth check changes behaviour: identity release without
// --force clears a reused pid holder but is refused by a live matching one, and
// a live session holder is never taken over. Acquire is unaffected for
// same-host pid holders, which existing compatibility already lets any
// same-host process replace; another host's pid holder is never released here.
func TestLeaseReleaseReusedPIDHolder(t *testing.T) {
	now := time.Now()
	host := machineLabel()
	holder := "pid:" + strconv.Itoa(os.Getpid()) + "@" + host + "#100"
	reused := leaseApp(func(int) (int64, error) { return 200, nil }).leaseDead(config.Paths{})
	matching := leaseApp(func(int) (int64, error) { return 100, nil }).leaseDead(config.Paths{})
	lease := func(h, on string) config.State {
		return config.State{Lease: &config.Lease{Holder: h, Host: on, PID: os.Getpid(), Expires: now.Add(time.Minute)}}
	}

	st := lease(holder, host)
	before := *st.Lease
	if ok, err := st.Release("alice", "sess-other", false, now, matching); ok || err == nil || st.Lease == nil || *st.Lease != before {
		t.Fatalf("live matching pid holder released or changed: ok=%v err=%v", ok, err)
	}
	st = lease(holder, host)
	if ok, err := st.Release("alice", "sess-other", false, now, reused); !ok || err != nil || st.Lease != nil {
		t.Fatalf("reused pid holder not released: ok=%v err=%v", ok, err)
	}
	st = lease("pid:"+strconv.Itoa(os.Getpid())+"@other-host#100", "other-host")
	before = *st.Lease
	if ok, err := st.Release("alice", "sess-other", false, now, reused); ok || err == nil || st.Lease == nil || *st.Lease != before {
		t.Fatalf("another host's pid holder released or changed: ok=%v err=%v", ok, err)
	}
	st = lease("sess-live", host)
	before = *st.Lease
	if err := st.Acquire("alice", "sess-new", host, 4, now, config.LeaseTTL, reused); err == nil || st.Lease == nil || *st.Lease != before {
		t.Fatal("live session holder was taken over or changed")
	}
}

// Proves the real start-time reader is independent of the caller's TZ, so two
// readers of the same process can never disagree and fake a reuse. Requires ps.
func TestPIDBirthIndependentOfTZ(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps unavailable")
	}
	a := &App{Getenv: os.Getenv}
	t.Setenv("TZ", "Asia/Yerevan")
	first, err := a.pidBirth(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TZ", "America/Los_Angeles")
	second, err := a.pidBirth(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("start time differs by TZ: %d vs %d", first, second)
	}
	if d := time.Since(time.Unix(first, 0)); d < 0 || d > 24*time.Hour {
		t.Fatalf("implausible start time %d (age %v)", first, d)
	}
}
