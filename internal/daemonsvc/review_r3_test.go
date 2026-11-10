//go:build darwin || linux

package daemonsvc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
	"golang.org/x/sys/unix"
)

// W-STOCK-PARENT/C-SYSTEM-GID/C-PARENT-ACL: explicit metadata rows drive the
// actual prepare parent gate. Source's root:daemon/0775 shape succeeds; foreign
// groups, non-root, world write, ACL write, links and writable ancestors refuse
// without chown or any child creation. No system parent is touched.
func TestRunParentPlatformPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, platform string
		uid, gid, mode uint32
		valid          bool
	}{
		{"mac_stock_daemon", "darwin", 0, 1, 0775, true},
		{"mac_wheel_group_write", "darwin", 0, 0, 0775, true},
		{"mac_wheel_readonly", "darwin", 0, 0, 0755, true},
		{"mac_daemon_readonly", "darwin", 0, 1, 0755, true},
		{"mac_foreign_group", "darwin", 0, 42, 0775, false},
		{"mac_unlisted_group", "darwin", 0, 999, 0775, false},
		{"mac_foreign_readonly", "darwin", 0, 42, 0755, false},
		{"mac_nonroot", "darwin", 401, 1, 0775, false},
		{"mac_world_write", "darwin", 0, 1, 0777, false},
		{"mac_acl_write", "darwin", 0, 1, 0775, false},
		{"mac_symlink", "darwin", 0, 1, 0775, false},
		{"mac_ancestor_group_write", "darwin", 0, 1, 0775, false},
		{"linux_previous_rule", "linux", 0, 0, 0755, true},
		{"linux_readonly_gid_immaterial", "linux", 0, 999, 0755, true},
		{"linux_no_group_write", "linux", 0, 0, 0775, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := physicalTemp(t)
			parent := filepath.Join(root, "parent")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			d, meta := fixtureDisk(t, parent)
			parentInode := inode(t, parent)
			meta[parentInode] = ownership{tc.uid, tc.gid, tc.mode}
			if tc.name == "mac_ancestor_group_write" {
				meta[inode(t, root)] = ownership{0, 1, 0775}
			}
			aclSeen := false
			d.aclSafe = func(fd int) bool {
				var st unix.Stat_t
				if unix.Fstat(fd, &st) != nil {
					return false
				}
				if st.Ino == parentInode {
					aclSeen = true
					return tc.name != "mac_acl_write"
				}
				return true
			}
			if tc.name == "mac_symlink" {
				if os.Rename(parent, parent+"-real") != nil || os.Symlink(parent+"-real", parent) != nil {
					t.Fatal("link plant")
				}
			}
			chowns := 0
			chown := d.chown
			d.chown = func(fd, uid, gid int) error { chowns++; return chown(fd, uid, gid) }
			before, err := os.Lstat(parent)
			if err != nil {
				t.Fatal(err)
			}
			err = d.prepareFor(filepath.Join(parent, "a2a"), 401, 401, tc.platform)
			if tc.valid {
				if err != nil || chowns != 1 || !aclSeen {
					t.Fatalf("valid parent: %v, chowns=%d, acl=%v", err, chowns, aclSeen)
				}
			} else {
				if !errors.Is(err, ErrRefused) || chowns != 0 {
					t.Fatalf("refusal=%v, chowns=%d", err, chowns)
				}
				children, e := os.ReadDir(parent)
				if e != nil || len(children) != 0 {
					t.Fatal("refused parent changed", e)
				}
				if tc.name == "mac_acl_write" && !aclSeen {
					t.Fatal("ACL plant was not consulted")
				}
			}
			after, err := os.Lstat(parent)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatal("system-parent analogue was adopted", err)
			}
		})
	}
}

