package voiceaudio

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMissingProfile(t *testing.T) {
	r := Request{Bytes: fixture(t, "mono-1s"), TMPDIR: privateTemp(t)}
	refuse(t, r, MissingProfile)
	assertEmpty(t, r.TMPDIR)
}

// These static adapter probes are not codec implementations. Pins are bound
// to the recorded offline control.c build, separately from the real helper.
var controlPins = map[int]string{
	1: "546f727d95939a35d9cf9220e8da36b061c678565f95f5b6f2fce686589ba204",
	2: "05d5a830879d5b73b23f699fd213a704255b4b7b78f4eee11a1a0b392bb9df02",
	3: "9d95156b8b1b4fe6ae9d66782a1027f0373e31d2fbfe385c58b90550c8b2f1c8",
	4: "17c09f5170a286051c21f242264618b5d1e83854f909aa22e50b047516c3ceaa",
	5: "7be4dc13e6be7fd0c3d3a7688420a7564512f7ec0e379fe64ceba1beef20df6b",
	6: "91db9b83a2dbeb656497e023c7b1a1de0a9518d570f2ad21b56f1ae0d63dc4fa",
	7: "661895c4ceaf8e12c7c045787a435592239ef6240e0c14bfe0a9ed3b561c2003",
	8: "ca397e86a6753942df0d3a5298c7bf1a65f931f872b1f1aa8deb87bb4d275645",
	9: "2a63b9e026e320addb859595c848f0468b97e491574e263d07c5ff9c2efaa702",
}

func controlProfile(t *testing.T, mode int) Profile {
	t.Helper()
	_ = nativeProfile(t)
	dir := os.Getenv("A2A_VOICEAUDIO_CONTROLS")
	if dir == "" {
		t.Fatal("explicit controls directory required")
	}
	return Profile{ProfileID, filepath.Join(dir, fmt.Sprintf("control-%d", mode)), controlPins[mode], "linux/amd64"}
}

func TestProfilePinsAndPaths(t *testing.T) {
	p := nativeProfile(t)
	for _, tc := range []struct {
		name   string
		change func(*Profile)
		reason Reason
	}{
		{"missing", func(p *Profile) { p.Executable = "" }, MissingProfile},
		{"absent", func(p *Profile) { p.Executable = "/native/not-provisioned" }, MissingProfile},
		{"wrong-id", func(p *Profile) { p.ID = "untrusted" }, ProfileMismatch},
		{"wrong-pin", func(p *Profile) { p.SHA256 = strings.Repeat("0", 64) }, ProfileMismatch},
		{"bad-pin", func(p *Profile) { p.SHA256 = "unknown" }, ProfileMismatch},
		{"wrong-platform", func(p *Profile) { p.Platform = "darwin/arm64" }, ProfileMismatch},
		{"relative-path", func(p *Profile) { p.Executable = "voiceaudio-decode" }, ProfileMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request(t)
			tc.change(&r.Profile)
			refuse(t, r, tc.reason)
			assertEmpty(t, r.TMPDIR)
			neighbor(t, p)
		})
	}
	t.Run("symlink", func(t *testing.T) {
		dir := privateTemp(t)
		link := filepath.Join(dir, "decoder")
		if err := os.Symlink(p.Executable, link); err != nil {
			t.Fatal(err)
		}
		r := request(t)
		r.Profile.Executable = link
		refuse(t, r, ProfileMismatch)
		assertEmpty(t, r.TMPDIR)
		neighbor(t, p)
	})
	t.Run("non-elf", func(t *testing.T) {
		r := request(t)
		dir := privateTemp(t)
		r.Profile.Executable = filepath.Join(dir, "decoder")
		if err := os.WriteFile(r.Profile.Executable, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
		refuse(t, r, ProfileMismatch)
		assertEmpty(t, r.TMPDIR)
		neighbor(t, p)
	})
	t.Run("writable-binary", func(t *testing.T) {
		r := request(t)
		dir := privateTemp(t)
		b, e := os.ReadFile(p.Executable)
		if e != nil {
			t.Fatal(e)
		}
		r.Profile.Executable = filepath.Join(dir, "decoder")
		if e = os.WriteFile(r.Profile.Executable, b, 0722); e != nil {
			t.Fatal(e)
		}
		if e = os.Chmod(r.Profile.Executable, 0722); e != nil {
			t.Fatal(e)
		}
		refuse(t, r, ProfileMismatch)
		assertEmpty(t, r.TMPDIR)
		neighbor(t, p)
	})
}

