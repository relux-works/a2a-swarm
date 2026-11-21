//go:build phase1measure && darwin && arm64

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

const (
	phase1IdleBindChildEnv = "A2A_PHASE1_IDLE_BIND_CHILD"
	phase1IdleBindWarmup   = 5 * time.Second
	phase1IdleBindSample   = 10 * time.Minute
	phase1IdleBindSession  = "phase1-fixture"
)

// TestPhase1IdleBindCPUWorkload measures the production detached-consumer loop
// in a private tmux owner fixture with a local fake homeserver. Its child is
// this exact candidate test image and runs runTerminalWithRefresh; no installed
// runtime, credential, or shared identity participates.
func TestPhase1IdleBindCPUWorkload(t *testing.T) {
	phase1RunIdleBindMeasurement(t, phase1IdleBindSample)
}

// TestPhase1IdleBindSmoke is the bounded setup/ownership control for the
// ten-minute lane. The task's performance verdict comes only from the workload
// test above.
func TestPhase1IdleBindSmoke(t *testing.T) {
	phase1RunIdleBindMeasurement(t, 20*time.Second)
}

// TestPhase1IdleBindChild is started only by the two parent measurement tests.
func TestPhase1IdleBindChild(t *testing.T) {
	if os.Getenv(phase1IdleBindChildEnv) != "1" {
		t.Skip("private idle-bind measurement child")
	}
	phase1RunIdleBindChild(t)
}

