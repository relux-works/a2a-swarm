package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func fakeBuildInfo(revision, modified string) *debug.BuildInfo {
	return &debug.BuildInfo{
		Path: "github.com/relux-works/a2a-swarm/cmd/a2a",
		Main: debug.Module{Path: "github.com/relux-works/a2a-swarm"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: revision},
			{Key: "vcs.modified", Value: modified},
		},
	}
}

func stubRunning(t *testing.T, executable, argv0, revision string) {
	t.Helper()
	oldExe, oldInfo, oldArgv0 := runningExecutablePath, runningBuildInfo, runningArgv0
	runningExecutablePath = func() (string, error) { return executable, nil }
	runningBuildInfo = func() (*debug.BuildInfo, bool) { return fakeBuildInfo(revision, "false"), true }
	runningArgv0 = func() string { return argv0 }
	t.Cleanup(func() { runningExecutablePath, runningBuildInfo, runningArgv0 = oldExe, oldInfo, oldArgv0 })
}

// Claim: the reported revision comes from the loaded image's own build
// info, never from the launch alias file: an alias symlink to a real Go
// binary (the test binary, whose embedded build info is not the stub's)
// changes nothing, so a regression that read the alias target's build
// info would fail here. A modified tree is marked, and missing build
// info is unknown rather than guessed from any file.
func TestRunningRevisionFromLoadedBuildInfo(t *testing.T) {
	dir := t.TempDir()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "a2a")
	if err := os.Symlink(testBinary, alias); err != nil {
		t.Fatal(err)
	}
	rev := strings.Repeat("a", 40)
	stubRunning(t, filepath.Join(dir, "a2a-image"), alias, rev)
	if got := resolveRunningInfo(); got.Revision != rev {
		t.Fatalf("revision=%q, want loaded build info %q (alias target %q carries its own build info)", got.Revision, rev, testBinary)
	}
	if got := runningRevision(fakeBuildInfo(rev, "true")); got != rev+"+modified" {
		t.Fatalf("modified revision=%q", got)
	}
	if got := runningRevision(nil); got != "unknown" {
		t.Fatalf("missing build info=%q", got)
	}
	if got := runningRevision(&debug.BuildInfo{}); got != "unknown" {
		t.Fatalf("empty build info=%q", got)
	}
}