func TestTMPDIRAncestryAndForeignPreservation(t *testing.T) {
	p := nativeProfile(t)
	for _, tc := range []string{"missing", "relative", "symlink-root", "symlink-ancestor", "writable-ancestor", "root-mode"} {
		t.Run(tc, func(t *testing.T) {
			r := request(t)
			parent := privateTemp(t)
			root := filepath.Join(parent, "root")
			if e := os.Mkdir(root, 0700); e != nil {
				t.Fatal(e)
			}
			switch tc {
			case "missing":
				r.TMPDIR = filepath.Join(parent, "missing")
			case "relative":
				r.TMPDIR = "."
			case "symlink-root":
				r.TMPDIR = filepath.Join(parent, "link")
				if e := os.Symlink(root, r.TMPDIR); e != nil {
					t.Fatal(e)
				}
			case "symlink-ancestor":
				link := filepath.Join(parent, "link")
				if e := os.Symlink(root, link); e != nil {
					t.Fatal(e)
				}
				if e := os.Mkdir(filepath.Join(root, "child"), 0700); e != nil {
					t.Fatal(e)
				}
				r.TMPDIR = filepath.Join(link, "child")
			case "writable-ancestor":
				if e := os.Chmod(parent, 0777); e != nil {
					t.Fatal(e)
				}
				r.TMPDIR = root
			case "root-mode":
				if e := os.Chmod(root, 0755); e != nil {
					t.Fatal(e)
				}
				r.TMPDIR = root
			}
			refuse(t, r, UnsafeTMPDIR)
			neighbor(t, p)
		})
	}
	t.Run("long-private-valid", func(t *testing.T) {
		r := request(t)
		root := r.TMPDIR
		for i := 0; i < 4; i++ {
			root = filepath.Join(root, strings.Repeat("x", 80))
			if e := os.Mkdir(root, 0700); e != nil {
				t.Fatal(e)
			}
		}
		r.TMPDIR = root
		a, e := Decode(context.Background(), r)
		if e != nil {
			t.Fatal(e)
		}
		assertWAV(t, a, 16000)
		assertEmpty(t, root)
	})
	t.Run("preexisting-foreign", func(t *testing.T) {
		r := request(t)
		path := filepath.Join(r.TMPDIR, "foreign.keep")
		if e := os.WriteFile(path, []byte("keep"), 0600); e != nil {
			t.Fatal(e)
		}
		a, refusal := Decode(context.Background(), r)
		if refusal != nil {
			t.Fatal(refusal)
		}
		assertWAV(t, a, 16000)
		b, e := os.ReadFile(path)
		if e != nil || string(b) != "keep" {
			t.Fatal("foreign file changed")
		}
		entries, e := os.ReadDir(r.TMPDIR)
		if e != nil || len(entries) != 1 {
			t.Fatal("unexpected scratch leftovers")
		}
	})
}

func TestCleanupDoesNotRemoveForeignIdentity(t *testing.T) {
	tmp := privateTemp(t)
	s, r := newScratch(tmp)
	if r != "" {
		t.Fatal(r)
	}
	if !s.writeInput([]byte("owned")) {
		t.Fatal("write")
	}
	if e := s.root.Rename("input.ogg", "original-held"); e != nil {
		t.Fatal(e)
	}
	if e := s.root.WriteFile("input.ogg", []byte("foreign"), 0600); e != nil {
		t.Fatal(e)
	}
	path := s.path
	if s.cleanup() {
		t.Fatal("foreign identity was not refused")
	}
	b, e := os.ReadFile(filepath.Join(path, "input.ogg"))
	if e != nil || string(b) != "foreign" {
		t.Fatal("foreign replacement deleted")
	}
	b, e = os.ReadFile(filepath.Join(path, "original-held"))
	if e != nil || string(b) != "owned" {
		t.Fatal("untracked file deleted")
	}
}

func TestNativeCleanupRefusesForeignCreatedFile(t *testing.T) {
	r := request(t)
	r.Profile = controlProfile(t, 3)
	refuse(t, r, CleanupRefused)
	entries, e := os.ReadDir(r.TMPDIR)
	if e != nil || len(entries) != 1 {
		t.Fatal("expected preserved operation directory")
	}
	op := filepath.Join(r.TMPDIR, entries[0].Name())
	files, e := os.ReadDir(op)
	if e != nil || len(files) != 1 || files[0].Name() != "foreign.keep" {
		t.Fatalf("unexpected files %v %v", files, e)
	}
	b, e := os.ReadFile(filepath.Join(op, "foreign.keep"))
	if e != nil || string(b) != "synthetic foreign sentinel" {
		t.Fatal("foreign file deleted")
	}
	neighbor(t, nativeProfile(t))
}

func TestInertAdapterOutputAndEnvironment(t *testing.T) {
	for _, tc := range []struct {
		mode   int
		reason Reason
	}{{4, Malformed}, {5, DurationLimit}, {6, OutputInvalid}, {7, OutputInvalid}, {9, DiagnosticLimit}} {
		t.Run(strconv.Itoa(tc.mode), func(t *testing.T) {
			r := request(t)
			r.Profile = controlProfile(t, tc.mode)
			refuse(t, r, tc.reason)
			assertEmpty(t, r.TMPDIR)
			neighbor(t, nativeProfile(t))
		})
	}
	t.Setenv("AUDIO_SENTINEL", "untrusted inherited environment")
	r := request(t)
	r.Profile = controlProfile(t, 8)
	a, e := Decode(context.Background(), r)
	if e != nil {
		t.Fatal("clean environment/private modes probe:", e)
	}
	assertWAV(t, a, 16000)
	assertEmpty(t, r.TMPDIR)
}

