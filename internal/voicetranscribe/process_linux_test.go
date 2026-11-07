//go:build linux

package voicetranscribe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func operationFile(t *testing.T, root, file string) string {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		entries, err := os.ReadDir(root)
		must(t, err)
		for _, entry := range entries {
			path := filepath.Join(root, entry.Name(), file)
			if _, err := os.Stat(path); err == nil {
				return path
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("private child did not reach the intended process boundary")
	return ""
}

func awaitOutcome(t *testing.T, done <-chan Outcome) Outcome {
	t.Helper()
	select {
	case out := <-done:
		return out
	case <-time.After(6 * time.Second):
		t.Fatal("child cancellation/join exceeded fixture bound")
		return Outcome{}
	}
}

func TestProcessGateCancelTimeoutCleanup(t *testing.T) {
	for _, name := range []string{"cancel", "timeout", "cleanup"} {
		t.Run(name, func(t *testing.T) {
			a, tmp := fixture(t)
			r := request(7)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan Outcome, 1)
			go func() { done <- a.Transcribe(ctx, r) }()
			operationFile(t, tmp, "ready")
			// A separate Adapter must share the same no-queue process gate.
			other, otherTmp := fixture(t)
			sibling := request(0)
			assertRefusal(t, other.Transcribe(context.Background(), sibling), sibling, Busy, GateStage)
			emptyScratch(t, otherTmp)
			code, stage := Cancelled, InferenceStage
			if name == "cleanup" {
				must(t, os.Chmod(tmp, 0500))
				defer os.Chmod(tmp, 0700)
				code, stage = CleanupFailed, CleanupStage
			}
			if name == "timeout" {
				code = Timeout
			} else {
				cancel()
			}
			out := awaitOutcome(t, done)
			assertRefusal(t, out, r, code, stage)
			if name == "cleanup" {
				must(t, os.Chmod(tmp, 0700))
				entries, err := os.ReadDir(tmp)
				must(t, err)
				if len(entries) != 1 {
					t.Fatal("cleanup failure did not leave only the owned operation directory")
				}
				must(t, os.RemoveAll(filepath.Join(tmp, entries[0].Name())))
			}
			emptyScratch(t, tmp)
			assertSuccess(t, other.Transcribe(context.Background(), sibling), sibling)
			assertSuccess(t, a.Transcribe(context.Background(), sibling), sibling)
			emptyScratch(t, tmp)
			emptyScratch(t, otherTmp)
		})
	}
}

func running(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
		return false
	}
	if err != nil {
		return true
	}
	// A killed descendant may briefly remain a zombie awaiting namespace init.
	end := strings.LastIndexByte(string(data), ')')
	return end < 0 || end+2 >= len(data) ||
		(data[end+2] != 'Z' && data[end+2] != 'X' && data[end+2] != 'x')
}

func TestCancellationKillsDescendantGroup(t *testing.T) {
	a, tmp := fixture(t)
	r := request(8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Outcome, 1)
	go func() { done <- a.Transcribe(ctx, r) }()
	path := operationFile(t, tmp, "descendant")
	b, err := os.ReadFile(path)
	must(t, err)
	pid, err := strconv.Atoi(string(b))
	must(t, err)
	if !running(pid) {
		t.Fatal("descendant negative did not start a live child")
	}
	cancel()
	assertRefusal(t, awaitOutcome(t, done), r, Cancelled, InferenceStage)
	until := time.Now().Add(time.Second)
	for {
		// Use the same observation for the verdict. Procfs can transition
		// through zombie/dead/disappeared while a namespace reaper runs;
		// a second independent read is not the first observation's result.
		if !running(pid) {
			break
		}
		if !time.Now().Before(until) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("cancelled engine left a live descendant")
		}
		time.Sleep(5 * time.Millisecond)
	}
	emptyScratch(t, tmp)
	sibling := request(0)
	assertSuccess(t, a.Transcribe(context.Background(), sibling), sibling)
}

func TestSpecialArtifactRefusal(t *testing.T) {
	a, tmp := fixture(t)
	path := filepath.Join(a.profile.Directory, a.model[0].Path)
	must(t, os.Rename(path, path+".held"))
	must(t, syscall.Mkfifo(path, 0600))
	r := request(0)
	assertRefusal(t, a.Transcribe(context.Background(), r), r, ModelIntegrity, ModelStage)
	must(t, os.Remove(path))
	must(t, os.Rename(path+".held", path))
	assertSuccess(t, a.Transcribe(context.Background(), r), r)
	emptyScratch(t, tmp)
}

// Read only this test's isolated PID namespace. Select a direct child of the
// test executable with exactly the operation-owned CLI path, never a host PID.
func nativePID(t *testing.T, root string) int {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		entries, err := os.ReadDir("/proc")
		must(t, err)
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			cmd, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
			if err != nil {
				continue
			}
			exe := strings.SplitN(string(cmd), "\x00", 2)[0]
			if !strings.HasPrefix(exe, root+"/a2a-stt-") || !strings.HasSuffix(exe, "/bin/sherpa-onnx-offline") {
				continue
			}
			status, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status"))
			if err != nil {
				continue
			}
			if strings.Contains(string(status), "PPid:\t"+strconv.Itoa(os.Getpid())+"\n") {
				return pid
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("real pinned native process was not observed; intended control not reached")
	return 0
}

func TestRealOfflineProcessControls(t *testing.T) {
	a, tmp, wavs := realFixture(t)
	for _, name := range []string{"busy-cancel", "timeout", "cleanup"} {
		t.Run(name, func(t *testing.T) {
			r := request(0)
			// Repeat the nonpersonal public sample to the admitted 30s PCM cap.
			pcm := make([]byte, MaxPCMBytes)
			for offset := 0; offset < len(pcm); {
				offset += copy(pcm[offset:], wavs["ru"][44:])
			}
			r.Audio = wav(pcm)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan Outcome, 1)
			go func() { done <- a.Transcribe(ctx, r) }()
			pid := nativePID(t, tmp)
			// Deliberately stop only the observed private child so the negative
			// reaches native inference even on a fast machine. SIGKILL on the
			// caller deadline must still join it and clean the real snapshot.
			must(t, syscall.Kill(pid, syscall.SIGSTOP))
			sibling := request(0)
			sibling.Audio = wavs["en"]
			assertRefusal(t, New(PinnedProfile(a.profile.Directory), t.TempDir()).Transcribe(context.Background(), sibling), sibling, Busy, GateStage)
			code, stage := Cancelled, InferenceStage
			if name == "cleanup" {
				must(t, os.Chmod(tmp, 0500))
				defer os.Chmod(tmp, 0700)
				code, stage = CleanupFailed, CleanupStage
			}
			if name == "timeout" {
				code = Timeout
			} else {
				cancel()
			}
			assertRefusal(t, awaitOutcome(t, done), r, code, stage)
			if running(pid) {
				t.Fatal("real native child remained live after outcome")
			}
			if name == "cleanup" {
				must(t, os.Chmod(tmp, 0700))
				entries, err := os.ReadDir(tmp)
				must(t, err)
				if len(entries) != 1 {
					t.Fatal("real cleanup refusal did not retain exactly its owned directory")
				}
				must(t, os.RemoveAll(filepath.Join(tmp, entries[0].Name())))
			}
			emptyScratch(t, tmp)
			realSuccess(t, a, wavs["en"], "en")
			emptyScratch(t, tmp)
		})
	}
}
