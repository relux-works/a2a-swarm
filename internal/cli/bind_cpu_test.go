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

// Claim: the window base survives tick jitter. After a full high window
// at exact minutes, an early tick (30s later) and the jittered ticks
// after it still compute a full share from the retained base sample;
// dropping the base each call would leave those ticks with no window.
func TestBinderCPUTrackerJitteredTicks(t *testing.T) {
	start := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	tr := &binderCPUTracker{}
	at, cpu := start, time.Duration(0)
	tr.observe(at, cpu, 0.20)
	fired := false
	for i := 0; i < 10; i++ {
		at = at.Add(time.Minute)
		cpu += 54 * time.Second
		_, _, fire, _ := tr.observe(at, cpu, 0.20)
		fired = fired || fire
	}
	if !fired {
		t.Fatal("exact-minute control did not fire at the full window")
	}
	for _, step := range []time.Duration{30 * time.Second, 90 * time.Second, 45 * time.Second, 75 * time.Second, 60 * time.Second} {
		at = at.Add(step)
		cpu += step * 9 / 10
		share, full, fire, _ := tr.observe(at, cpu, 0.20)
		if !full || share < 0.85 || share > 0.95 {
			t.Fatalf("jittered tick +%v lost the window: share=%v full=%v", step, share, full)
		}
		if fire {
			t.Fatal("jittered ticks refired while still above")
		}
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
// and the fake clocks installed, at the first (baseline) sample. The
// loaded image is stubbed to the same revision, so notices that name the
// in-process revision match the record unless a test stales it.
func prepareBinderCPU(t *testing.T) (*env, *App, *session, *binderCPUTracker, *time.Time, *time.Duration, *strings.Builder) {
	t.Helper()
	e, a, s, room, _ := alarmFixture(t)
	_ = room
	rev := strings.Repeat("d", 40)
	stubRunning(t, "/installed/a2a-image", "/installed/a2a-image", rev)
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
// one owner notice carrying share, window and the loaded image's running
// revision (no file or mailbox text), and logs one line; the persisted
// record is deliberately staled first, so a notice or log that named the
// stored revision instead of the in-process one would fail here. The
// queued job is classified under binder-cpu in status, never as stale
// mail. Continued high samples repeat none of them, and the ordinary
// alarm worker then delivers that one notice. The control is the same
// feed just below the threshold, which stays silent.
func TestBinderCPUSampleQueuesOneOwnerNotice(t *testing.T) {
	e, a, s, tracker, now, cpu, log := prepareBinderCPU(t)
	feedBinderCPU(a, s, tracker, now, cpu, 15, 11400*time.Millisecond)
	if len(cpuJobs(e)) != 0 || log.Len() != 0 {
		t.Fatal("just-below control was not silent")
	}
	notificationState(t, e, func(st *config.State) {
		st.Terminal.Observe.Revision = strings.Repeat("e", 40)
	})
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
	if !strings.Contains(log.String(), "revision="+strings.Repeat("d", 40)) || strings.Contains(log.String(), strings.Repeat("e", 40)) {
		t.Fatalf("log line named the stale stored revision: %q", log.String())
	}
	status := alarmStatus(s, e.state("alice"))
	if status.Pending != 0 || status.BinderCPU.Pending != 1 || !status.BinderCPU.Outstanding || status.BinderCPU.State != "armed" {
		t.Fatalf("cpu job misclassified in alarm status: stale=%+v binder-cpu=%+v", status.alarmRouteStatus, status.BinderCPU)
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

// Claim: a fresh tracker (the state a re-exec leaves: RAM gone, the
// persisted episode intact) queues no second notice for the still-high
// episode, because sampling mirrors the persisted fence before each
// sample. The control is the first tracker, which queued exactly one.
func TestBinderCPUFreshTrackerOverPersistedEpisode(t *testing.T) {
	e, a, s, tracker, now, cpu, log := prepareBinderCPU(t)
	feedBinderCPU(a, s, tracker, now, cpu, 11, 54*time.Second)
	if len(cpuJobs(e)) != 1 || strings.Count(log.String(), "binding cpu alarm:") != 1 {
		t.Fatal("control: first tracker did not queue exactly one notice")
	}
	fresh := &binderCPUTracker{}
	feedBinderCPU(a, s, fresh, now, cpu, 15, 54*time.Second)
	if len(cpuJobs(e)) != 1 || strings.Count(log.String(), "binding cpu alarm:") != 1 {
		t.Fatalf("fresh tracker repeated the persisted episode: jobs=%+v log=%q", cpuJobs(e), log.String())
	}
	if observe := e.state("alice").Terminal.Observe; observe == nil || !observe.CPUAlerted || observe.CPUJobID != cpuJobs(e)[0].ID {
		t.Fatalf("persisted episode changed under the fresh tracker: %+v", observe)
	}
}

// Claim: after a full window at or below the threshold the persisted
// episode rearms in state, and a new high window then queues a second
// notice through the real sampling step (the tracker-level test alone
// cannot see the state fence). The first notice is delivered before the
// low window, so exactly one job is ever pending.
func TestBinderCPURearmSecondNoticeThroughState(t *testing.T) {
	e, a, s, tracker, now, cpu, log := prepareBinderCPU(t)
	feedBinderCPU(a, s, tracker, now, cpu, 11, 54*time.Second)
	if len(cpuJobs(e)) != 1 {
		t.Fatal("setup: no cpu job queued")
	}
	*now = now.Add(time.Minute)
	a.sendMailAlarm(context.Background(), s, "alarm")
	if len(cpuJobs(e)) != 0 || len(cpuAlarmEvents(e)) != 1 {
		t.Fatal("setup: first notice not delivered exactly once")
	}
	feedBinderCPU(a, s, tracker, now, cpu, 30, 6*time.Second)
	if observe := e.state("alice").Terminal.Observe; observe == nil || observe.CPUAlerted || observe.CPUJobID != "" {
		t.Fatalf("episode did not rearm in state after a full low window: %+v", observe)
	}
	feedBinderCPU(a, s, tracker, now, cpu, 12, 54*time.Second)
	if len(cpuJobs(e)) != 1 || strings.Count(log.String(), "binding cpu alarm:") != 2 {
		t.Fatalf("no second notice after rearm and a new high window: jobs=%+v log=%q", cpuJobs(e), log.String())
	}
}

// Claim: an episode whose owner route is missing at fire time records its
// fence and log line but queues nothing; when the route appears, a later
// sample queues its one notice (naming the in-process revision, with the
// stored record deliberately staled) without repeating the log line,
// and further samples queue no more.
func TestBinderCPUDeferredRouteQueuesOnce(t *testing.T) {
	e, a, s, tracker, now, cpu, log := prepareBinderCPU(t)
	owner := e.state("alice").MailAlarmConfig.Owner
	notificationState(t, e, func(st *config.State) { st.MailAlarmConfig.Owner = "off" })
	feedBinderCPU(a, s, tracker, now, cpu, 11, 54*time.Second)
	if len(cpuJobs(e)) != 0 {
		t.Fatal("notice queued without an owner route")
	}
	if observe := e.state("alice").Terminal.Observe; observe == nil || !observe.CPUAlerted || observe.CPUJobID != "" {
		t.Fatalf("episode fence not recorded without a route: %+v", observe)
	}
	if strings.Count(log.String(), "binding cpu alarm:") != 1 {
		t.Fatalf("fire without a route logged %q", log.String())
	}
	notificationState(t, e, func(st *config.State) {
		st.MailAlarmConfig.Owner = owner
		st.Terminal.Observe.Revision = strings.Repeat("e", 40)
	})
	feedBinderCPU(a, s, tracker, now, cpu, 2, 54*time.Second)
	jobs := cpuJobs(e)
	if len(jobs) != 1 || jobs[0].Reason != strings.Repeat("d", 40) || !validAlarmJob(jobs[0], "alice") {
		t.Fatalf("deferred notice not queued once with the in-process revision: %+v", jobs)
	}
	if observe := e.state("alice").Terminal.Observe; observe == nil || observe.CPUJobID != jobs[0].ID {
		t.Fatalf("deferred job not fenced in state: %+v", observe)
	}
	feedBinderCPU(a, s, tracker, now, cpu, 2, 54*time.Second)
	if len(cpuJobs(e)) != 1 || strings.Count(log.String(), "binding cpu alarm:") != 1 {
		t.Fatal("deferred notice or log repeated after the route appeared")
	}
}

// Claim: a share above ten cores queues a valid, deliverable notice at
// the capped share instead of an invalid job held forever with the
// episode fenced off. The control is the uncapped conversion below the
// bound, which is exact.
func TestBinderCPUAboveTenCoresClamped(t *testing.T) {
	if got := cpuShareTenths(0.9); got != 900 {
		t.Fatalf("uncapped control: cpuShareTenths(0.9)=%d, want 900", got)
	}
	if got := cpuShareTenths(25); got != maxBinderCPUCount {
		t.Fatalf("cpuShareTenths(25)=%d, want capped %d", got, maxBinderCPUCount)
	}
	e, a, s, tracker, now, cpu, _ := prepareBinderCPU(t)
	feedBinderCPU(a, s, tracker, now, cpu, 11, 660*time.Second) // 11 cores
	jobs := cpuJobs(e)
	if len(jobs) != 1 || jobs[0].Count != maxBinderCPUCount || !validAlarmJob(jobs[0], "alice") {
		t.Fatalf("above-ten-cores job not valid at the cap: %+v", jobs)
	}
	*now = now.Add(time.Minute)
	a.sendMailAlarm(context.Background(), s, "alarm")
	bodies := cpuAlarmEvents(e)
	if len(bodies) != 1 || !strings.Contains(bodies[0], "share=1000.0%") || len(cpuJobs(e)) != 0 {
		t.Fatalf("capped notice not delivered once: %v", bodies)
	}
}