// C-UNMAPPED: the metadata seam leaves an unmapped directory's actual mode,
// uid and gid intact; a blanket rewrite to root/0755 cannot satisfy this row.
func TestFixtureDiskKeepsUnmappedDirectoryMetadata(t *testing.T) {
	root := physicalTemp(t)
	d, _ := fixtureDisk(t, root)
	child := filepath.Join(root, "unmapped")
	if os.Mkdir(child, 0775) != nil || os.Chmod(child, 0775) != nil {
		t.Fatal("mkdir")
	}
	f, err := d.directory(child)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var actual, mapped unix.Stat_t
	if unix.Fstat(int(f.Fd()), &actual) != nil || d.stat(int(f.Fd()), &mapped) != nil || actual.Uid != mapped.Uid || actual.Gid != mapped.Gid || actual.Mode != mapped.Mode {
		t.Fatal("unmapped metadata rewritten")
	}
}

// W-POLICY-ACL/C-ACL-GRANT: each trusted parent, ancestor and file descriptor
// drives the ACL seam, including when tests execute as root. Native ACL syscall
// behaviour is a later precheck; fixture bytes and modes stay unchanged.
func TestPolicyACLRefusals(t *testing.T) {
	for _, kind := range []string{"valid", "parent", "ancestor", "file"} {
		t.Run(kind, func(t *testing.T) {
			root := physicalTemp(t)
			parent := filepath.Join(root, "pin")
			if os.Mkdir(parent, 0755) != nil {
				t.Fatal("mkdir")
			}
			path := filepath.Join(parent, "owner.json")
			b := []byte(`{"version":1,"owner":"@ivan:fixture.invalid","identities":["alice"]}`)
			if os.WriteFile(path, b, 0440) != nil || os.Chmod(path, 0440) != nil {
				t.Fatal("pin fixture")
			}
			d, _ := fixtureDisk(t, parent)
			target := map[string]string{"parent": parent, "ancestor": root, "file": path}[kind]
			seen := false
			d.aclSafe = func(fd int) bool {
				var st unix.Stat_t
				if unix.Fstat(fd, &st) != nil {
					return false
				}
				if target != "" && st.Ino == inode(t, target) {
					seen = true
					return false
				}
				return true
			}
			_, err := d.policy(path, Digest(b), 401)
			if kind == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrRefused) || !seen {
				t.Fatal("ACL grant not refused", err, seen)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(b, after) {
				t.Fatal("policy changed", err)
			}
		})
	}
}

// W-ACL-DECODE/C-ACL-WRITE: SDK-shaped layout accepts no ACL, read permits
// and deny-delete entries; each mutation right and malformed framing refuses.
// This checks decoding, not a live fgetattrlist or cross-UID permission query.
func TestDarwinACLWriteRights(t *testing.T) {
	makeACL := func(kind, rights uint32) []byte {
		b := make([]byte, 12+44+24)
		o := binary.NativeEndian
		o.PutUint32(b[:4], uint32(len(b)))
		o.PutUint32(b[4:8], 8)
		o.PutUint32(b[8:12], 68)
		o.PutUint32(b[12+36:12+40], 1)
		o.PutUint32(b[12+44+16:12+44+20], kind)
		o.PutUint32(b[12+44+20:], rights)
		return b
	}
	for _, b := range [][]byte{makeACL(1, 1<<1|1<<3), makeACL(2, 1<<4)} {
		if !darwinACLNoWrite(b) {
			t.Fatal("valid ACL refused")
		}
	}
	for _, bit := range []uint{2, 4, 5, 6, 8, 10, 12, 13, 21, 23} {
		if darwinACLNoWrite(makeACL(1, 1<<bit)) {
			t.Fatalf("write bit %d accepted", bit)
		}
	}
	for _, b := range [][]byte{nil, makeACL(3, 0), makeACL(1, 0)[:20]} {
		if darwinACLNoWrite(b) {
			t.Fatal("malformed/unknown ACL accepted")
		}
	}
	b := make([]byte, 12)
	binary.NativeEndian.PutUint32(b[:4], 12)
	if !darwinACLNoWrite(b) {
		t.Fatal("no ACL refused")
	}
}

