package cli

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type cpuFeed struct {
	tracker *binderCPUTracker
	now     time.Time
	cpu     time.Duration
	fires   int
	rearms  int
}

func newCPUFeed(start time.Time) *cpuFeed {
	return &cpuFeed{tracker: &binderCPUTracker{}, now: start}
}

// step advances one minute burning perMinute of CPU in that minute.
func (f *cpuFeed) step(perMinute time.Duration, threshold float64) (fire, rearm bool) {
	f.now = f.now.Add(time.Minute)
	f.cpu += perMinute
	_, _, fire, rearm = f.tracker.observe(f.now, f.cpu, threshold)
	if fire {
		f.fires++
	}
	if rearm {
		f.rearms++
	}
	return fire, rearm
}

func (f *cpuFeed) run(minutes int, perMinute time.Duration, threshold float64) {
	for i := 0; i < minutes; i++ {
		f.step(perMinute, threshold)
	}
}

// Claim: the sliding 10-minute window fires exactly once when the share is
// above the threshold for a full window, never before it and never again
// while still above; it rearms only after the window share has stayed at
// or below the threshold for a full window, after which a new high window
// fires a second time. Beside that control, a share just below (19%), a
// share exactly at the threshold, a disabled (0) threshold, and a sampling
// gap inside the window never fire. Fake samples only; no wall clock.
func TestBinderCPUTrackerWindow(t *testing.T) {
	start := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	const high = 54 * time.Second // 90% of one core
	const low = 6 * time.Second   // 10%

	below := newCPUFeed(start)
	below.tracker.observe(start, 0, 0.20)
	below.run(15, 11400*time.Millisecond, 0.20)
	if below.fires != 0 {
		t.Fatal("19% share fired")
	}
	atThreshold := newCPUFeed(start)
	atThreshold.tracker.observe(start, 0, 0.20)
	atThreshold.run(15, 12*time.Second, 0.20)
	if atThreshold.fires != 0 {
		t.Fatal("share exactly at the threshold fired")
	}
	disabled := newCPUFeed(start)
	disabled.tracker.observe(start, 0, 0)
	disabled.run(15, high, 0)
	if disabled.fires != 0 {
		t.Fatal("disabled alarm fired")
	}

	f := newCPUFeed(start)
	f.tracker.observe(start, 0, 0.20)
	f.run(9, high, 0.20)
	if f.fires != 0 {
		t.Fatal("fired before a full window")
	}
	if fire, _ := f.step(high, 0.20); !fire || f.fires != 1 {
		t.Fatal("no fire at the first full high window")
	}
	f.run(5, high, 0.20)
	if f.fires != 1 {
		t.Fatal("repeated while still above")
	}
	f.run(25, low, 0.20)
	if f.rearms != 1 || f.fires != 1 {
		t.Fatalf("rearm after a full window below: rearms=%d fires=%d", f.rearms, f.fires)
	}
	f.run(10, high, 0.20)
	if f.fires != 2 {
		t.Fatalf("no second fire after rearm and a new high window: fires=%d", f.fires)
	}

	gap := newCPUFeed(start)
	gap.tracker.observe(start, 0, 0.20)
	gap.run(5, high, 0.20)
	gap.now = gap.now.Add(20 * time.Minute)
	gap.cpu += 20 * high
	if _, full, fire, _ := gap.tracker.observe(gap.now, gap.cpu, 0.20); full || fire {
		t.Fatal("a sampling gap inside the window fired")
	}
}

