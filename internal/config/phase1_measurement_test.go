//go:build phase1measure && darwin && arm64

package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	phase1MeasureIdentity = "phase1-fixture"
	phase1WarmupCycles    = 5
	phase1SampleRounds    = 3
	phase1SampleCycles    = 30
	phase1CycleInterval   = time.Second
)

type phase1MeasureCounts struct {
	cycles, loadCalls, mailboxCalls, updateCalls int64
	stats                                        phase1MeasureStats
}

// phase1MeasureStats is local to this test harness so the same test source can
// be overlaid onto the pinned baseline, which predates ReadStateDecodeStats.
// The baseline adapter marks internal counters unavailable; process CPU/time
// and these direct production-API invocation counts remain comparable.
type phase1MeasureStats struct {
	available                                                          bool
	loadStateCalls, stateReadAttempts, stateReadErrors, stateReadBytes int64
	unmarshalStatePasses, unmarshalStateBytes                          int64
	scopeHits, scopeMisses, scopeFallbacks, writerClones               int64
	stateDigests, canonicalStates, sealedStates, writerSealChecks      int64
	stateEncodes, mailboxDecodes, stateWriteBytes, mailboxWriteBytes   int64
}

type phase1ProcessCPU struct {
	user, system time.Duration
}

type phase1SampleObservation struct {
	loadCalls, mailboxCalls, updateCalls int64
	elapsed, userCPU, systemCPU          time.Duration
}

