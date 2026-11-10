//go:build darwin || linux

package daemonsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
	"golang.org/x/sys/unix"
)

func testPlan() Plan {
	return Plan{Version: 1, Platform: "darwin", Image: "/opt/a2a/image/a2a", ImageSHA256: strings.Repeat("a", 64), Home: "/private/var/a2a", Policy: "/Library/ApplicationSupport/a2a/owner.json", PolicySHA256: strings.Repeat("b", 64), UID: 401, GID: 401, SweepSeconds: 300, SoakDays: 3}
}

// W-UNIT with C-SECRET/C-SOCKET: both grammars contain required supervision and
// reject additions or mutations; planted violations establish checker detection.
func TestRenderedUnitGrammar(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			p := testPlan()
			p.Platform = platform
			files, e := Render(p)
			if e != nil || ValidateUnits(p, files) != nil {
				t.Fatal("valid neighbour", e)
			}
			joined := []byte{}
			for _, b := range files {
				joined = append(joined, b...)
			}
			required := []string{p.Image, p.ImageSHA256, p.PolicySHA256, "serve", "sweep"}
			if platform == "darwin" {
				required = append(required, "<integer>30</integer>", "<integer>300</integer>", "prepare-run-dir", "<string>_a2a</string>", "<key>KeepAlive</key>")
			} else {
				required = append(required, "User=a2a", "Restart=on-failure", "RestartSec=30", "RuntimeDirectory=a2a", "RuntimeDirectoryMode=0711", "OnUnitActiveSec=300s")
			}
			for _, s := range required {
				if !bytes.Contains(joined, []byte(s)) {
					t.Fatalf("missing %s", s)
				}
			}
			for _, plant := range []string{"Environment=TOKEN=secret-control", "ListenStream=/extra.sock"} {
				mut := map[string][]byte{}
				for name, b := range files {
					mut[name] = append(append([]byte(nil), b...), []byte(plant)...)
					break
				}
				for name, b := range files {
					if _, ok := mut[name]; !ok {
						mut[name] = b
					}
				}
				if !errors.Is(ValidateUnits(p, mut), ErrRefused) {
					t.Fatal("control not detected", plant)
				}
			}
		})
	}
}

// W-PLAN/C-PLAN: exact authority fields, duplicate keys, invalid paths, missing
// cadence and unknown secret/socket fields fail beside a valid serialized plan.
func TestPlanClosedSchema(t *testing.T) {
	p := testPlan()
	b, _ := json.Marshal(p)
	if _, e := DecodePlan(b); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{strings.Replace(string(b), `"version":1`, `"version":1,"version":1`, 1), strings.Replace(string(b), `"version":1`, `"Version":1`, 1), strings.Replace(string(b), `"sweep_seconds":300`, `"sweep_seconds":0`, 1), strings.TrimSuffix(string(b), "}") + `,"token":"control"}`, strings.Replace(string(b), p.Image, "/opt/../unsafe", 1), string(b) + " garbage", string(b) + " {}"} {
		if _, e := DecodePlan([]byte(bad)); !errors.Is(e, ErrRefused) {
			t.Fatalf("accepted invalid plan: %s", bad)
		}
	}
}

type fakeSupervisor struct {
	tuitestkit.MockCallRecorder
	states   map[string]State
	failStop bool
}

func (s *fakeSupervisor) State(_ context.Context, _ Plan, name string) (State, error) {
	s.Record("state", name)
	v, ok := s.states[name]
	if !ok {
		return Absent, nil
	}
	return v, nil
}
func (s *fakeSupervisor) Start(_ context.Context, _ Plan, name string) error {
	s.Record("start", name)
	s.states[name] = Running
	return nil
}
func (s *fakeSupervisor) Stop(_ context.Context, _ Plan, name string) error {
	s.Record("stop", name)
	if s.failStop {
		return ErrUnresolved
	}
	s.states[name] = Absent
	return nil
}
func (s *fakeSupervisor) Reload(context.Context, Plan) error { s.Record("reload"); return nil }

