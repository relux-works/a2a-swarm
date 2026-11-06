package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
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

// A real exited child is dead; injected births release only known local reuse and preserve all conservative neighbors.
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

// A session-less holder records its injected birth and retains the legacy form when the read fails.
func TestLeaseHolderCarriesBirth(t *testing.T) {
	base := "pid:" + strconv.Itoa(os.Getpid()) + "@" + machineLabel()
	if got := leaseApp(func(int) (int64, error) { return 42, nil }).leaseHolder(config.Paths{}, "", time.Time{}); got != base+"#42" {
		t.Fatalf("holder = %q, want %q", got, base+"#42")
	}
	if got := leaseApp(func(int) (int64, error) { return 0, errors.New("no ps") }).leaseHolder(config.Paths{}, "", time.Time{}); got != base {
		t.Fatalf("holder = %q, want legacy %q", got, base)
	}
}

// Release accepts injected local reuse, refuses matching/foreign holders, and acquisition preserves a live session lease.
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

// Native Darwin readers with different TZ environments agree on their own process birth; other platforms skip.
func TestPIDBirthIndependentOfTZ(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("start-time reader is Darwin-only")
	}
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

// Hostless acquisition requires confirmed reuse and retains both births; same-host matching replacement stays compatible.
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

// A blocking comparison honors the caller deadline without changing the lease, while a fast injected reuse releases it.
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

// Release reports both injected births on stdout and inbox reports them only on stderr; matching holders refuse.
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

// A zero deadline remains unbounded by the caller; expired deadlines refuse and future deadlines permit injected reuse.
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

// Holder construction records birth with time available and falls back promptly for expired or blocking reads.
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

// A session-less inbox command records its own injected birth with the zero-deadline convention.
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

// The real non-Darwin gate keeps birth unknown, refuses live-holder release, and still releases a real exited child.
func TestPIDBirthUnsupportedPlatform(t *testing.T) {
	a := &App{Getenv: func(string) string { return "" }, Now: time.Now, leaseGOOS: "linux"}
	if _, err := a.pidBirth(context.Background(), os.Getpid()); !errors.Is(err, errPIDBirthUnsupported) {
		t.Fatalf("pidBirth error = %v, want errPIDBirthUnsupported", err)
	}
	base := "pid:" + strconv.Itoa(os.Getpid()) + "@" + machineLabel()
	if got := a.leaseHolder(config.Paths{}, "", time.Time{}); got != base {
		t.Fatalf("holder = %q, want legacy %q", got, base)
	}
	live := base + "#100"
	if a.leaseDead(config.Paths{})(live) || a.leaseReuse != nil {
		t.Fatalf("live holder judged reused on an unsupported platform: %+v", a.leaseReuse)
	}
	st := config.State{Lease: &config.Lease{Holder: live, Host: machineLabel(), PID: os.Getpid(), Expires: time.Now().Add(time.Minute)}}
	before := *st.Lease
	if ok, err := st.Release("alice", "sess-other", false, time.Now(), a.leaseDead(config.Paths{})); ok || err == nil || *st.Lease != before {
		t.Fatalf("live holder released on an unsupported platform: ok=%v err=%v", ok, err)
	}
	if !a.leaseDead(config.Paths{})("pid:" + strconv.Itoa(exitedPID(t)) + "@" + machineLabel() + "#100") {
		t.Fatal("exited pid holder not released on an unsupported platform")
	}
}

