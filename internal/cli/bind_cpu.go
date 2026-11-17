package cli

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// CPU self-alarm: the consumer samples its own process CPU time once a
// minute and, when its share of one core over the sliding window stays
// above the configured threshold, posts one owner notice through the
// mail-alarm outbox and logs one line. It never stops or restarts itself.
const (
	binderCPUAlarmKind    = "binder-cpu"
	binderCPUWindow       = 10 * time.Minute
	binderCPUSampleEvery  = time.Minute
	defaultBinderCPUshare = 0.20
)

// binderCPUTime is a test seam for the process CPU clock; production code
// never changes it.
var binderCPUTime = processCPUTime

// binderCPUThresholdFor resolves the protected config member: absent (or an
// out-of-range value a provisioning writer should not have produced) selects
// the default share, and an explicit zero disables the alarm.
func binderCPUThresholdFor(percent *int) float64 {
	if percent == nil || *percent < 0 || *percent > 1000 {
		return defaultBinderCPUshare
	}
	return float64(*percent) / 100
}

// binderCPUThreshold re-reads the identity's protected credential metadata,
// so an operator change takes effect without a rebind. An unreadable or
// foreign record keeps the default rather than silencing the alarm.
func binderCPUThreshold(s *session) float64 {
	meta, err := config.LoadCredentialMetadata(s.paths, s.name, false)
	if err != nil || meta.UserID != s.creds.UserID || meta.Homeserver != s.creds.Homeserver {
		return defaultBinderCPUshare
	}
	return binderCPUThresholdFor(meta.BinderCPUPercent)
}

type binderCPUSample struct {
	at  time.Time
	cpu time.Duration
}

// binderCPUTracker holds the sliding window in RAM. The alerted fence is
// mirrored from State before every sample, so a re-exec (or a failed
// persist) cannot repeat a notice; only the below-threshold timer is
// RAM-only, which is conservative across a re-exec.
type binderCPUTracker struct {
	samples    []binderCPUSample
	alerted    bool
	belowSince time.Time
}

// observe adds one sample and reports the window share. fire is set on the
// sample where a full window first exceeds the threshold while not
// alerted; rearm is set once the window share has stayed at or below the
// threshold for a full window while alerted. A zero threshold disables.
func (t *binderCPUTracker) observe(at time.Time, cpu time.Duration, threshold float64) (share float64, full, fire, rearm bool) {
	if n := len(t.samples); n > 0 && (cpu < t.samples[n-1].cpu || !at.After(t.samples[n-1].at)) {
		// CPU counters restart with a new image and clocks can step back;
		// a non-monotonic reading restarts the window instead of firing.
		t.samples = nil
	}
	t.samples = append(t.samples, binderCPUSample{at: at, cpu: cpu})
	cutoff := at.Add(-binderCPUWindow)
	kept := t.samples[:0]
	var base *binderCPUSample
	for i := range t.samples {
		if !t.samples[i].at.After(cutoff) {
			s := t.samples[i]
			base = &s
			continue
		}
		kept = append(kept, t.samples[i])
	}
	if base == nil {
		t.samples = kept
		return 0, false, false, false
	}
	// Retain the base sample for the next call: with ticker jitter the
	// next cutoff advances by less (or more) than the sample spacing, and
	// a dropped base would leave that call with no window base at all.
	t.samples = append([]binderCPUSample{*base}, kept...)
	elapsed := at.Sub(base.at)
	if elapsed < binderCPUWindow || cpu < base.cpu {
		return 0, false, false, false
	}
	if len(kept) == 0 || kept[0].at.Sub(base.at) > 2*binderCPUSampleEvery {
		// A sampling gap inside the window proves no continuous share.
		return 0, false, false, false
	}
	share = float64(cpu-base.cpu) / float64(elapsed)
	full = true
	if threshold <= 0 {
		return share, full, false, false
	}
	if !t.alerted {
		if share > threshold {
			t.alerted = true
			t.belowSince = time.Time{}
			return share, full, true, false
		}
		return share, full, false, false
	}
	if share > threshold {
		t.belowSince = time.Time{}
		return share, full, false, false
	}
	if t.belowSince.IsZero() {
		t.belowSince = at
	}
	if at.Sub(t.belowSince) >= binderCPUWindow {
		t.alerted = false
		t.belowSince = time.Time{}
		return share, full, false, true
	}
	return share, full, false, false
}

// maxBinderCPUCount bounds the share a CPU job can carry, in tenths of a
// percent of one core (1000.0%). validAlarmJob enforces the same bound, so
// a binder burning more than ten cores queues a valid, deliverable notice
// reporting the capped share instead of an invalid job held forever.
const maxBinderCPUCount = 10000

func cpuShareTenths(share float64) int {
	return min(int(math.Round(share*1000)), maxBinderCPUCount)
}

func cpuShareText(tenths int) string {
	return fmt.Sprintf("%d.%d%%", tenths/10, tenths%10)
}

var errBinderCPUUnchanged = errors.New("binder cpu observation unchanged")