// W-PREPARE-ERRORS/C-ENOENT: neighbouring setup, publication-race and post-
// publish metadata failures refuse specifically; no foreign inode is adopted.
// A post-publish failure leaves the new untrusted object for explicit diagnosis.
func TestPrepareAbsentFailureNeighbours(t *testing.T) {
	for _, kind := range []string{"valid", "chown", "rename_exists", "published_metadata"} {
		t.Run(kind, func(t *testing.T) {
			root := physicalTemp(t)
			target := filepath.Join(root, "a2a")
			d, meta := fixtureDisk(t, root)
			var raced os.FileInfo
			faultSeen := false
			if kind == "chown" {
				d.chown = func(int, int, int) error { faultSeen = true; return unix.EPERM }
			}
			if kind == "rename_exists" {
				d.rename = func(fd int, from, to string) error {
					if os.Mkdir(target, 0755) != nil {
						t.Fatal("race plant")
					}
					raced, _ = os.Lstat(target)
					err := renameExclusive(fd, from, to)
					if !errors.Is(err, unix.EEXIST) {
						t.Fatal("race control did not produce EEXIST", err)
					}
					faultSeen = true
					return err
				}
			}
			if kind == "published_metadata" {
				d.rename = func(fd int, from, to string) error {
					if e := renameExclusive(fd, from, to); e != nil {
						return e
					}
					meta[inode(t, target)] = ownership{42, 401, 0711}
					faultSeen = true
					return nil
				}
			}
			err := d.prepareFor(target, 401, 401, "darwin")
			if kind == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrRefused) || !faultSeen {
				t.Fatal("failure admitted", err)
			}
			children, e := os.ReadDir(root)
			if e != nil {
				t.Fatal(e)
			}
			if kind == "chown" {
				if len(children) != 0 {
					t.Fatal("failed chown published state")
				}
				return
			}
			if len(children) != 1 || children[0].Name() != "a2a" {
				t.Fatal("temporary sibling leaked")
			}
			if raced != nil {
				after, _ := os.Lstat(target)
				if !os.SameFile(raced, after) || after.Mode().Perm() != 0755 {
					t.Fatal("race object adopted")
				}
			}
		})
	}
}

// W-RUN-TICK/M-TICK: Run consumes an injected tick and advances the real fd's
// mtime to floor(t). C-RUN-ERROR uses a closed owned fd and requires ErrRefused;
// C-NO-TICK verifies an idle loop does not pulse. This does not wait 30 seconds.
func TestHeartbeatRunDrivesPulse(t *testing.T) {
	for _, kind := range []string{"tick", "pulse_failure", "closed_ticks"} {
		t.Run(kind, func(t *testing.T) {
			home := physicalTemp(t)
			if os.Chmod(home, 0700) != nil {
				t.Fatal("mode")
			}
			h, err := OpenHeartbeat(home)
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			initial := time.Unix(1700000000, 123456789)
			if h.Pulse(initial) != nil {
				t.Fatal("initial pulse")
			}
			before, err := os.Stat(filepath.Join(home, "heartbeat"))
			if err != nil {
				t.Fatal(err)
			}
			ticks := make(chan time.Time)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan error, 1)
			if kind == "pulse_failure" {
				if err = h.Close(); err != nil {
					t.Fatal(err)
				}
			}
			go func() { done <- h.Run(ctx, ticks) }()
			if kind == "closed_ticks" {
				close(ticks)
				if err = <-done; err != nil {
					t.Fatal(err)
				}
				idle, err := os.Stat(filepath.Join(home, "heartbeat"))
				if err != nil || !idle.ModTime().Equal(before.ModTime()) {
					t.Fatal("no-tick loop pulsed", err)
				}
				return
			}
			idle, err := os.Stat(filepath.Join(home, "heartbeat"))
			if err != nil || !idle.ModTime().Equal(before.ModTime()) {
				t.Fatal("idle loop pulsed", err)
			}
			next := initial.Add(31*time.Second + 111*time.Nanosecond)
			select {
			case ticks <- next:
			case <-ctx.Done():
				t.Fatal("tick not consumed")
			}
			if kind == "pulse_failure" {
				select {
				case err = <-done:
					if !errors.Is(err, ErrRefused) {
						t.Fatal("Pulse error swallowed", err)
					}
				case <-ctx.Done():
					t.Fatal("Pulse failure did not stop loop")
				}
				return
			}
			poll := time.NewTicker(time.Millisecond)
			defer poll.Stop()
			for {
				info, err := os.Stat(filepath.Join(home, "heartbeat"))
				if err != nil {
					t.Fatal(err)
				}
				if info.ModTime().Equal(next.Truncate(time.Microsecond)) {
					break
				}
				select {
				case <-poll.C:
				case <-ctx.Done():
					t.Fatal("tick did not advance heartbeat to floor(t)")
				}
			}
			cancel()
			if err = <-done; err != nil {
				t.Fatal("cancel failed", err)
			}
		})
	}
}