type ownership struct {
	uid, gid uint32
	mode     uint32
}

// Fixtures never chown, need root, or touch a host unit/run path. Metadata
// mapping is the only fake; no-follow opens and actual file operations are real.
func fixtureDisk(t *testing.T) (disk, map[uint64]ownership) {
	t.Helper()
	meta := map[uint64]ownership{}
	stat := func(fd int, st *unix.Stat_t) error {
		if e := unix.Fstat(fd, st); e != nil {
			return e
		}
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			st.Uid = 0
			st.Gid = 0
			st.Mode = (st.Mode &^ 07777) | 0755
		} else {
			st.Uid = 0
			st.Gid = 401
		}
		if v, ok := meta[st.Ino]; ok {
			st.Uid = v.uid
			st.Gid = v.gid
			if v.mode != 0 {
				st.Mode &^= 07777
				for bit := uint(0); bit < 12; bit++ {
					if v.mode&(1<<bit) != 0 {
						st.Mode |= 1 << bit
					}
				}
			}
		}
		return nil
	}
	d := disk{stat: stat, noWrite: func(int, string) bool { return true }, chown: func(fd, uid, gid int) error {
		var st unix.Stat_t
		if e := unix.Fstat(fd, &st); e != nil {
			return e
		}
		meta[st.Ino] = ownership{uint32(uid), uint32(gid), 0711}
		return nil
	}}
	return d, meta
}
func inode(t *testing.T, path string) uint64 {
	t.Helper()
	var st unix.Stat_t
	if unix.Lstat(path, &st) != nil {
		t.Fatal("stat fixture")
	}
	return st.Ino
}
func physicalTemp(t *testing.T) string {
	t.Helper()
	p, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return p
}

// W-BOOT/C-RUNDIR: absent boot directory is published as 401:401/0711; a
// correct existing inode is retained. Symlink/file/wrong metadata never change.
func TestPrepareRunDirRefusals(t *testing.T) {
	for _, kind := range []string{"absent", "correct", "symlink", "file", "owner", "group", "mode"} {
		t.Run(kind, func(t *testing.T) {
			root := physicalTemp(t)
			target := filepath.Join(root, "a2a")
			d, meta := fixtureDisk(t)
			if kind == "symlink" {
				if os.Symlink(root, target) != nil {
					t.Fatal("symlink")
				}
			} else if kind == "file" {
				if os.WriteFile(target, []byte("untouched"), 0600) != nil {
					t.Fatal("write")
				}
			} else if kind != "absent" {
				if os.Mkdir(target, 0711) != nil {
					t.Fatal("mkdir")
				}
				v := ownership{401, 401, 0711}
				switch kind {
				case "owner":
					v.uid = 42
				case "group":
					v.gid = 42
				case "mode":
					v.mode = 0755
				}
				meta[inode(t, target)] = v
			}
			var before os.FileInfo
			before, _ = os.Lstat(target)
			e := d.prepare(target, 401, 401)
			if kind == "absent" || kind == "correct" {
				if e != nil {
					t.Fatal("valid neighbour", e)
				}
				f, e := d.directory(target)
				if e != nil {
					t.Fatal(e)
				}
				var st unix.Stat_t
				d.stat(int(f.Fd()), &st)
				f.Close()
				if st.Uid != 401 || st.Gid != 401 || st.Mode&07777 != 0711 {
					t.Fatal("wrong prepared metadata")
				}
				if before != nil {
					after, _ := os.Lstat(target)
					if !os.SameFile(before, after) {
						t.Fatal("correct directory replaced")
					}
				}
			} else {
				if !errors.Is(e, ErrRefused) {
					t.Fatal("unsafe accepted")
				}
				after, _ := os.Lstat(target)
				if !os.SameFile(before, after) || before.Mode() != after.Mode() {
					t.Fatal("unsafe object changed")
				}
				children, _ := os.ReadDir(root)
				if len(children) != 1 {
					t.Fatal("refusal wrote sibling")
				}
			}
		})
	}
}