// enqueueBinderCPUJob appends the one owner job for an alerted episode.
// Called under the state lock: the job and its fence in the self-observation
// commit together. For this kind Count is the share in tenths of a percent
// of one core, Minutes is the window, and Reason is the running revision.
func enqueueBinderCPUJob(s *session, st *config.State, now time.Time, share float64, revision string) error {
	b := st.Terminal
	if b == nil || b.Observe == nil || b.Observe.CPUJobID != "" || st.MailAlarm.Invalid {
		return nil
	}
	c, targets, _ := alarmPolicy(s, *st)
	if !validAlarmConfig(c) {
		return nil
	}
	var owner *config.MailAlarmJob
	for _, target := range targets {
		if target.Target == "" {
			copy := target
			owner = &copy
			break
		}
	}
	if owner == nil {
		return nil
	}
	id, err := alarmID()
	if err != nil {
		return err
	}
	owner.ID, owner.Identity, owner.Host = id, s.name, alarmHost(b.Host)
	owner.Kind = binderCPUAlarmKind
	owner.Since = now.UTC().Format(time.RFC3339Nano)
	owner.Count = cpuShareTenths(share)
	owner.Minutes = int64(binderCPUWindow / time.Minute)
	if !safeRevisionToken(revision) || revision == "" {
		revision = "unknown"
	}
	owner.Reason = revision
	owner.At, owner.Next = now, now
	st.MailAlarm.Jobs = append(st.MailAlarm.Jobs, *owner)
	b.Observe.CPUJobID = id
	return nil
}

// sampleBinderCPU runs one sampling step. Delivery never waits on it: the
// worker is independent, its state writes are bounded, and a queued notice
// is sent (and retried on failure) by the ordinary mail-alarm worker.
func (a *App) sampleBinderCPU(s *session, generation string, tracker *binderCPUTracker) {
	if s.checkConnection() != nil {
		return
	}
	cpu, err := binderCPUTime()
	if err != nil {
		return
	}
	st, err := config.LoadState(s.paths, s.name)
	if err != nil || st.Terminal == nil || st.Terminal.Stopped || st.Terminal.Generation != generation {
		return
	}
	if st.Terminal.Observe != nil {
		tracker.alerted = st.Terminal.Observe.CPUAlerted
	} else {
		tracker.alerted = false
	}
	now := a.Now()
	threshold := binderCPUThreshold(s)
	share, _, fire, rearm := tracker.observe(now, cpu, threshold)
	switch {
	case fire:
		// The notice and the log line name this process's own loaded
		// revision, never a possibly stale persisted record: which build
		// is burning CPU is the question the notice exists to answer.
		revision := resolveRunningInfo().Revision
		_, err := a.updateStateBy(s.paths, s.name, time.Now(), func(cur *config.State) error {
			b := cur.Terminal
			if b == nil || b.Stopped || b.Generation != generation {
				return errBinderCPUUnchanged
			}
			if b.Observe == nil {
				info := resolveRunningInfo()
				b.Observe = &config.TerminalObserve{Executable: info.Executable, Revision: info.Revision, OnLaunchAlias: info.OnLaunchAlias, PID: os.Getpid(), Generation: generation}
			}
			if b.Observe.CPUAlerted {
				// The persisted fence, not this process's RAM, decides.
				return errBinderCPUUnchanged
			}
			b.Observe.CPUAlerted = true
			b.Observe.CPUAlertAt = now
			if err := enqueueBinderCPUJob(s, cur, now, share, revision); err != nil {
				return err
			}
			return nil
		})
		if errors.Is(err, errBinderCPUUnchanged) {
			tracker.alerted = true
			return
		}
		if err != nil {
			// Nothing was recorded; the next sample retries the alert.
			tracker.alerted = false
			return
		}
		fmt.Fprintf(a.Stderr, "binding cpu alarm: share=%s window=10m revision=%s\n", cpuShareText(cpuShareTenths(share)), revision)
	case rearm:
		_, _ = a.updateStateBy(s.paths, s.name, time.Now(), func(cur *config.State) error {
			b := cur.Terminal
			if b == nil || b.Stopped || b.Generation != generation || b.Observe == nil || !b.Observe.CPUAlerted {
				return errBinderCPUUnchanged
			}
			b.Observe.CPUAlerted = false
			b.Observe.CPUAlertAt = time.Time{}
			b.Observe.CPUJobID = ""
			return nil
		})
	default:
		// An episode whose owner route was missing at alert time queues its
		// one notice when a route appears; the log line is not repeated.
		if tracker.alerted && threshold > 0 && (st.Terminal.Observe == nil || st.Terminal.Observe.CPUJobID == "") {
			revision := resolveRunningInfo().Revision
			_, _ = a.updateStateBy(s.paths, s.name, time.Now(), func(cur *config.State) error {
				b := cur.Terminal
				if b == nil || b.Stopped || b.Generation != generation || b.Observe == nil || !b.Observe.CPUAlerted || b.Observe.CPUJobID != "" {
					return errBinderCPUUnchanged
				}
				before := len(cur.MailAlarm.Jobs)
				if err := enqueueBinderCPUJob(s, cur, now, share, revision); err != nil {
					return err
				}
				if len(cur.MailAlarm.Jobs) == before {
					return errBinderCPUUnchanged
				}
				return nil
			})
		}
	}
}

// runBinderCPU is the per-minute sampling worker. A panic in this optional
// worker must never escape into delivery, mirroring the mail-alarm worker.
func (a *App) runBinderCPU(ctx context.Context, s *session, generation string) {
	tracker := &binderCPUTracker{}
	a.sampleBinderCPUSafe(s, generation, tracker)
	ticker := time.NewTicker(binderCPUSampleEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.sampleBinderCPUSafe(s, generation, tracker)
		}
	}
}

func (a *App) sampleBinderCPUSafe(s *session, generation string, tracker *binderCPUTracker) {
	defer func() {
		if recover() != nil {
			fmt.Fprintln(a.Stderr, "binding cpu alarm worker panic; bounded retry")
		}
	}()
	a.sampleBinderCPU(s, generation, tracker)
}