// W-HB-RETRY/C-HB-RETRY: a short sweep-like contention yields to a later real
// flock; persistent contention exhausts the exact bound, and other errors do
// not retry. The clock/lock seam is private; no waiting or supervisor is needed.
func TestHeartbeatBoundedLockRetry(t *testing.T) {
	for _, kind := range []string{"temporary", "persistent", "other"} {
		t.Run(kind, func(t *testing.T) {
			home := physicalTemp(t)
			if os.Chmod(home, 0700) != nil {
				t.Fatal("mode")
			}
			calls, pauses, fd := 0, 0, -1
			h, err := openHeartbeat(home, func(n int) error {
				fd = n
				calls++
				if kind == "other" {
					return unix.EIO
				}
				if kind == "persistent" || calls <= 2 {
					return unix.EWOULDBLOCK
				}
				return unix.Flock(n, unix.LOCK_EX|unix.LOCK_NB)
			}, func() { pauses++ })
			if kind == "temporary" {
				if err != nil || calls != 3 || pauses != 2 {
					t.Fatal("transient contention", err, calls, pauses)
				}
				h.Close()
				return
			}
			if !errors.Is(err, ErrRefused) || h != nil {
				t.Fatal("lock error admitted", err)
			}
			wantCalls, wantPauses := heartbeatLockAttempts, heartbeatLockAttempts-1
			if kind == "other" {
				wantCalls, wantPauses = 1, 0
			}
			if calls != wantCalls || pauses != wantPauses {
				t.Fatal("wrong retry bound", calls, pauses)
			}
			var st unix.Stat_t
			if unix.Fstat(fd, &st) != unix.EBADF {
				t.Fatal("failed heartbeat leaked fd")
			}
		})
	}
}

type recheckSupervisor struct {
	fakeSupervisor
	keepRunning      bool
	starting         bool
	transient        map[string]int
	unknownPostStart bool
}

func (s *recheckSupervisor) Stop(ctx context.Context, p Plan, n string) error {
	if s.keepRunning {
		s.Record("stop", n)
		return nil
	}
	return s.fakeSupervisor.Stop(ctx, p, n)
}
func (s *recheckSupervisor) Start(ctx context.Context, p Plan, n string) error {
	s.starting = true
	return s.fakeSupervisor.Start(ctx, p, n)
}
func (s *recheckSupervisor) State(ctx context.Context, p Plan, n string) (State, error) {
	if s.starting && s.unknownPostStart {
		return Unknown, nil
	}
	if s.starting && s.transient[n] > 0 {
		s.transient[n]--
		return Activating, nil
	}
	return s.fakeSupervisor.State(ctx, p, n)
}