// W-PIN/C-PIN: real no-follow reads with mapped root metadata admit exactly
// root:401/0440 and refuse wrong owner/group/mode, symlinks, parent and hash.
func TestPolicyStartupRefusals(t *testing.T) {
	for _, kind := range []string{"valid", "owner", "group", "mode", "symlink", "parent", "hash", "missing", "duplicate", "extra"} {
		t.Run(kind, func(t *testing.T) {
			root := physicalTemp(t)
			path := filepath.Join(root, "owner.json")
			d, meta := fixtureDisk(t)
			b := []byte(`{"version":1,"owner":"@ivan:fixture.invalid","identities":["alice"]}`)
			if kind == "duplicate" {
				b = []byte(`{"version":1,"owner":"@ivan:fixture.invalid","identities":["alice","alice"]}`)
			}
			if kind == "extra" {
				b = []byte(`{"version":1,"owner":"@ivan:fixture.invalid","identities":["alice"],"token":"control"}`)
			}
			if kind != "missing" {
				if os.WriteFile(path, b, 0440) != nil || os.Chmod(path, 0440) != nil {
					t.Fatal("fixture write")
				}
			}
			if kind == "symlink" {
				if os.Rename(path, path+".real") != nil || os.Symlink(path+".real", path) != nil {
					t.Fatal("symlink")
				}
			}
			if kind == "parent" {
				meta[inode(t, root)] = ownership{42, 401, 0755}
			}
			if kind == "owner" || kind == "group" || kind == "mode" {
				v := ownership{0, 401, 0440}
				switch kind {
				case "owner":
					v.uid = 401
				case "group":
					v.gid = 42
				case "mode":
					v.mode = 0640
				}
				meta[inode(t, path)] = v
			}
			hash := Digest(b)
			if kind == "hash" {
				hash = strings.Repeat("0", 64)
			}
			p, e := d.policy(path, hash, 401)
			if kind == "valid" {
				if e != nil || p.Owner != "@ivan:fixture.invalid" || !p.Allows("alice") || p.Allows("mallory") {
					t.Fatal("valid neighbour", e)
				}
				p.Identities[0] = "mallory"
				again, e := d.policy(path, hash, 401)
				if e != nil || !again.Allows("alice") {
					t.Fatal("snapshot mutated allowed file")
				}
			} else if !errors.Is(e, ErrRefused) {
				t.Fatal("unsafe startup admitted")
			}
		})
	}
}

// W-S3/C-UNCERTAIN: unknown state and failed joins preserve the entire unit
// set. Known running and stopped neighbours are stopped/joined then removed.
func TestServiceTransactions(t *testing.T) {
	for _, kind := range []string{"absent", "running", "stopped", "unknown", "stop-failure", "mismatch"} {
		t.Run(kind, func(t *testing.T) {
			root := physicalTemp(t)
			p := testPlan()
			d, _ := fixtureDisk(t)
			s := &fakeSupervisor{states: map[string]State{}, failStop: kind == "stop-failure"}
			m := &Manager{Supervisor: s, d: d, dir: root}
			files, _ := Render(p)
			if kind == "absent" {
				if _, e := m.Run(context.Background(), p, "install"); e != nil {
					t.Fatal(e)
				}
				tuitestkit.AssertCalledN(t, &s.MockCallRecorder, "start", 3)
				return
			}
			state := Running
			if kind == "stopped" {
				state = Stopped
			}
			if kind == "unknown" {
				state = Unknown
			}
			for n, b := range files {
				if kind == "mismatch" {
					b = append(append([]byte(nil), b...), []byte("secret-control")...)
				}
				if os.WriteFile(filepath.Join(root, n), b, 0644) != nil || os.Chmod(filepath.Join(root, n), 0644) != nil {
					t.Fatal("write")
				}
				s.states[n] = state
			}
			before := map[string][]byte{}
			for n := range files {
				before[n], _ = os.ReadFile(filepath.Join(root, n))
			}
			_, e := m.Run(context.Background(), p, "uninstall")
			if kind == "unknown" || kind == "stop-failure" || kind == "mismatch" {
				if !errors.Is(e, ErrUnresolved) {
					t.Fatal("expected unresolved", e)
				}
				for n, b := range before {
					after, _ := os.ReadFile(filepath.Join(root, n))
					if !bytes.Equal(b, after) {
						t.Fatal("unresolved removed files")
					}
				}
				if kind != "stop-failure" {
					tuitestkit.AssertNotCalled(t, &s.MockCallRecorder, "stop")
				}
			} else {
				if e != nil {
					t.Fatal(e)
				}
				for n := range files {
					if _, e := os.Lstat(filepath.Join(root, n)); !os.IsNotExist(e) {
						t.Fatal("matching stopped file retained")
					}
				}
				tuitestkit.AssertCalledN(t, &s.MockCallRecorder, "stop", 3)
			}
		})
	}
}