func TestCancellationTimeoutBusyAndProcessGroup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    int
		timeout bool
	}{{"cancel", 1, false}, {"timeout", 1, true}, {"descendants", 2, false}} {
		t.Run(tc.name, func(t *testing.T) {
			r := request(t)
			r.Profile = controlProfile(t, tc.mode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.timeout {
				ctx, cancel = context.WithTimeout(context.Background(), 750*time.Millisecond)
				defer cancel()
			}
			type result struct {
				a Audio
				e *Refusal
			}
			done := make(chan result, 1)
			go func() { a, e := Decode(ctx, r); done <- result{a, e} }()
			marker := ""
			var fields []string
			deadline := time.Now().Add(500 * time.Millisecond)
			for time.Now().Before(deadline) {
				matches, _ := filepath.Glob(filepath.Join(r.TMPDIR, "voiceaudio-*", "started"))
				if len(matches) == 1 {
					b, e := os.ReadFile(matches[0])
					if e == nil && len(strings.Fields(string(b))) == 2 {
						marker = matches[0]
						fields = strings.Fields(string(b))
						break
					}
				}
				time.Sleep(2 * time.Millisecond)
			}
			if marker == "" {
				cancel()
				<-done
				t.Fatal("actual child did not reach marker")
			}
			if e := os.Remove(marker); e != nil {
				t.Fatal(e)
			}
			pids := []int{}
			for _, f := range fields {
				pid, e := strconv.Atoi(f)
				if e != nil {
					t.Fatal(e)
				}
				if pid > 0 {
					pids = append(pids, pid)
					if syscall.Kill(pid, 0) != nil {
						t.Fatal("probe process not alive")
					}
				}
			}
			// While the owned process is known live, a second call refuses busy.
			refuse(t, Request{Bytes: fixture(t, "mono-1s"), TMPDIR: privateTemp(t), Profile: nativeProfile(t)}, Busy)
			start := time.Now()
			if !tc.timeout {
				cancel()
			}
			select {
			case got := <-done:
				want := Cancelled
				if tc.timeout {
					want = Timeout
				}
				if got.e == nil || got.e.Reason != want || got.a.WAV != nil {
					t.Fatalf("wrong joined refusal: %v", got.e)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancellation did not join")
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("slow cancellation")
			}
			for _, pid := range pids {
				until := time.Now().Add(time.Second)
				for syscall.Kill(pid, 0) == nil && time.Now().Before(until) {
					time.Sleep(time.Millisecond)
				}
				if e := syscall.Kill(pid, 0); e != syscall.ESRCH {
					t.Fatalf("owned process %d remains: %v", pid, e)
				}
			}
			assertEmpty(t, r.TMPDIR)
			neighbor(t, nativeProfile(t))
		})
	}
}

func TestSnapshotSourceReplacementDoesNotRedirect(t *testing.T) {
	p := nativeProfile(t)
	dir := privateTemp(t)
	source := filepath.Join(dir, "source")
	bytesBefore, e := os.ReadFile(p.Executable)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(source, bytesBefore, 0700); e != nil {
		t.Fatal(e)
	}
	p.Executable = source
	tmp := privateTemp(t)
	s, r := newScratch(tmp)
	if r != "" {
		t.Fatal(r)
	}
	defer func() {
		if !s.cleanup() {
			t.Error("snapshot cleanup")
		}
		assertEmpty(t, tmp)
	}()
	if !s.writeInput(fixture(t, "mono-1s")) {
		t.Fatal("input")
	}
	if reason := snapshot(context.Background(), s, p); reason != "" {
		t.Fatal(reason)
	}
	if e = os.Rename(source, source+".held"); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(source, []byte("untrusted replacement"), 0700); e != nil {
		t.Fatal(e)
	}
	output, reason := runNative(context.Background(), s)
	if reason != "" || len(output) != 32012 || !bytes.Equal(output[:12], nativeHeader) {
		t.Fatalf("snapshot redirected: %s bytes=%d", reason, len(output))
	}
}

func TestBoundedOutputRetainsExactlyOneExtraByte(t *testing.T) {
	killed := false
	w := &boundedOutput{stop: func() { killed = true }}
	p := make([]byte, MaxPCMBytes+10000)
	n, e := w.Write(p)
	if n != len(p) || e == nil || !killed || !w.overflow || w.Len() != 12+MaxPCMBytes+1 {
		t.Fatalf("unbounded output: n=%d e=%v length=%d killed=%v", n, e, w.Len(), killed)
	}
}