// Claim: the protected config member resolves as absent = default 20%,
// explicit 0 = disabled, other values as their percent of one core, and
// out-of-range values fall back to the default instead of silencing the
// alarm.
func TestBinderCPUThresholdFor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value *int
		want  float64
	}{
		{"absent", nil, 0.20},
		{"disabled", intPtr(0), 0},
		{"fifty", intPtr(50), 0.50},
		{"negative", intPtr(-1), 0.20},
		{"too large", intPtr(1001), 0.20},
	} {
		if got := binderCPUThresholdFor(tc.value); got != tc.want {
			t.Errorf("%s: threshold=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func intPtr(v int) *int { return &v }

// stubBinderCPU installs a fake CPU clock and clock for sampleBinderCPU.
func stubBinderCPU(t *testing.T, a *App, now *time.Time, cpu *time.Duration) {
	t.Helper()
	old := binderCPUTime
	binderCPUTime = func() (time.Duration, error) { return *cpu, nil }
	t.Cleanup(func() { binderCPUTime = old })
	a.Now = func() time.Time { return *now }
}

// feedBinderCPU feeds minute samples through the real sampling step.
func feedBinderCPU(a *App, s *session, tracker *binderCPUTracker, now *time.Time, cpu *time.Duration, minutes int, perMinute time.Duration) {
	for i := 0; i < minutes; i++ {
		*now = now.Add(time.Minute)
		*cpu += perMinute
		a.sampleBinderCPU(s, "alarm", tracker)
	}
}

func cpuAlarmEvents(e *env) []string {
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	var bodies []string
	for _, ev := range e.hs.events {
		if strings.HasPrefix(ev.Message().Body, "Binder CPU alarm:") {
			bodies = append(bodies, ev.Message().Body)
		}
	}
	return bodies
}

func cpuJobs(e *env) []config.MailAlarmJob {
	var jobs []config.MailAlarmJob
	for _, j := range e.state("alice").MailAlarm.Jobs {
		if j.Kind == binderCPUAlarmKind {
			jobs = append(jobs, j)
		}
	}
	return jobs
}

// prepareBinderCPU returns the alarm fixture with a recorded running image
// and the fake clocks installed, at the first (baseline) sample.
func prepareBinderCPU(t *testing.T) (*env, *App, *session, *binderCPUTracker, *time.Time, *time.Duration, *strings.Builder) {
	t.Helper()
	e, a, s, room, _ := alarmFixture(t)
	_ = room
	rev := strings.Repeat("d", 40)
	notificationState(t, e, func(st *config.State) {
		st.Terminal.Observe = &config.TerminalObserve{Executable: "/installed/a2a-image", Revision: rev, OnLaunchAlias: true}
	})
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	var cpu time.Duration
	log := &strings.Builder{}
	a.Stderr = log
	stubBinderCPU(t, a, &now, &cpu)
	tracker := &binderCPUTracker{}
	a.sampleBinderCPU(s, "alarm", tracker)
	return e, a, s, tracker, &now, &cpu, log
}

// Claim: a full high window records the episode in state, queues exactly
// one owner notice carrying share, window and the recorded running
// revision (no file or mailbox text), and logs one line; continued high
// samples repeat none of them, and the ordinary alarm worker then
// delivers that one notice. The control is the same feed just below the
// threshold, which stays silent.
func TestBinderCPUSampleQueuesOneOwnerNotice(t *testing.T) {
	e, a, s, tracker, now, cpu, log := prepareBinderCPU(t)
	feedBinderCPU(a, s, tracker, now, cpu, 15, 11400*time.Millisecond)
	if len(cpuJobs(e)) != 0 || log.Len() != 0 {
		t.Fatal("just-below control was not silent")
	}
	// Continue from the below-threshold feed into a high window.
	feedBinderCPU(a, s, tracker, now, cpu, 12, 54*time.Second)
	jobs := cpuJobs(e)
	if len(jobs) != 1 {
		t.Fatalf("jobs=%d, want exactly one", len(jobs))
	}
	job := jobs[0]
	if job.Target != "" || job.Minutes != 10 || job.Reason != strings.Repeat("d", 40) || !validAlarmJob(job, "alice") {
		t.Fatalf("queued cpu job: %+v", job)
	}
	observe := e.state("alice").Terminal.Observe
	if observe == nil || !observe.CPUAlerted || observe.CPUJobID != job.ID {
		t.Fatalf("episode not recorded in state: %+v", observe)
	}
	if got := strings.Count(log.String(), "binding cpu alarm: share="); got != 1 {
		t.Fatalf("log lines=%d: %q", got, log.String())
	}
	feedBinderCPU(a, s, tracker, now, cpu, 3, 54*time.Second)
	if len(cpuJobs(e)) != 1 || strings.Count(log.String(), "binding cpu alarm:") != 1 {
		t.Fatal("notice or log repeated while still above")
	}
	*now = now.Add(time.Minute)
	a.sendMailAlarm(context.Background(), s, "alarm")
	bodies := cpuAlarmEvents(e)
	if len(bodies) != 1 || !strings.Contains(bodies[0], "window=10m revision="+strings.Repeat("d", 40)) || !strings.Contains(bodies[0], "share=") {
		t.Fatalf("delivered notices: %v", bodies)
	}
	if len(cpuJobs(e)) != 0 {
		t.Fatal("delivered cpu job not drained")
	}
}

// Claim: an explicit zero in the protected credential member disables the
// alarm end to end, beside the default-threshold control, which the
// adjacent test shows firing on the same feed.
func TestBinderCPUDisabledByProtectedConfig(t *testing.T) {
	e, a, s, tracker, now, cpu, log := prepareBinderCPU(t)
	if got := binderCPUThreshold(s); got != defaultBinderCPUshare {
		t.Fatalf("default threshold=%v", got)
	}
	if _, err := config.UpdateCredentials(e.paths(), "alice", func(c *config.Credentials) error {
		c.BinderCPUPercent = intPtr(0)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := binderCPUThreshold(s); got != 0 {
		t.Fatalf("disabled threshold=%v", got)
	}
	feedBinderCPU(a, s, tracker, now, cpu, 15, 54*time.Second)
	if len(cpuJobs(e)) != 0 || log.Len() != 0 {
		t.Fatal("disabled alarm queued or logged")
	}
	if observe := e.state("alice").Terminal.Observe; observe == nil || observe.CPUAlerted {
		t.Fatalf("disabled alarm recorded an episode: %+v", observe)
	}
}

// Claim: a failed CPU notice send is retried by the ordinary mail-alarm
// rules (pending with backoff, beyond the ordinary two-attempt uncertain
// hold) and never blocks the consumer: sampling and state updates keep
// working while it is pending, and restoring transport delivers it once.
func TestBinderCPUNoticeFailureRetried(t *testing.T) {
	e, a, s, tracker, now, cpu, _ := prepareBinderCPU(t)
	feedBinderCPU(a, s, tracker, now, cpu, 11, 54*time.Second)
	if len(cpuJobs(e)) != 1 {
		t.Fatal("setup: no cpu job queued")
	}
	base := s.client.HTTP.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	calls := 0
	failing := &http.Client{Transport: alarmTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPut {
			return base.RoundTrip(r)
		}
		calls++
		return nil, errors.New("synthetic offline")
	})}
	s.client.HTTP = failing
	*now = now.Add(time.Minute)
	a.sendMailAlarm(context.Background(), s, "alarm")
	jobs := cpuJobs(e)
	if calls != 1 || len(jobs) != 1 || jobs[0].Failure != "send" || jobs[0].Uncertain != 1 || !jobs[0].Next.After(*now) {
		t.Fatalf("first failure not pending with backoff: calls=%d jobs=%+v", calls, jobs)
	}
	// Delivery-side state work continues while the notice is pending.
	feedBinderCPU(a, s, tracker, now, cpu, 2, 54*time.Second)
	if len(cpuJobs(e)) != 1 {
		t.Fatal("sampling blocked or duplicated while the notice was pending")
	}
	*now = now.Add(6 * time.Minute)
	a.sendMailAlarm(context.Background(), s, "alarm")
	if calls != 2 || len(cpuJobs(e)) != 1 || cpuJobs(e)[0].Uncertain != 2 {
		t.Fatalf("second failure: calls=%d jobs=%+v", calls, cpuJobs(e))
	}
	s.client.HTTP = &http.Client{Transport: base}
	*now = now.Add(6 * time.Minute)
	a.sendMailAlarm(context.Background(), s, "alarm")
	if len(cpuJobs(e)) != 0 || len(cpuAlarmEvents(e)) != 1 {
		t.Fatalf("retry after failures did not deliver exactly once: jobs=%+v events=%v", cpuJobs(e), cpuAlarmEvents(e))
	}
}