// Both holder creation and comparison cap a zero or distant deadline at 500ms and preserve a shorter caller deadline.
func TestLeaseBirthProbeCap(t *testing.T) {
	for _, path := range []string{"holder", "comparison"} {
		for _, budget := range []time.Duration{0, 5 * time.Second, 100 * time.Millisecond, -time.Second} {
			t.Run(path+"/"+budget.String(), func(t *testing.T) {
				var deadline time.Time
				if budget != 0 {
					deadline = time.Now().Add(budget)
				}
				calls := 0
				a := leaseApp(nil)
				a.leaseBirth = func(ctx context.Context, pid int) (int64, error) {
					calls++
					limit, ok := ctx.Deadline()
					if !ok || time.Until(limit) > 500*time.Millisecond {
						t.Fatalf("probe is not capped at 500ms: deadline=%v present=%v", limit, ok)
					}
					if budget != 0 && budget < 500*time.Millisecond && !limit.Equal(deadline) {
						t.Fatalf("caller deadline changed: got %v, want %v", limit, deadline)
					}
					if pid != os.Getpid() {
						t.Fatalf("probed pid %d, want own pid %d", pid, os.Getpid())
					}
					return 200, ctx.Err()
				}
				base := "pid:" + strconv.Itoa(os.Getpid()) + "@" + machineLabel()
				if path == "holder" {
					want := base + "#200"
					if budget < 0 {
						want = base
					}
					if got := a.leaseHolder(config.Paths{}, "", deadline); got != want {
						t.Fatalf("holder = %q, want %q", got, want)
					}
				} else if got := a.leaseDeadBy(config.Paths{}, deadline)(base + "#100"); got != (budget >= 0) {
					t.Fatalf("reuse = %v for budget %v", got, budget)
				}
				if calls != 1 {
					t.Fatalf("birth reader calls = %d, want 1", calls)
				}
			})
		}
	}
}

// Real commands preserve live, legacy, unknown, malformed, foreign and session leases without syncing, while dead holders release and acquire.
func TestLeaseBirthCommandBoundaries(t *testing.T) {
	self := "pid:" + strconv.Itoa(os.Getpid()) + "@" + machineLabel()
	deadPID := exitedPID(t)
	dead := "pid:" + strconv.Itoa(deadPID) + "@" + machineLabel()
	for _, tc := range []struct {
		name, holder string
		birth        int64
		readErr      error
		released     bool
	}{
		{"matching", self + "#100", 100, nil, false},
		{"legacy", self, 200, nil, false},
		{"unavailable", self + "#100", 0, errors.New("unknown birth"), false},
		{"malformed", self + "#not-a-time", 200, nil, false},
		{"empty", self + "#", 200, nil, false},
		{"foreign", "pid:" + strconv.Itoa(os.Getpid()) + "@other-host#100", 200, nil, false},
		{"session with exited CLI pid", "session-old", 200, nil, false},
		{"exited", dead + "#100", 0, errors.New("unknown birth"), true},
		{"exited legacy", dead, 0, errors.New("unknown birth"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := world(t)
			e.extra[EnvSessionID] = "session-new"
			e.configure = func(a *App) {
				a.leaseBirth = ctxAware(func(int) (int64, error) { return tc.birth, tc.readErr })
			}
			original := config.Lease{Holder: tc.holder, PID: os.Getpid(), Expires: time.Now().Add(time.Hour).UTC()}
			if strings.HasPrefix(tc.holder, "session-") {
				original.PID = deadPID
			}
			plant := func() {
				t.Helper()
				if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
					copy := original
					st.Lease = &copy
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			for _, args := range [][]string{{"identity", "release"}, {"inbox"}} {
				plant()
				syncs := len(e.hs.CallsFor("GET /_matrix/client/v3/sync"))
				if tc.released {
					e.as("alice", args...).ok(t)
					l := e.lease("alice")
					if args[0] == "identity" && l != nil || args[0] == "inbox" && (l == nil || l.Holder != "session-new") {
						t.Fatalf("dead holder was not cleared/replaced: %+v", l)
					}
				} else {
					r := e.as("alice", args...).failsWith(t, ExitError, "held by "+tc.holder)
					if l := e.lease("alice"); l == nil || *l != original {
						t.Fatalf("refused lease changed: %+v", l)
					}
					if len(e.hs.CallsFor("GET /_matrix/client/v3/sync")) != syncs || r.stdout != "" || strings.Contains(r.stderr, "was reused") {
						t.Fatalf("refusal synced or reported reuse: %+v", r)
					}
				}
			}
		})
	}
}