// Claim: bind status shows the running consumer's PID, self-resolved
// executable (quoted), revision, alias verdict and refresh status, in
// text and JSON, only for a record the live consumer itself wrote (same
// PID and generation). Beside the on-alias control, an off-alias record
// prints the fixed not-on-the-launch-alias line exactly once. A record
// by another PID or generation, an unstamped legacy record, and a
// consumer with no record at all show running=unknown (not yet recorded)
// and never the other consumer's revision; a stopped binding shows no
// running claim and no running_pid in either form.
func TestBindStatusRunningObservation(t *testing.T) {
	rev := strings.Repeat("c", 40)
	own := func(on bool) *config.TerminalObserve {
		return &config.TerminalObserve{Executable: "/installed/a2a-image", Revision: rev, OnLaunchAlias: on, PID: 4242, Generation: "gen"}
	}
	for _, tc := range []struct {
		name     string
		observe  *config.TerminalObserve
		stopped  bool
		onAlias  *bool
		fixedLn  int
		hasField bool
		unknown  bool
	}{
		{"on alias", own(true), false, boolPtr(true), 0, true, false},
		{"off alias", func() *config.TerminalObserve { o := own(false); o.Executable = "/checkout/a2a"; return o }(), false, boolPtr(false), 1, true, false},
		{"no record", nil, false, nil, 0, false, true},
		{"another pid", func() *config.TerminalObserve { o := own(true); o.PID = 9999; return o }(), false, nil, 0, false, true},
		{"another generation", func() *config.TerminalObserve { o := own(true); o.Generation = "old-gen"; return o }(), false, nil, 0, false, true},
		{"unstamped record", &config.TerminalObserve{Executable: "/installed/a2a-image", Revision: rev, OnLaunchAlias: true}, false, nil, 0, false, true},
		{"stopped", own(true), true, nil, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := world(t)
			notificationState(t, e, func(st *config.State) {
				st.Terminal = &config.TerminalBinding{Generation: "gen", DaemonPID: 4242, RefreshStatus: "current", Stopped: tc.stopped, Observe: tc.observe}
			})
			text := e.as("alice", "bind", "status").ok(t).stdout
			wantRefresh := "current"
			if tc.stopped {
				wantRefresh = "unavailable"
			}
			if !strings.Contains(text, "refresh="+wantRefresh) || !strings.Contains(text, "pid=4242") {
				t.Fatalf("status missing refresh/pid: %s", text)
			}
			if got := strings.Count(text, notOnLaunchAliasLine+"\n"); got != tc.fixedLn {
				t.Fatalf("fixed line count=%d, want %d: %s", got, tc.fixedLn, text)
			}
			var out struct {
				Running    string `json:"running"`
				PID        int    `json:"running_pid"`
				Executable string `json:"running_executable"`
				Revision   string `json:"running_revision"`
				OnAlias    *bool  `json:"on_launch_alias"`
				Refresh    string `json:"refresh"`
			}
			raw := e.as("alice", "bind", "status", "--json").ok(t).stdout
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				t.Fatal(err)
			}
			if out.Refresh != wantRefresh {
				t.Fatalf("json refresh: %+v", out)
			}
			if tc.unknown {
				if !strings.Contains(text, "running="+runningUnknown+"\n") || out.Running != runningUnknown {
					t.Fatalf("live binding without an own record not shown as unknown: %+v / %s", out, text)
				}
			} else if strings.Contains(text, "running="+runningUnknown) || out.Running != "" {
				t.Fatalf("unexpected unknown running claim: %+v / %s", out, text)
			}
			if !tc.hasField {
				if out.PID != 0 || out.Executable != "" || out.Revision != "" || out.OnAlias != nil || strings.Contains(text, "running_executable=") || strings.Contains(text, "running_revision="+rev) {
					t.Fatalf("record not the live consumer's invented running fields: %+v / %s", out, text)
				}
				return
			}
			if out.PID != 4242 {
				t.Fatalf("json running pid: %+v", out)
			}
			want := "running_pid=4242 running_executable=\"" + tc.observe.Executable + "\" running_revision=" + rev
			if !strings.Contains(text, want) {
				t.Fatalf("status missing %q: %s", want, text)
			}
			if out.Executable != tc.observe.Executable || out.Revision != rev || out.OnAlias == nil || *out.OnAlias != *tc.onAlias {
				t.Fatalf("json running fields: %+v", out)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }

// Claim: the owning consumer records its running image at start, a re-exec
// record replaces executable and revision with the new image's, and the
// recorded CPU episode fence survives that replacement. A foreign
// generation records nothing.
func TestRecordRunningInfoAcrossReexec(t *testing.T) {
	e, _ := world(t)
	a, s := notificationApp(t, e)
	notificationState(t, e, func(st *config.State) {
		st.Terminal = &config.TerminalBinding{Generation: "gen", DaemonPID: os.Getpid()}
	})
	rev1, rev2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	stubRunning(t, "/installed/a2a-one", "/installed/a2a-one", rev1)
	if a.recordRunningInfo(s, "other") {
		t.Fatal("foreign generation reported its record committed")
	}
	if got := e.state("alice").Terminal.Observe; got != nil {
		t.Fatalf("foreign generation recorded: %+v", got)
	}
	if !a.recordRunningInfo(s, "gen") {
		t.Fatal("own record did not report committed")
	}
	got := e.state("alice").Terminal.Observe
	if got == nil || got.Executable != "/installed/a2a-one" || got.Revision != rev1 || got.OnLaunchAlias || got.PID != os.Getpid() || got.Generation != "gen" {
		t.Fatalf("first record: %+v", got)
	}
	if !a.recordRunningInfo(s, "gen") {
		t.Fatal("unchanged own record did not report proven committed")
	}
	alertAt := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	notificationState(t, e, func(st *config.State) {
		st.Terminal.Observe.CPUAlerted = true
		st.Terminal.Observe.CPUAlertAt = alertAt
		st.Terminal.Observe.CPUJobID = "a2a-mail-alarm-job"
	})
	// The re-exec: a new image and revision, same generation and PID.
	runningExecutablePath = func() (string, error) { return "/installed/a2a-two", nil }
	runningBuildInfo = func() (*debug.BuildInfo, bool) { return fakeBuildInfo(rev2, "false"), true }
	if !a.recordRunningInfo(s, "gen") {
		t.Fatal("re-exec record did not report committed")
	}
	got = e.state("alice").Terminal.Observe
	if got == nil || got.Executable != "/installed/a2a-two" || got.Revision != rev2 || got.PID != os.Getpid() || got.Generation != "gen" {
		t.Fatalf("re-exec record did not replace image/revision: %+v", got)
	}
	if !got.CPUAlerted || !got.CPUAlertAt.Equal(alertAt) || got.CPUJobID != "a2a-mail-alarm-job" {
		t.Fatalf("re-exec lost the cpu episode fence: %+v", got)
	}
}