// W-STATE/C-STATE: production parsers reject ambiguous/foreign loaded jobs;
// they never infer absence from arbitrary nonzero tool exits.
func TestNativeSupervisorStateParsing(t *testing.T) {
	p := testPlan()
	name := Label + ".plist"
	text := "program = " + p.Image + "\nusername = _a2a\npath = " + filepath.Join(p.UnitDir(), name) + "\narguments = {\n\t" + strings.Join(p.args("serve"), "\n\t") + "\n\t}\nstate = running\n"
	for _, tc := range []struct {
		name string
		r    commandResult
		want State
	}{{"running", commandResult{[]byte(text), 0}, Running}, {"foreign", commandResult{[]byte(strings.Replace(text, p.Image, "/foreign", 1)), 0}, Unknown}, {"error", commandResult{[]byte("permission denied"), 1}, Unknown}, {"missing", commandResult{[]byte("Could not find service"), 113}, Absent}, {"inconclusive", commandResult{nil, 0}, Unknown}} {
		t.Run(tc.name, func(t *testing.T) {
			s := nativeSupervisor{run: func(context.Context, string, ...string) commandResult { return tc.r }}
			got, _ := s.State(context.Background(), p, name)
			if got != tc.want {
				t.Fatal(got, tc.want)
			}
		})
	}
}

// W-HEARTBEAT/C-DEATH: alive, stale boundary, future and missing pulses are
// distinguished within one 300s sweep cadence, without invoking a supervisor.
func TestHeartbeatDeathWithinCadence(t *testing.T) {
	home := physicalTemp(t)
	os.Chmod(home, 0700)
	now := time.Now()
	if !daemonDown(home, now) {
		t.Fatal("missing alive")
	}
	h, e := OpenHeartbeat(home)
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	if e = h.Pulse(now); e != nil {
		t.Fatal(e)
	}
	if daemonDown(home, now) || !daemonDown(home, now.Add(StaleSeconds*time.Second)) || !daemonDown(home, now.Add(300*time.Second)) || !daemonDown(home, now.Add(-time.Second)) {
		t.Fatal("liveness boundary")
	}
	if e := h.Close(); e != nil {
		t.Fatal(e)
	}
	if !daemonDown(home, now) {
		t.Fatal("fresh pulse hid daemon death")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if h.Run(ctx, make(chan time.Time)) != nil {
		t.Fatal("bounded stop")
	}
}

// W-SWEEP/C-RECEIPT: a snapshot sees pending/shown counts and cannot alter a
// mailbox, State, lease, sync cursor or outbox; a known receipt reduces counts.
func TestSweepCountsAndNoWrites(t *testing.T) {
	home := physicalTemp(t)
	os.Chmod(home, 0700)
	base := filepath.Join(home, "identities", "alice")
	if os.MkdirAll(filepath.Join(base, "state"), 0700) != nil {
		t.Fatal("mkdir")
	}
	os.Chmod(base, 0700)
	p := config.Paths{Base: base}
	mb, e := config.OpenMailbox(p, "alice")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = mb.Append([]config.MailEntry{{ID: "$a", Kind: config.MailEvent, Event: json.RawMessage(`{"body":"secret-control"}`)}, {ID: "$b", Kind: config.MailEvent}}); e != nil {
		t.Fatal(e)
	}
	mb.MarkShown([]string{"$a"})
	sentinels := []string{p.MailboxFile("alice"), filepath.Join(base, "state", "alice.json"), filepath.Join(base, "outbox-control")}
	for _, path := range sentinels[1:] {
		os.WriteFile(path, []byte("lease-sync-outbox-control"), 0600)
	}
	before := map[string][]byte{}
	for _, path := range sentinels {
		before[path], _ = os.ReadFile(path)
	}
	pol := Policy{Version: 1, Owner: "@ivan:fixture.invalid", Identities: []string{"alice", "missing"}}
	r := Sweep(home, pol, time.Now())
	if r.Liveness != "daemon_down" || len(r.Findings) != 2 || r.Findings[0].Pending != 2 || r.Findings[0].Shown != 1 || !r.Findings[1].Unreadable {
		t.Fatal(r)
	}
	if e = WriteReport(home, "sweep-report.json", r); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(home, "sweep-report.json"))
	if bytes.Contains(b, []byte("secret-control")) || bytes.Contains(b, []byte("$a")) {
		t.Fatal("report leaked body/id")
	}
	for path, b := range before {
		after, _ := os.ReadFile(path)
		if !bytes.Equal(after, b) {
			t.Fatal("sweep wrote", path)
		}
	}
	mb.Receive([]string{"$a"})
	r = Sweep(home, pol, time.Now())
	if r.Findings[0].Pending != 1 || r.Findings[0].Shown != 0 {
		t.Fatal("receipt positive control")
	}
}