func phase1RunIdleBindMeasurement(t *testing.T, sample time.Duration) {
	isolateBindTest(t)
	for _, name := range []string{"tmux", "sleep"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip(name + " unavailable; private idle-bind measurement not run")
		}
	}
	e, _ := world(t)
	e.extra["CODEX_THREAD_ID"] = phase1IdleBindSession
	e.as("alice", "wait", "--timeout", "1ms").ok(t)

	dir, err := os.MkdirTemp("/tmp", "a2a-phase1-")
	if err != nil {
		t.Fatalf("create short private tmux directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "tmux.sock")
	tmux := exec.Command("tmux", "-S", socket, "-f", "/dev/null", "new-session", "-d", "-x", "120", "-y", "24", "sleep", "900")
	if raw, err := tmux.CombinedOutput(); err != nil {
		t.Fatalf("start private tmux owner: %v %s", err, raw)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-S", socket, "kill-server").Run()
	})
	target, err := tmuxwake.Pin(context.Background(), socket, "%0", "codex")
	if err != nil {
		t.Fatalf("pin private fixture pane: %v", err)
	}
	ownerBirth, err := tmuxwake.Birth(context.Background(), target.PanePID)
	if err != nil {
		t.Fatalf("read fixture owner birth: %v", err)
	}
	target.OwnerPID, target.OwnerBirth = target.PanePID, ownerBirth
	generation, err := config.NewBindToken()
	if err != nil {
		t.Fatal(err)
	}
	phase1SeedIdleBindState(t, e.paths(), target, generation, ownerBirth)
	if _, err := config.OpenMailbox(e.paths(), "alice"); err != nil {
		t.Fatalf("prepare empty delivery mailbox: %v", err)
	}
	if err := os.WriteFile(e.paths().MailboxFile("alice"), nil, 0o600); err != nil {
		t.Fatalf("persist empty delivery mailbox: %v", err)
	}

	fixtureDir := os.Getenv("PHASE1_IDLE_BIND_FIXTURE_DIR")
	if fixtureDir != "" {
		if err := os.Mkdir(fixtureDir, 0700); err != nil {
			t.Fatalf("create preserved fixture directory: %v", err)
		}
	}
	syncsBefore := e.hs.SyncCount("@alice:" + testServer)
	e.hs.mu.Lock()
	txnsBefore := len(e.hs.txns)
	e.hs.mu.Unlock()

	cmd := exec.Command(os.Args[0], "-test.run=^TestPhase1IdleBindChild$", "-test.v", "-test.timeout=13m")
	cmd.Env = append(os.Environ(),
		phase1IdleBindChildEnv+"=1",
		"A2A_CONFIG_DIR="+e.dir,
		"A2A_AGENT=alice",
		"CODEX_THREAD_ID="+phase1IdleBindSession,
		"PHASE1_IDLE_BIND_GENERATION="+generation,
		"PHASE1_IDLE_BIND_SAMPLE="+sample.String(),
		"PHASE1_IDLE_BIND_FIXTURE_DIR="+fixtureDir,
	)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	harnessStartCPU, _ := phase1BindCPUUsage(t)
	runErr := cmd.Run()
	harnessEndCPU, _ := phase1BindCPUUsage(t)
	harnessCPU := harnessEndCPU - harnessStartCPU
	if runErr != nil {
		diagnostic := ""
		if fixtureDir != "" {
			afterChildDir := filepath.Join(fixtureDir, "after-child")
			if mkdirErr := os.Mkdir(afterChildDir, 0700); mkdirErr != nil {
				diagnostic = fmt.Sprintf("; create after-child evidence directory: %v", mkdirErr)
			} else if preserveErr := phase1PreserveIdleBindFixture(t, afterChildDir, e.paths(), "alice"); preserveErr != nil {
				diagnostic = fmt.Sprintf("; preserve after-child fixture: %v", preserveErr)
			}
		}
		t.Fatalf("idle-bind workload child failed: %v; harness self CPU=%s%s\n%s", runErr, harnessCPU, diagnostic, output.String())
	}
	if !strings.Contains(output.String(), "PHASE1_IDLE_BIND_RESULT=") {
		t.Fatalf("idle-bind workload child omitted its result: %s", output.String())
	}

	state, err := config.LoadState(e.paths(), "alice")
	if err != nil || !config.BindPartActive(state) || state.Terminal == nil || state.Terminal.Generation != generation {
		t.Fatalf("fixture binding lost its active generation: active=%t terminal=%+v err=%v", config.BindPartActive(state), state.Terminal, err)
	}
	mailbox, err := config.OpenMailbox(e.paths(), "alice")
	if err != nil || len(mailbox.Pending()) != 0 {
		t.Fatalf("idle sample changed delivery mailbox: pending=%d err=%v", len(mailbox.Pending()), err)
	}
	syncsAfter := e.hs.SyncCount("@alice:" + testServer)
	e.hs.mu.Lock()
	txnsAfter := len(e.hs.txns)
	e.hs.mu.Unlock()
	if syncsAfter <= syncsBefore || txnsAfter != txnsBefore {
		t.Fatalf("fixture did not exercise idle sync without sends: syncs %d->%d, transactions %d->%d\n%s", syncsBefore, syncsAfter, txnsBefore, txnsAfter, output.String())
	}
	t.Logf("idle-bind workload image=%s sample=%s syncs=%d->%d sends=%d->%d harness-self-cpu=%s fixture=%q\n%s",
		filepath.Base(os.Args[0]), sample, syncsBefore, syncsAfter, txnsBefore, txnsAfter, harnessCPU, fixtureDir, output.String())
}

