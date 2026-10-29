package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// leaseApp injects a start-time reader that honours its context like the
// real ps read: an expired or cancelled context yields an error, never a value.
func leaseApp(birth func(int) (int64, error)) *App {
	return &App{Getenv: func(string) string { return "" }, Now: time.Now, leaseBirth: ctxAware(birth)}
}

func ctxAware(birth func(int) (int64, error)) func(context.Context, int) (int64, error) {
	return func(ctx context.Context, pid int) (int64, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		return birth(pid)
	}
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
	if got := leaseApp(func(int) (int64, error) { return 42, nil }).leaseHolder(config.Paths{}, "", time.Time{}); got != base+"#42" {
		t.Fatalf("holder = %q, want %q", got, base+"#42")
	}
	if got := leaseApp(func(int) (int64, error) { return 0, errors.New("no ps") }).leaseHolder(config.Paths{}, "", time.Time{}); got != base {
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
	first, err := a.pidBirth(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TZ", "America/Los_Angeles")
	second, err := a.pidBirth(context.Background(), os.Getpid())
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

// Proves acquisition is decided by the birth check where the compatibility
// rule does not apply (a pid holder without a recorded host): a matching or
// unreadable start time refuses and leaves the lease byte-identical, a reused
// PID is taken over and its evidence recorded. Also pins the pre-existing
// exception: a same-host pid lease is replaceable on acquire whatever its
// start time, which this change does not alter.
func TestLeaseAcquireHostlessPIDHolder(t *testing.T) {
	now := time.Now()
	host := machineLabel()
	holder := "pid:" + strconv.Itoa(os.Getpid()) + "@" + host + "#100"
	hostless := func() config.State {
		return config.State{Lease: &config.Lease{Holder: holder, PID: os.Getpid(), Expires: now.Add(time.Minute)}}
	}
	for name, birth := range map[string]func(int) (int64, error){
		"matching":   func(int) (int64, error) { return 100, nil },
		"unreadable": func(int) (int64, error) { return 0, errors.New("ps unavailable") },
	} {
		a := leaseApp(birth)
		st := hostless()
		before := *st.Lease
		if err := st.Acquire("alice", "sess-new", host, 2, now, config.LeaseTTL, a.leaseDead(config.Paths{})); err == nil || *st.Lease != before || a.leaseReuse != nil {
			t.Fatalf("%s: hostless live pid holder taken over or changed: err=%v lease=%+v reuse=%+v", name, err, *st.Lease, a.leaseReuse)
		}
	}
	a := leaseApp(func(int) (int64, error) { return 200, nil })
	st := hostless()
	if err := st.Acquire("alice", "sess-new", host, 2, now, config.LeaseTTL, a.leaseDead(config.Paths{})); err != nil || st.Lease.Holder != "sess-new" {
		t.Fatalf("hostless reused pid holder not taken over: err=%v lease=%+v", err, st.Lease)
	}
	if r := a.leaseReuse; r == nil || r.Holder != holder || r.Recorded != 100 || r.Observed != 200 {
		t.Fatalf("reuse evidence = %+v", r)
	}

	matching := leaseApp(func(int) (int64, error) { return 100, nil })
	st = config.State{Lease: &config.Lease{Holder: holder, Host: host, PID: os.Getpid(), Expires: now.Add(time.Minute)}}
	if err := st.Acquire("alice", "sess-new", host, 2, now, config.LeaseTTL, matching.leaseDead(config.Paths{})); err != nil || st.Lease.Holder != "sess-new" || matching.leaseReuse != nil {
		t.Fatalf("pre-existing same-host pid replacement changed: err=%v lease=%+v reuse=%+v", err, st.Lease, matching.leaseReuse)
	}
}

// Proves the start-time read inside the state lock honours the caller's
// deadline: a probe that would block returns as "unknown" by the deadline,
// refuses the release and leaves the lease unchanged; the same holder with a
// fast reused observation is released (paired positive control).
func TestLeaseDeadHonoursDeadline(t *testing.T) {
	now := time.Now()
	host := machineLabel()
	holder := "pid:" + strconv.Itoa(os.Getpid()) + "@" + host + "#100"
	slow := &App{Getenv: func(string) string { return "" }, Now: time.Now, leaseBirth: func(ctx context.Context, _ int) (int64, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}}
	st := config.State{Lease: &config.Lease{Holder: holder, Host: host, PID: os.Getpid(), Expires: now.Add(time.Minute)}}
	before := *st.Lease
	start := time.Now()
	ok, err := st.Release("alice", "sess-other", false, now, slow.leaseDeadBy(config.Paths{}, time.Now().Add(50*time.Millisecond)))
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("deadline not honoured: %v", elapsed)
	}
	if ok || err == nil || *st.Lease != before || slow.leaseReuse != nil {
		t.Fatalf("slow probe released or changed the lease: ok=%v err=%v lease=%+v", ok, err, *st.Lease)
	}
	fast := leaseApp(func(int) (int64, error) { return 200, nil })
	if ok, err := st.Release("alice", "sess-other", false, now, fast.leaseDeadBy(config.Paths{}, time.Now().Add(50*time.Millisecond))); !ok || err != nil {
		t.Fatalf("fast reused probe not released: ok=%v err=%v", ok, err)
	}
}

// Proves the reuse decision is reported with both start times through the
// real commands: identity release prints it on stdout, and an inbox that takes
// over a hostless reused pid holder writes one stderr line while its stdout
// stays the ordinary inbox output. A matching start time refuses both and
// reports nothing.
func TestLeaseReuseReported(t *testing.T) {
	e, _ := world(t)
	e.extra[EnvSessionID] = "s1"
	e.as("alice", "inbox").ok(t)
	holder := "pid:" + strconv.Itoa(os.Getpid()) + "@" + machineLabel() + "#100"
	plant := func(onHost bool) {
		t.Helper()
		l := &config.Lease{Holder: holder, PID: os.Getpid(), Expires: time.Now().Add(time.Minute)}
		if onHost {
			l.Host = machineLabel()
		}
		if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Lease = l; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	birth := int64(100)
	e.configure = func(a *App) {
		a.leaseBirth = ctxAware(func(int) (int64, error) { return birth, nil })
	}
	e.extra[EnvSessionID] = "s2"

	plant(true)
	e.as("alice", "identity", "release").failsWith(t, ExitError, "held by "+holder)
	if l := e.lease("alice"); l == nil || l.Holder != holder {
		t.Fatalf("matching release changed lease: %+v", l)
	}
	birth = 200
	r := e.as("alice", "identity", "release").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "released alice: pid holder "+holder+" was reused (recorded start 100, observed start 200)")

	birth = 100
	plant(false)
	e.as("alice", "inbox").failsWith(t, ExitError, "held by "+holder)
	birth = 200
	r = e.as("alice", "inbox").ok(t)
	tuitestkit.ContainsStr(t, r.stderr, "a2a: lease of alice taken over: pid holder "+holder+" was reused (recorded start 100, observed start 200)")
	tuitestkit.NotContainsStr(t, r.stdout, "reused")
	if l := e.lease("alice"); l == nil || l.Holder != "s2" {
		t.Fatalf("lease after takeover = %+v", l)
	}
}

// Regression witness for the zero-deadline convention (re-review R2-1): inbox,
// wait and watch pass time.Time{} meaning "no deadline". With a reader that
// honours its context, a zero or future deadline still observes the start time
// and detects reuse; only a deadline that has already passed yields "unknown".
func TestLeaseDeadDeadlineConventions(t *testing.T) {
	holder := "pid:" + strconv.Itoa(os.Getpid()) + "@" + machineLabel() + "#100"
	for name, tc := range map[string]struct {
		deadline time.Time
		dead     bool
	}{
		"zero means no deadline": {time.Time{}, true},
		"future deadline":        {time.Now().Add(time.Second), true},
		"expired deadline":       {time.Now().Add(-time.Second), false},
	} {
		a := leaseApp(func(int) (int64, error) { return 200, nil })
		if got := a.leaseDeadBy(config.Paths{}, tc.deadline)(holder); got != tc.dead || (a.leaseReuse != nil) != tc.dead {
			t.Fatalf("%s: dead=%v reuse=%+v, want dead=%v", name, got, a.leaseReuse, tc.dead)
		}
	}
}

// Proves holder creation honours the caller's remaining budget (re-review
// R2-2): with time left, or no deadline, the holder carries its start time; with
// the deadline already passed, or a reader that would block past it, it falls
// back promptly to the legacy form.
func TestLeaseHolderHonoursDeadline(t *testing.T) {
	base := "pid:" + strconv.Itoa(os.Getpid()) + "@" + machineLabel()
	fast := leaseApp(func(int) (int64, error) { return 42, nil })
	if got := fast.leaseHolder(config.Paths{}, "", time.Now().Add(time.Second)); got != base+"#42" {
		t.Fatalf("holder with time left = %q", got)
	}
	if got := fast.leaseHolder(config.Paths{}, "", time.Now().Add(-time.Second)); got != base {
		t.Fatalf("holder past deadline = %q, want legacy %q", got, base)
	}
	slow := &App{Getenv: func(string) string { return "" }, Now: time.Now, leaseBirth: func(ctx context.Context, _ int) (int64, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}}
	start := time.Now()
	if got := slow.leaseHolder(config.Paths{}, "", time.Now().Add(50*time.Millisecond)); got != base {
		t.Fatalf("slow holder = %q, want legacy %q", got, base)
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("holder creation overran the deadline: %v", elapsed)
	}
}

// Proves the full acquisition path of a session-less command (no hook id,
// A2A_SESSION_ID or CODEX_THREAD_ID; inbox passes no deadline) records a
// pid holder carrying its start time.
func TestLeaseSessionlessInboxRecordsBirth(t *testing.T) {
	e, _ := world(t)
	e.configure = func(a *App) {
		a.leaseBirth = ctxAware(func(int) (int64, error) { return 4242, nil })
	}
	e.as("alice", "inbox").ok(t)
	want := "pid:" + strconv.Itoa(os.Getpid()) + "@" + machineLabel() + "#4242"
	if l := e.lease("alice"); l == nil || l.Holder != want {
		t.Fatalf("lease = %+v, want holder %q", l, want)
	}
}