// W-RESCUE/C-SOAK: real-traffic observations with zero rescues qualify only
// after the selected interval; a recovery count, missing traffic or torn log
// cannot certify zero-rescue soak. No artifact removes a hook or emits receipt.
func TestRescueSoakAccounting(t *testing.T) {
	home := physicalTemp(t)
	os.Chmod(home, 0700)
	p := Policy{Identities: []string{"alice"}}
	from := time.Now().Add(-72 * time.Hour)
	to := time.Now()
	if _, e := Soak(home, p, from, to, 3); !errors.Is(e, ErrRefused) {
		t.Fatal("missing log qualified")
	}
	if e := RecordRescue(home, p, RescueRecord{At: to, Identity: "alice", Layer: "traffic", Count: 2}); e != nil {
		t.Fatal(e)
	}
	r, e := Soak(home, p, from, to, 3)
	if e != nil || !r.Qualified {
		t.Fatal("valid neighbour", r, e)
	}
	if _, e = Soak(home, p, to.Add(-time.Hour), to, 3); !errors.Is(e, ErrRefused) {
		t.Fatal("short soak")
	}
	if e = RecordRescue(home, p, RescueRecord{At: to, Identity: "alice", Layer: "watchdog", Count: 1}); e != nil {
		t.Fatal(e)
	}
	r, e = Soak(home, p, from, to, 3)
	if e != nil || r.Qualified || r.Findings[0].Rescues != 1 {
		t.Fatal("rescue ignored")
	}
	if e = RecordRescue(home, p, RescueRecord{At: to, Identity: "mallory", Layer: "manual", Count: 1}); !errors.Is(e, ErrRefused) {
		t.Fatal("foreign identity accounted")
	}
	log := filepath.Join(home, "alice.rescue.jsonl")
	f, _ := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0600)
	f.Write([]byte("{"))
	f.Close()
	if _, e = Soak(home, p, from, to, 3); !errors.Is(e, ErrRefused) {
		t.Fatal("torn log qualified")
	}
}