func phase1SeedIdleBindState(t *testing.T, p config.Paths, target tmuxwake.Target, generation, ownerBirth string) {
	t.Helper()
	state, err := config.LoadState(p, "alice")
	if err != nil || state.NextBatch == "" {
		t.Fatalf("initial delivery cursor unavailable: cursor=%q err=%v", state.NextBatch, err)
	}
	state.Threads = make([]string, 450)
	state.Muted = make([]string, 450)
	for i := range state.Threads {
		state.Threads[i] = fmt.Sprintf("$thread-%063d", i)
		state.Muted[i] = fmt.Sprintf("$muted-%063d", i)
	}
	state.ThreadEvents = make([]config.ThreadEvent, 2000)
	for i := range state.ThreadEvents {
		state.ThreadEvents[i] = config.ThreadEvent{Room: fmt.Sprintf("!room-%043d", i%16), EventID: fmt.Sprintf("$event-%043d", i), Root: fmt.Sprintf("$root-%043d", i%64)}
	}
	state.Lease = nil // the initializer wait process has exited; the fixture consumer takes its own lease.
	daemonBirth, err := tmuxwake.Birth(context.Background(), os.Getpid())
	if err != nil {
		t.Fatalf("read fixture initializer birth: %v", err)
	}
	state.Terminal = &config.TerminalBinding{
		Generation: generation, SessionID: phase1IdleBindSession, Host: machineLabel(),
		OwnerPID: target.OwnerPID, OwnerBirth: ownerBirth, DaemonPID: os.Getpid(),
		DaemonBirth: daemonBirth, Target: target, Interval: defaultBindInterval, RetryMax: 2 * time.Second,
		MaxWakes: 30, Outcome: "starting",
	}
	if err := config.SaveState(p, "alice", state); err != nil {
		t.Fatalf("save phase-1 State fixture: %v", err)
	}
	marked, err := config.EnsureBindPart(p, "alice", config.BindPartOpenOptions{Now: time.Now()})
	if err != nil || !config.BindPartActive(marked) || marked.Terminal == nil || marked.Terminal.Generation != generation {
		t.Fatalf("activate marked bind fixture: active=%t terminal=%+v err=%v", config.BindPartActive(marked), marked.Terminal, err)
	}
	raw, err := os.ReadFile(p.StateFile("alice"))
	if err != nil || len(raw) < 350<<10 || len(raw) > 600<<10 {
		t.Fatalf("phase-1 State fixture size=%d (want 350..600 KiB): %v", len(raw), err)
	}
}