// TestPhase1CacheCPUWorkload exercises the production State, mailbox and
// locked-update paths on the same deterministic fixture at the same 1s cycle
// interval. Build the baseline with -tags phase1measure plus its source overlay,
// and the candidate with -tags phase1measure,phase1cache. Wrap each exact
// test-binary invocation in /usr/bin/time -l for whole-process CPU and wall time.
func TestPhase1CacheCPUWorkload(t *testing.T) {
	base := Paths{Base: t.TempDir()}
	p, openEpoch, closeEpoch := phase1MeasurementPaths(base, phase1MeasureIdentity)
	stateBytes := phase1SeedState(t, p, phase1MeasureIdentity)
	mailboxBytes := phase1SeedMailbox(t, p, phase1MeasureIdentity)
	fixtureDir := phase1PreserveFixture(t, stateBytes, mailboxBytes)
	if len(stateBytes) < 400000 || len(stateBytes) > 580000 || len(mailboxBytes) != 2<<20 {
		t.Fatalf("fixture size State=%d mailbox=%d preservedAt=%q", len(stateBytes), len(mailboxBytes), fixtureDir)
	}
	stateHash, mailboxHash := sha256.Sum256(stateBytes), sha256.Sum256(mailboxBytes)

	var counts phase1MeasureCounts
	var sampleStart time.Time
	var sampleBefore phase1MeasureStats
	var sampleCPUStart phase1ProcessCPU
	var sampleLoadBefore, sampleMailboxBefore, sampleUpdateBefore int64
	var observations []phase1SampleObservation
	total := phase1WarmupCycles + phase1SampleRounds*phase1SampleCycles
	for i := 0; i < total; i++ {
		openEpoch()
		st, err := LoadState(p, phase1MeasureIdentity)
		if err != nil || st.NextBatch != "s0" || len(st.ThreadEvents) != 2000 || !phase1OpaqueFixture(st) {
			t.Fatalf("State cycle %d: next=%q events=%d err=%v", i, st.NextBatch, len(st.ThreadEvents), err)
		}
		counts.loadCalls++
		mail, err := OpenMailbox(p, phase1MeasureIdentity)
		if err != nil || len(mail.Pending()) != 0 || len(mail.received) != 200 {
			t.Fatalf("mailbox cycle %d: pending=%d received=%d err=%v", i, len(mail.Pending()), len(mail.received), err)
		}
		for receipt, id := range mail.received {
			if want := fmt.Sprintf("$mail%05d", receipt); id != want {
				t.Fatalf("mailbox cycle %d receipt[%d]=%q, want %q", i, receipt, id, want)
			}
		}
		counts.mailboxCalls++
		updated, err := UpdateState(p, phase1MeasureIdentity, func(current *State) error {
			if current.NextBatch != "s0" || len(current.ThreadEvents) != 2000 || !phase1OpaqueFixture(*current) {
				return fmt.Errorf("locked State changed before unchanged update")
			}
			return nil
		})
		if err != nil || updated.NextBatch != "s0" {
			t.Fatalf("unchanged update cycle %d: next=%q err=%v", i, updated.NextBatch, err)
		}
		counts.updateCalls++
		counts.cycles++
		closeEpoch()

		if i+1 == phase1WarmupCycles {
			sampleBefore = phase1MeasurementStats()
			sampleCPUStart = phase1ReadProcessCPU(t)
			sampleStart = time.Now()
			sampleLoadBefore, sampleMailboxBefore, sampleUpdateBefore = counts.loadCalls, counts.mailboxCalls, counts.updateCalls
		}
		measuredCycles := i + 1 - phase1WarmupCycles
		if measuredCycles > 0 && measuredCycles%phase1SampleCycles == 0 {
			cpuEnd := phase1ReadProcessCPU(t)
			observations = append(observations, phase1SampleObservation{
				loadCalls: counts.loadCalls - sampleLoadBefore, mailboxCalls: counts.mailboxCalls - sampleMailboxBefore,
				updateCalls: counts.updateCalls - sampleUpdateBefore, elapsed: time.Since(sampleStart),
				userCPU: cpuEnd.user - sampleCPUStart.user, systemCPU: cpuEnd.system - sampleCPUStart.system,
			})
			if measuredCycles == phase1SampleRounds*phase1SampleCycles {
				counts.stats = phase1StatsDelta(sampleBefore, phase1MeasurementStats())
			} else {
				sampleCPUStart = phase1ReadProcessCPU(t)
				sampleStart = time.Now()
				sampleLoadBefore, sampleMailboxBefore, sampleUpdateBefore = counts.loadCalls, counts.mailboxCalls, counts.updateCalls
			}
		}
		if i+1 < total {
			time.Sleep(phase1CycleInterval)
		}
	}
	if len(observations) != phase1SampleRounds {
		t.Fatalf("sample observations=%d, want %d", len(observations), phase1SampleRounds)
	}
	for i, observation := range observations {
		if observation.loadCalls != phase1SampleCycles || observation.mailboxCalls != phase1SampleCycles || observation.updateCalls != phase1SampleCycles {
			t.Fatalf("sample %d direct API calls=(%d,%d,%d), want %d each", i+1,
				observation.loadCalls, observation.mailboxCalls, observation.updateCalls, phase1SampleCycles)
		}
	}
	finalState, err := os.ReadFile(p.StateFile(phase1MeasureIdentity))
	if err != nil || !bytes.Equal(finalState, stateBytes) {
		t.Fatalf("State bytes changed: err=%v equal=%t", err, bytes.Equal(finalState, stateBytes))
	}
	finalMailbox, err := os.ReadFile(p.MailboxFile(phase1MeasureIdentity))
	if err != nil || !bytes.Equal(finalMailbox, mailboxBytes) {
		t.Fatalf("mailbox bytes changed: err=%v equal=%t", err, bytes.Equal(finalMailbox, mailboxBytes))
	}
	t.Logf("fixture: cycles=%d warmup=%d sampleRounds=%d cyclesPerSample=%d interval=%s StateBytes=%d StateSHA256=%x mailboxBytes=%d mailboxSHA256=%x preservedAt=%q",
		counts.cycles, phase1WarmupCycles, phase1SampleRounds, phase1SampleCycles, phase1CycleInterval,
		len(stateBytes), stateHash, len(mailboxBytes), mailboxHash, fixtureDir)
	t.Logf("direct API operations: total LoadState=%d OpenMailbox=%d UpdateState=%d; internalCountersAvailable=%t; aggregateSampleCounters=%+v",
		counts.loadCalls, counts.mailboxCalls, counts.updateCalls, counts.stats.available, counts.stats)
	for i, observation := range observations {
		t.Logf("sample %d direct API calls: LoadState=%d OpenMailbox=%d UpdateState=%d; elapsed=%s; processCPU user=%s system=%s total=%s",
			i+1, observation.loadCalls, observation.mailboxCalls, observation.updateCalls, observation.elapsed,
			observation.userCPU, observation.systemCPU, observation.userCPU+observation.systemCPU)
	}
}

func phase1ReadProcessCPU(t *testing.T) phase1ProcessCPU {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatalf("read process CPU: %v", err)
	}
	return phase1ProcessCPU{
		user:   time.Duration(usage.Utime.Sec)*time.Second + time.Duration(usage.Utime.Usec)*time.Microsecond,
		system: time.Duration(usage.Stime.Sec)*time.Second + time.Duration(usage.Stime.Usec)*time.Microsecond,
	}
}