// W-INSTALL/C-INSTALL-BYTES/C-POST-STOP: partial sets and changed bytes (the
// same secret/socket plants used by ValidateUnits) refuse before start/reload;
// a lying Stop followed by Running preserves every file. A complete stopped
// install and joined uninstall are valid neighbours. All supervisors are fake.
func TestServiceInstallAndRecheckRefusals(t *testing.T) {
	for _, kind := range []string{"install_valid", "install_partial", "install_secret", "install_socket", "uninstall_joined", "uninstall_running"} {
		t.Run(kind, func(t *testing.T) {
			root := physicalTemp(t)
			p := testPlan()
			d, _ := fixtureDisk(t, root)
			files, _ := Render(p)
			names := make([]string, 0, len(files))
			for n := range files {
				names = append(names, n)
			}
			sort.Strings(names)
			s := &recheckSupervisor{fakeSupervisor: fakeSupervisor{states: map[string]State{}}, keepRunning: kind == "uninstall_running"}
			before := map[string][]byte{}
			for i, n := range names {
				if kind == "install_partial" && i > 0 {
					continue
				}
				b := append([]byte(nil), files[n]...)
				if i == 0 && kind == "install_secret" {
					b = append(b, []byte("Environment=TOKEN=control")...)
				}
				if i == 0 && kind == "install_socket" {
					b = append(b, []byte("ListenStream=/extra.sock")...)
				}
				if os.WriteFile(filepath.Join(root, n), b, 0644) != nil || os.Chmod(filepath.Join(root, n), 0644) != nil {
					t.Fatal("write")
				}
				before[n] = b
				s.states[n] = Stopped
				if kind == "uninstall_running" || kind == "uninstall_joined" {
					s.states[n] = Running
				}
			}
			m := &Manager{Supervisor: s, d: d, dir: root}
			op := "install"
			if kind == "uninstall_running" || kind == "uninstall_joined" {
				op = "uninstall"
			}
			_, err := m.Run(context.Background(), p, op)
			if kind == "install_valid" || kind == "uninstall_joined" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrUnresolved) {
				t.Fatal("unsafe transaction admitted", err)
			}
			if op == "install" {
				tuitestkit.AssertNotCalled(t, &s.MockCallRecorder, "start")
				tuitestkit.AssertNotCalled(t, &s.MockCallRecorder, "reload")
				tuitestkit.AssertNotCalled(t, &s.MockCallRecorder, "stop")
			}
			for n, b := range before {
				after, e := os.ReadFile(filepath.Join(root, n))
				if e != nil || !bytes.Equal(b, after) {
					t.Fatal("refusal changed files", e)
				}
			}
			children, e := os.ReadDir(root)
			if e != nil || len(children) != len(before) {
				t.Fatal("partial set reconstructed", e)
			}
			for _, state := range s.states {
				if op == "install" && state != Stopped {
					t.Fatal("refusal started unit")
				}
			}
		})
	}
}

// W-ACTIVATING/C-UNKNOWN: exact matching activating main/worker units are
// polled to running; unknown states still refuse. Injected polls avoid sleeps.
func TestServiceReadinessToleratesActivating(t *testing.T) {
	root := physicalTemp(t)
	p := testPlan()
	p.Platform = "linux"
	d, _ := fixtureDisk(t, root)
	s := &recheckSupervisor{fakeSupervisor: fakeSupervisor{states: map[string]State{}}, transient: map[string]int{Label + ".service": 1, Label + ".sweep.service": 1, Label + ".sweep.timer": 1}}
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	m := &Manager{Supervisor: s, d: d, dir: root, poll: ticks}
	states, err := m.Run(context.Background(), p, "install")
	if err != nil || states[Label+".service"] != Running || states[Label+".sweep.timer"] != Running {
		t.Fatal("activating rejected", err, states)
	}
	s.states[Label+".service"] = Unknown
	if _, err = m.Run(context.Background(), p, "install"); !errors.Is(err, ErrUnresolved) {
		t.Fatal("unknown became activating", err)
	}
	other := physicalTemp(t)
	otherDisk, _ := fixtureDisk(t, other)
	bad := &recheckSupervisor{fakeSupervisor: fakeSupervisor{states: map[string]State{}}, unknownPostStart: true}
	badManager := &Manager{Supervisor: bad, d: otherDisk, dir: other, poll: ticks}
	if _, err = badManager.Run(context.Background(), p, "install"); !errors.Is(err, ErrUnresolved) {
		t.Fatal("post-start unknown tolerated", err)
	}
	files, _ := Render(p)
	for n, want := range files {
		got, e := os.ReadFile(filepath.Join(other, n))
		if e != nil || !bytes.Equal(got, want) {
			t.Fatal("unresolved start removed files", e)
		}
	}
}

