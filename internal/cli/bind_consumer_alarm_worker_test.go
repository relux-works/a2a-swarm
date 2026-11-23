//go:build darwin || linux

package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// TestBindingConsumerUnavailableAlarmReachesWorker proves the consumer's own
// alarm worker delivers the consumer-unavailable incident while every cycle
// keeps failing: fake time passes the ten-minute bound, the failed cycle
// queues the incident, and the worker sends it without any successful
// dispatch and without the test calling the sender.
//
// Regression witness: the worker waits for a wake while it has no work, and
// only dispatch used to wake it. A failed cycle never reaches dispatch, so the
// incident that reports the failure stayed queued until the consumer
// recovered. The neighbouring episode tests call the sender themselves and
// could not see this.
func TestBindingConsumerUnavailableAlarmReachesWorker(t *testing.T) {
	isolateBindTest(t)
	f := newOrphanDispatchFixture(t)
	gate := consumerConnectionGate(t)
	room := f.e.ownReportRoom("alice")
	f.update(func(st *config.State) {
		st.HooksPaused = true
		st.MailAlarmConfig = &config.MailAlarmConfig{Minutes: 20, Owner: room}
		st.Terminal.Interval, st.Terminal.RetryMax = time.Millisecond, 4*time.Millisecond
	})
	setConsumerConnectionGate(t, gate, true)
	start := f.now
	var log strings.Builder
	f.a.Stderr = &log
	// The worker ticks every alarmWorkerInterval and nothing wakes it while
	// every cycle fails, so allow it the three ticks main's own worker test does.
	ctx, cancel := context.WithTimeout(context.Background(), 3*alarmWorkerInterval+5*time.Second)
	defer cancel()
	cycles, delivered := 0, false
	_, err := f.a.runTerminalWithRefresh(ctx, f.s, "orphan-gen", *f.binding(), func() {
		cycles++
		if cycles == 1 {
			// The episode began at the first failed cycle; the next one is at
			// the bound and queues the incident.
			f.now = start.Add(10 * time.Minute)
			return
		}
		if len(strandedAlerts(f.e)) == 1 {
			delivered = true
			cancel()
		}
	})
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	if strings.Contains(log.String(), "binding consumer ready") {
		t.Fatal("premise: a cycle succeeded, so dispatch could have woken the worker")
	}
	if !delivered {
		t.Fatalf("the worker did not deliver the incident in %d failed cycles; queued jobs=%d", cycles, len(f.e.state("alice").MailAlarm.Jobs))
	}
	if alerts := strandedAlerts(f.e); len(alerts) != 1 || !strings.Contains(alerts[0].Message().Body, "reason=consumer unavailable") {
		t.Fatalf("delivered alerts=%d, want one consumer-unavailable incident", len(alerts))
	}
}