func phase1RunIdleBindChild(t *testing.T) {
	t.Helper()
	sample, err := time.ParseDuration(os.Getenv("PHASE1_IDLE_BIND_SAMPLE"))
	if err != nil || sample <= 0 {
		t.Fatalf("invalid private sample duration: %v", err)
	}
	image, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	imageBytes, err := os.ReadFile(image)
	if err != nil {
		t.Fatal(err)
	}
	imageHash := fmt.Sprintf("%x", sha256.Sum256(imageBytes))
	a := &App{
		as: "alice", Getenv: os.Getenv, HTTP: http.DefaultClient, Now: time.Now,
		Stdout: io.Discard, Stderr: os.Stderr, ctx: context.Background(),
		afterBindingCycleError: func(stage string, err error) {
			t.Logf("idle-bind fixture consumer retry stage=%s error=%T: %v", stage, err, err)
		},
	}
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	generation := os.Getenv("PHASE1_IDLE_BIND_GENERATION")
	ownerState, err := config.LoadState(s.paths, "alice")
	if err != nil || !config.BindPartActive(ownerState) || ownerState.Terminal == nil || ownerState.Terminal.Generation != generation {
		t.Fatalf("child refused unowned fixture State: active=%t terminal=%+v err=%v", config.BindPartActive(ownerState), ownerState.Terminal, err)
	}
	if birth, err := tmuxwake.Birth(context.Background(), ownerState.Terminal.OwnerPID); err != nil || birth != ownerState.Terminal.OwnerBirth {
		t.Fatalf("fixture process owner is not live: birth=%q want=%q err=%v", birth, ownerState.Terminal.OwnerBirth, err)
	}
	daemonBirth, err := tmuxwake.Birth(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.UpdateState(s.paths, "alice", func(st *config.State) error {
		if st.Terminal == nil || st.Terminal.Generation != generation || st.Terminal.Stopped {
			return errors.New("fixture generation changed before consumer start")
		}
		st.Terminal.DaemonPID, st.Terminal.DaemonBirth = os.Getpid(), daemonBirth
		return nil
	}); err != nil {
		t.Fatalf("record fixture consumer owner: %v", err)
	}
	if err := a.acquireLease(s, phase1IdleBindSession, time.Now().Add(5*time.Second)); err != nil {
		t.Fatalf("acquire fixture bind lease: %v", err)
	}
	initial, err := config.LoadState(s.paths, "alice")
	if err != nil || initial.Terminal == nil || initial.Terminal.DaemonPID != os.Getpid() || initial.Terminal.DaemonBirth != daemonBirth {
		t.Fatalf("fixture consumer identity not retained: terminal=%+v err=%v", initial.Terminal, err)
	}
	if fixtureDir := os.Getenv("PHASE1_IDLE_BIND_FIXTURE_DIR"); fixtureDir != "" {
		beforeLoopDir := filepath.Join(fixtureDir, "before-loop")
		if err := os.Mkdir(beforeLoopDir, 0700); err != nil {
			t.Fatalf("create before-loop evidence directory: %v", err)
		}
		if err := phase1PreserveIdleBindFixture(t, beforeLoopDir, s.paths, "alice"); err != nil {
			t.Fatalf("preserve before-loop fixture: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		reason string
		err    error
	}, 1)
	go func() {
		reason, runErr := a.runTerminalWithRefresh(ctx, s, generation, *initial.Terminal, nil)
		done <- struct {
			reason string
			err    error
		}{reason: reason, err: runErr}
	}()
	if err := phase1WaitIdleBindReady(s.paths, generation); err != nil {
		cancel()
		select {
		case result := <-done:
			t.Fatalf("%v; fixture consumer exited reason=%q err=%v", err, result.reason, result.err)
		case <-time.After(10 * time.Second):
			t.Fatalf("%v; fixture consumer did not exit after cancellation", err)
		}
	}
	if fixtureDir := os.Getenv("PHASE1_IDLE_BIND_FIXTURE_DIR"); fixtureDir != "" {
		if err := phase1PreserveIdleBindFixture(t, fixtureDir, s.paths, "alice"); err != nil {
			cancel()
			<-done
			t.Fatal(err)
		}
	}
	var cpuProfile *os.File
	profilePath := os.Getenv("PHASE1_IDLE_BIND_CPU_PROFILE")
	profileStarted := false
	if profilePath != "" {
		cpuProfile, err = os.Create(profilePath)
		if err != nil {
			cancel()
			<-done
			t.Fatalf("create CPU profile: %v", err)
		}
		if err := pprof.StartCPUProfile(cpuProfile); err != nil {
			_ = cpuProfile.Close()
			cancel()
			<-done
			t.Fatalf("start CPU profile: %v", err)
		}
		profileStarted = true
		defer func() {
			if profileStarted {
				pprof.StopCPUProfile()
			}
			if cpuProfile != nil {
				_ = cpuProfile.Close()
			}
		}()
	}
	startStats := config.ReadStateDecodeStats()
	startSelfCPU, startChildCPU := phase1BindCPUUsage(t)
	sampleStart := time.Now()
	timer := time.NewTimer(sample)
	<-timer.C
	endStats := config.ReadStateDecodeStats()
	endSelfCPU, endChildCPU := phase1BindCPUUsage(t)
	elapsed := time.Since(sampleStart)
	if profileStarted {
		pprof.StopCPUProfile()
		profileStarted = false
		if err := cpuProfile.Close(); err != nil {
			t.Errorf("close CPU profile: %v", err)
		}
		cpuProfile = nil
		if report, err := exec.Command("go", "tool", "pprof", "-top", "-nodecount=40", image, profilePath).CombinedOutput(); err != nil {
			t.Logf("CPU profile report unavailable: %v", err)
		} else if err := os.WriteFile(profilePath+".top", report, 0600); err != nil {
			t.Errorf("write CPU profile report: %v", err)
		}
	}
	fixtureDir := os.Getenv("PHASE1_IDLE_BIND_FIXTURE_DIR")
	if fixtureDir != "" {
		sampleEndDir := filepath.Join(fixtureDir, "sample-end")
		if err := os.Mkdir(sampleEndDir, 0700); err != nil {
			cancel()
			<-done
			t.Fatalf("create sample-end evidence directory: %v", err)
		}
		if err := phase1PreserveIdleBindFixture(t, sampleEndDir, s.paths, "alice"); err != nil {
			cancel()
			<-done
			t.Fatal(err)
		}
	}
	cancel()
	var result struct {
		reason string
		err    error
	}
	select {
	case result = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("fixture consumer did not exit after cancellation")
	}
	delta := phase1StateStatsDelta(startStats, endStats)
	selfCPU := endSelfCPU - startSelfCPU
	childCPU := endChildCPU - startChildCPU
	cpuUsed := selfCPU + childCPU
	share := float64(cpuUsed) / float64(elapsed) * 100
	stateBytes, stateErr := os.ReadFile(s.paths.StateFile("alice"))
	_, mailboxErr := os.ReadFile(s.paths.MailboxFile("alice"))
	var fixtureStateEqual, fixtureMailboxEqual bool
	if fixtureDir != "" {
		beforeState, err := os.ReadFile(filepath.Join(fixtureDir, "state.json"))
		if err == nil {
			sampleEndState, sampleEndErr := os.ReadFile(filepath.Join(fixtureDir, "sample-end", "state.json"))
			fixtureStateEqual = sampleEndErr == nil && bytes.Equal(beforeState, sampleEndState)
		}
		beforeMailbox, err := os.ReadFile(filepath.Join(fixtureDir, "mailbox.jsonl"))
		if err == nil {
			sampleEndMailbox, sampleEndErr := os.ReadFile(filepath.Join(fixtureDir, "sample-end", "mailbox.jsonl"))
			fixtureMailboxEqual = sampleEndErr == nil && bytes.Equal(beforeMailbox, sampleEndMailbox)
		}
	}
	viewState, viewErr := config.LoadState(s.paths, "alice")
	view, partErr := config.ReadBindPartView(s.paths, "alice", viewState)
	metrics := struct {
		ImageSHA256      string                  `json:"image_sha256"`
		Sample           string                  `json:"sample"`
		Elapsed          string                  `json:"elapsed"`
		StateBytes       int                     `json:"state_bytes"`
		ThreadCount      int                     `json:"thread_count"`
		ThreadEventCount int                     `json:"thread_event_count"`
		CPU              string                  `json:"cpu"`
		SelfCPU          string                  `json:"self_cpu"`
		ChildCPU         string                  `json:"child_cpu"`
		OneCorePct       float64                 `json:"one_core_percent"`
		Counters         config.StateDecodeStats `json:"counter_deltas"`
		Reason           string                  `json:"exit_reason"`
	}{ImageSHA256: imageHash, Sample: sample.String(), Elapsed: elapsed.String(), StateBytes: len(stateBytes), ThreadCount: len(viewState.Threads), ThreadEventCount: len(viewState.ThreadEvents), CPU: cpuUsed.String(), SelfCPU: selfCPU.String(), ChildCPU: childCPU.String(), OneCorePct: share, Counters: delta, Reason: result.reason}
	encoded, _ := json.Marshal(metrics)
	fmt.Printf("PHASE1_IDLE_BIND_RESULT=%s\n", encoded)
	if result.err != nil || result.reason != "cancelled" {
		t.Errorf("fixture consumer exit: reason=%q err=%v", result.reason, result.err)
	}
	if stateErr != nil || mailboxErr != nil || viewErr != nil || partErr != nil || !config.BindPartActive(viewState) || view.Generation != generation {
		t.Errorf("fixture State/part validation failed: stateErr=%v mailboxErr=%v viewErr=%v partErr=%v", stateErr, mailboxErr, viewErr, partErr)
	}
	if fixtureDir != "" && (!fixtureStateEqual || !fixtureMailboxEqual) {
		t.Errorf("steady idle sample changed State or mailbox bytes: StateEqual=%t MailboxEqual=%t", fixtureStateEqual, fixtureMailboxEqual)
	}
	if delta.StateReadAttempts != 0 || delta.StateReadErrors != 0 || delta.StateReadBytes != 0 ||
		delta.UnmarshalStatePasses != 0 || delta.UnmarshalStateBytes != 0 || delta.SealedStates != 0 || delta.StateWriteBytes != 0 {
		t.Errorf("unchanged idle ticks read, decoded, resealed, or wrote the large State: %+v", delta)
	}
	// CPU remains an observed metric; this phase has no percentage failure threshold.
}

func phase1WaitIdleBindReady(p config.Paths, generation string) error {
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := config.LoadState(p, "alice")
		if err == nil && config.BindPartActive(state) && state.Terminal != nil && state.Terminal.Generation == generation && state.Terminal.Outcome == "armed: no new mail" {
			return nil
		}
		select {
		case <-deadline.C:
			return fmt.Errorf("fixture consumer did not reach armed idle state: terminal=%+v err=%v", state.Terminal, err)
		case <-ticker.C:
		}
	}
}

func phase1PreserveIdleBindFixture(t *testing.T, dir string, p config.Paths, name string) error {
	t.Helper()
	files := map[string]string{
		"state.json":     p.StateFile(name),
		"bind-part.json": p.BindPartFile(name),
		"mailbox.jsonl":  p.MailboxFile(name),
	}
	for target, source := range files {
		b, err := os.ReadFile(source)
		if err != nil {
			return fmt.Errorf("read fixture %s: %w", target, err)
		}
		if err := os.WriteFile(filepath.Join(dir, target), b, 0600); err != nil {
			return fmt.Errorf("preserve fixture %s: %w", target, err)
		}
	}
	return nil
}

func phase1BindCPUUsage(t *testing.T) (time.Duration, time.Duration) {
	t.Helper()
	read := func(who int) time.Duration {
		var usage syscall.Rusage
		if err := syscall.Getrusage(who, &usage); err != nil {
			t.Fatalf("read process CPU usage: %v", err)
		}
		return time.Duration(usage.Utime.Sec)*time.Second + time.Duration(usage.Utime.Usec)*time.Microsecond +
			time.Duration(usage.Stime.Sec)*time.Second + time.Duration(usage.Stime.Usec)*time.Microsecond
	}
	return read(syscall.RUSAGE_SELF), read(syscall.RUSAGE_CHILDREN)
}

func phase1StateStatsDelta(a, b config.StateDecodeStats) config.StateDecodeStats {
	return config.StateDecodeStats{
		LoadStateCalls: b.LoadStateCalls - a.LoadStateCalls, StateReadAttempts: b.StateReadAttempts - a.StateReadAttempts,
		StateReadErrors: b.StateReadErrors - a.StateReadErrors, StateReadBytes: b.StateReadBytes - a.StateReadBytes,
		UnmarshalStatePasses: b.UnmarshalStatePasses - a.UnmarshalStatePasses, UnmarshalStateBytes: b.UnmarshalStateBytes - a.UnmarshalStateBytes,
		ScopeHits: b.ScopeHits - a.ScopeHits, ScopeMisses: b.ScopeMisses - a.ScopeMisses, ScopeFallbacks: b.ScopeFallbacks - a.ScopeFallbacks,
		WriterClones: b.WriterClones - a.WriterClones, StateDigests: b.StateDigests - a.StateDigests, CanonicalStates: b.CanonicalStates - a.CanonicalStates,
		SealedStates: b.SealedStates - a.SealedStates, WriterSealChecks: b.WriterSealChecks - a.WriterSealChecks,
		StateEncodes: b.StateEncodes - a.StateEncodes, MailboxDecodes: b.MailboxDecodes - a.MailboxDecodes,
		StateWriteBytes: b.StateWriteBytes - a.StateWriteBytes, MailboxWriteBytes: b.MailboxWriteBytes - a.MailboxWriteBytes,
	}
}