// W-ACTIVATING-PARSER/C-ACTIVATING-FOREIGN: only a matching loaded unit with
// a known activating substate gets the transient token; foreign metadata or
// unknown substates remain unresolved. No systemctl is executed.
func TestLinuxActivatingStateParser(t *testing.T) {
	p := testPlan()
	p.Platform = "linux"
	name := Label + ".sweep.service"
	output := "LoadState=loaded\nActiveState=activating\nSubState=start\nFragmentPath=" + filepath.Join(p.UnitDir(), name) + "\nNeedDaemonReload=no\nUser=a2a\nGroup=a2a\nExecStart={ path=" + p.Image + " ; argv[]=" + strings.Join(p.args("sweep"), " ") + " ; }\n"
	for _, tc := range []struct {
		name, out string
		want      State
	}{
		{"matching", output, Activating},
		{"foreign", strings.Replace(output, "User=a2a", "User=root", 1), Unknown},
		{"unknown_substate", strings.Replace(output, "SubState=start", "SubState=unexpected", 1), Unknown},
		{"stopping", strings.Replace(output, "ActiveState=activating", "ActiveState=deactivating", 1), Unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := nativeSupervisor{run: func(context.Context, string, ...string) commandResult { return commandResult{[]byte(tc.out), 0} }}
			state, err := s.State(context.Background(), p, name)
			if state != tc.want || tc.want == Activating && err != nil || tc.want == Unknown && !errors.Is(err, ErrUnresolved) {
				t.Fatal("transient classification", state, err)
			}
		})
	}
}

// W-DIR-SYNC/C-DIR-SYNC: real fixture directory fsync follows every unit create
// and remove, and the first rescue-log create only. An injected sync failure
// is reported as unresolved/refused rather than durable success.
func TestDirectorySyncBoundaries(t *testing.T) {
	for _, kind := range []string{"units", "unit_sync_failure", "rescue", "rescue_sync_failure"} {
		t.Run(kind, func(t *testing.T) {
			root := physicalTemp(t)
			if os.Chmod(root, 0700) != nil {
				t.Fatal("mode")
			}
			d, _ := fixtureDisk(t, root)
			syncs := 0
			d.syncDir = func(f *os.File) error {
				syncs++
				if kind == "units" {
					children, err := os.ReadDir(root)
					want := syncs
					if syncs > 3 {
						want = 6 - syncs
					}
					if err != nil || len(children) != want {
						t.Fatal("directory sync preceded mutation", err, len(children), want)
					}
				}
				if kind == "rescue" {
					b, err := os.ReadFile(filepath.Join(root, "alice.rescue.jsonl"))
					if err != nil || len(b) == 0 {
						t.Fatal("directory sync preceded first record", err)
					}
				}
				if kind == "unit_sync_failure" || kind == "rescue_sync_failure" {
					return unix.EIO
				}
				return f.Sync()
			}
			if kind == "rescue" || kind == "rescue_sync_failure" {
				p := Policy{Identities: []string{"alice"}}
				r := RescueRecord{At: time.Now(), Identity: "alice", Layer: "traffic", Count: 1}
				err := recordRescue(root, p, r, d)
				if kind == "rescue_sync_failure" {
					if !errors.Is(err, ErrRefused) || syncs != 1 {
						t.Fatal("sync failure hidden", err, syncs)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if err = recordRescue(root, p, r, d); err != nil || syncs != 1 {
					t.Fatal("first-create sync missing/repeated", err, syncs)
				}
				return
			}
			p := testPlan()
			s := &fakeSupervisor{states: map[string]State{}}
			m := &Manager{Supervisor: s, d: d, dir: root}
			_, err := m.Run(context.Background(), p, "install")
			if kind == "unit_sync_failure" {
				if !errors.Is(err, ErrUnresolved) || syncs != 1 {
					t.Fatal("sync failure hidden", err, syncs)
				}
				return
			}
			if err != nil || syncs != 3 {
				t.Fatal("create sync", err, syncs)
			}
			if _, err = m.Run(context.Background(), p, "uninstall"); err != nil || syncs != 6 {
				t.Fatal("remove sync", err, syncs)
			}
		})
	}
}