// W-NAMED/C-PROFILE: sweep resolves the same selected named identity mailbox
// as the daemon; a decoy legacy mailbox is never counted or changed.
func TestSweepNamedConnection(t *testing.T) {
	home := physicalTemp(t)
	os.Chmod(home, 0700)
	base := filepath.Join(home, "identities", "alice")
	os.MkdirAll(base, 0700)
	c := config.Connection{ID: "primary", URL: "https://fixture.invalid", ServerName: "fixture.invalid", Store: config.CredentialStore{Backend: "file"}}
	root := config.Paths{Base: base}
	registry := config.ConnectionRegistry{SchemaVersion: 1, Current: "primary", Profiles: map[string]config.Connection{"primary": c}}
	b, _ := json.Marshal(registry)
	if e := os.WriteFile(root.ConnectionsFile(), b, 0600); e != nil {
		t.Fatal(e)
	}
	selected, e := c.Paths(root, "alice", false)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(filepath.Join(selected.Base, "state"), 0700); e != nil {
		t.Fatal(e)
	}
	mb, e := config.OpenMailbox(selected, "alice")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = mb.Append([]config.MailEntry{{ID: "$named", Kind: config.MailEvent}}); e != nil {
		t.Fatal(e)
	}
	os.MkdirAll(filepath.Join(base, "state"), 0700)
	decoy, e := config.OpenMailbox(root, "alice")
	if e != nil {
		t.Fatal(e)
	}
	decoy.Append([]config.MailEntry{{ID: "$decoy-1"}, {ID: "$decoy-2"}})
	before, _ := os.ReadFile(selected.MailboxFile("alice"))
	r := Sweep(home, Policy{Identities: []string{"alice"}}, time.Now())
	if len(r.Findings) != 1 || r.Findings[0].Unreadable || r.Findings[0].Pending != 1 {
		t.Fatal(r)
	}
	after, _ := os.ReadFile(selected.MailboxFile("alice"))
	if !bytes.Equal(before, after) {
		t.Fatal("named snapshot wrote")
	}
}

// W-LINUX-STATE/C-FOREIGN: exact loaded image argv, service account, fragment
// and stable substate are required; pending reload or transitional jobs refuse.
func TestLinuxSupervisorStateParsing(t *testing.T) {
	p := testPlan()
	p.Platform = "linux"
	name := Label + ".service"
	output := "LoadState=loaded\nActiveState=active\nSubState=running\nFragmentPath=" + filepath.Join(p.UnitDir(), name) + "\nNeedDaemonReload=no\nUser=a2a\nGroup=a2a\nExecStart={ path=" + p.Image + " ; argv[]=" + strings.Join(p.args("serve"), " ") + " ; }\n"
	for _, tc := range []struct {
		name, out string
		want      State
	}{{"valid", output, Running}, {"reload", strings.Replace(output, "NeedDaemonReload=no", "NeedDaemonReload=yes", 1), Unknown}, {"foreign", strings.Replace(output, "User=a2a", "User=root", 1), Unknown}, {"transition", strings.Replace(output, "SubState=running", "SubState=start", 1), Unknown}, {"missing", "LoadState=not-found\nActiveState=inactive\n", Absent}} {
		t.Run(tc.name, func(t *testing.T) {
			s := nativeSupervisor{run: func(context.Context, string, ...string) commandResult { return commandResult{[]byte(tc.out), 0} }}
			got, _ := s.State(context.Background(), p, name)
			if got != tc.want {
				t.Fatal(got, tc.want)
			}
		})
	}
}

// W-TEMPLATES/C-TEMPLATE-DRIFT: checked-in inert rendered examples match the
// closed renderer exactly. Fake hashes make these examples non-installable.
func TestRenderedExamples(t *testing.T) {
	for _, platform := range []string{"darwin", "linux"} {
		t.Run(platform, func(t *testing.T) {
			base := filepath.Join("testdata", platform)
			b, e := os.ReadFile(filepath.Join(base, "plan.json"))
			if e != nil {
				t.Fatal(e)
			}
			p, e := DecodePlan(b)
			if e != nil {
				t.Fatal(e)
			}
			files, e := Render(p)
			if e != nil {
				t.Fatal(e)
			}
			for name, want := range files {
				got, e := os.ReadFile(filepath.Join(base, name))
				if e != nil || !bytes.Equal(got, want) {
					t.Fatal("rendered example drift", name, e)
				}
			}
		})
	}
}