// phase1PreserveFixture leaves the exact measured inputs in ignored scratch
// when requested. A fresh directory is required so a run never overwrites an
// earlier evidence fixture.
func phase1PreserveFixture(t *testing.T, stateBytes, mailboxBytes []byte) string {
	t.Helper()
	dir := os.Getenv("PHASE1_MEASURE_FIXTURE_DIR")
	if dir == "" {
		return ""
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create fixture evidence directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), stateBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mailbox.jsonl"), mailboxBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func phase1SeedState(t *testing.T, p Paths, name string) []byte {
	t.Helper()
	st := State{NextBatch: "s0", Threads: make([]string, 500), Muted: make([]string, 500), ThreadEvents: make([]ThreadEvent, 2000)}
	for i := range st.Threads {
		st.Threads[i] = fmt.Sprintf("$thread-%063d", i)
		st.Muted[i] = fmt.Sprintf("$muted-%063d", i)
	}
	for i := range st.ThreadEvents {
		st.ThreadEvents[i] = ThreadEvent{Room: fmt.Sprintf("!room-%043d", i%16), EventID: fmt.Sprintf("$event-%043d", i), Root: fmt.Sprintf("$root-%043d", i%64)}
	}
	encoded, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	const opaque = `{"nested":{"values":[true,null,9007199254740993,1.000000000000000000001]}}`
	encoded = bytes.Replace(encoded, []byte("{\n"), []byte("{\n  \"synthetic_future\": "+opaque+",\n"), 1)
	var input State
	if err := unmarshalState(encoded, &input); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(p, name, input); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p.StateFile(name))
	if err != nil {
		t.Fatal(err)
	}
	if !phase1OpaqueFixture(input) {
		t.Fatal("fixture opaque value lost before measurement")
	}
	return raw
}

func phase1SeedMailbox(t *testing.T, p Paths, name string) []byte {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p.MailboxFile(name)), 0o700); err != nil {
		t.Fatal(err)
	}
	var mailbox bytes.Buffer
	ids := make([]string, 200)
	body := strings.Repeat("x", 9800)
	event, err := json.Marshal(map[string]any{"type": "m.room.message", "content": map[string]string{"body": body}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range ids {
		ids[i] = fmt.Sprintf("$mail%05d", i)
		line, err := json.Marshal(mailLine{Op: "add", Entry: &MailEntry{ID: ids[i], Kind: MailEvent, Room: "!fixture:fixture.invalid", Event: event}})
		if err != nil {
			t.Fatal(err)
		}
		mailbox.Write(line)
		mailbox.WriteByte('\n')
	}
	if err := writeLine(&mailbox, mailLine{Op: "rcpt", IDs: ids}); err != nil {
		t.Fatal(err)
	}
	const padding = `{"op":"padding"}`
	pad := (2 << 20) - mailbox.Len() - len(padding) - 1
	if pad < 0 {
		t.Fatalf("mailbox fixture exceeds 2 MiB before padding: %d", mailbox.Len())
	}
	mailbox.Write(bytes.Repeat([]byte(" "), pad))
	mailbox.WriteString(padding + "\n")
	if mailbox.Len() != 2<<20 {
		t.Fatalf("mailbox fixture size=%d", mailbox.Len())
	}
	if err := os.WriteFile(p.MailboxFile(name), mailbox.Bytes(), SecretFileMode); err != nil {
		t.Fatal(err)
	}
	return bytes.Clone(mailbox.Bytes())
}

func phase1OpaqueFixture(st State) bool {
	var compact bytes.Buffer
	err := json.Compact(&compact, st.opaque["synthetic_future"])
	return err == nil && compact.String() == `{"nested":{"values":[true,null,9007199254740993,1.000000000000000000001]}}`
}

func phase1StatsDelta(a, b phase1MeasureStats) phase1MeasureStats {
	return phase1MeasureStats{
		available:      b.available,
		loadStateCalls: b.loadStateCalls - a.loadStateCalls, stateReadAttempts: b.stateReadAttempts - a.stateReadAttempts,
		stateReadErrors: b.stateReadErrors - a.stateReadErrors, stateReadBytes: b.stateReadBytes - a.stateReadBytes,
		unmarshalStatePasses: b.unmarshalStatePasses - a.unmarshalStatePasses, unmarshalStateBytes: b.unmarshalStateBytes - a.unmarshalStateBytes,
		scopeHits: b.scopeHits - a.scopeHits, scopeMisses: b.scopeMisses - a.scopeMisses, scopeFallbacks: b.scopeFallbacks - a.scopeFallbacks,
		writerClones: b.writerClones - a.writerClones, stateDigests: b.stateDigests - a.stateDigests, canonicalStates: b.canonicalStates - a.canonicalStates,
		sealedStates: b.sealedStates - a.sealedStates, writerSealChecks: b.writerSealChecks - a.writerSealChecks,
		stateEncodes: b.stateEncodes - a.stateEncodes, mailboxDecodes: b.mailboxDecodes - a.mailboxDecodes,
		stateWriteBytes: b.stateWriteBytes - a.stateWriteBytes, mailboxWriteBytes: b.mailboxWriteBytes - a.mailboxWriteBytes,
	}
}
