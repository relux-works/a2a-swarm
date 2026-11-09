//go:build linux || darwin

package courierenroll

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Fixtures are generated only in memory. Assertions never print the payload,
// raw key, encoded key or a diff containing those values.
func fixture(identity string, fill byte) string {
	b, _ := json.Marshal(map[string]string{"socket": "/run/a2a/courier.sock", "identity": identity, "station": "station-1", "secret_b64": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))})
	return string(b) + "\n"
}

func canonicalHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return home
}

func target(t *testing.T) string { t.Helper(); return filepath.Join(canonicalHome(t), ".a2a") }

func checkGeneration(t *testing.T, dir, identity string, fill byte) {
	t.Helper()
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal("generation directory unavailable")
	}
	defer f.Close()
	want := [][]byte{[]byte("/run/a2a/courier.sock\n"), []byte(identity + "\n"), bytes.Repeat([]byte{fill}, 32), []byte("station-1\n")}
	if verify(f, want) != nil {
		t.Fatal("generation bytes, entries, ownership or modes differ")
	}
}

// Proves atomic four-file publication removes the old namespace while a pinned key stays readable;
// this does not simulate power loss.
func TestFreshRotateAndPinnedReader(t *testing.T) {
	dir := target(t)
	if err := Install(dir, strings.NewReader(fixture("first", 1)), false); err != nil {
		t.Fatal("fresh enrollment failed")
	}
	checkGeneration(t, dir, "first", 1)
	old, err := os.Open(filepath.Join(dir, "station.key"))
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if Install(dir, strings.NewReader(fixture("second", 2)), false) == nil {
		t.Fatal("existing generation accepted without rotate")
	}
	checkGeneration(t, dir, "first", 1)
	if err := Install(dir, strings.NewReader(fixture("second", 2)), true); err != nil {
		t.Fatal("rotation failed")
	}
	checkGeneration(t, dir, "second", 2)
	data, err := io.ReadAll(old)
	if err != nil || !bytes.Equal(data, bytes.Repeat([]byte{1}, 32)) {
		t.Fatal("pinned key descriptor changed")
	}
	parentEntries, err := os.ReadDir(filepath.Dir(dir))
	if err != nil || len(parentEntries) != 1 {
		t.Fatal("old key generation retained")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 4 {
		t.Fatal("temporary file admitted to enrollment namespace")
	}
}

// Proves malformed enrollment payloads refuse beside a valid payload without
// publishing a generation; no live registration is involved.
func TestInputRefusals(t *testing.T) {
	valid := fixture("valid", 3)
	var fields map[string]any
	if json.Unmarshal([]byte(valid), &fields) != nil {
		t.Fatal("fixture setup")
	}
	cases := map[string]string{
		"bad-json": "{\n", "missing-lf": strings.TrimSuffix(valid, "\n"),
		"two-lines": valid + valid, "trailing-json": strings.TrimSuffix(valid, "\n") + "{}\n",
		"duplicate":        strings.Replace(valid, "{", `{"identity":"other",`, 1),
		"unknown":          strings.Replace(valid, "{", `{"extra":"x",`, 1),
		"case":             strings.Replace(valid, `"socket"`, `"Socket"`, 1),
		"null":             strings.Replace(valid, `"identity":"valid"`, `"identity":null`, 1),
		"oversize":         strings.Repeat(" ", maxInput) + valid,
		"physical-newline": strings.Replace(valid, "{", "{\n", 1),
		"invalid-utf8":     "{\xff}\n",
	}
	for _, field := range []string{"socket", "identity", "station", "secret_b64"} {
		for _, val := range []any{nil, 1, true, []string{}, map[string]string{}, ""} {
			copyFields := make(map[string]any)
			for k, v := range fields {
				copyFields[k] = v
			}
			copyFields[field] = val
			b, _ := json.Marshal(copyFields)
			cases[fmt.Sprintf("type-%s-%T", field, val)] = string(b) + "\n"
		}
		copyFields := make(map[string]any)
		for k, v := range fields {
			if k != field {
				copyFields[k] = v
			}
		}
		b, _ := json.Marshal(copyFields)
		cases["missing-"+field] = string(b) + "\n"
	}
	badValues := map[string][]string{
		"identity":   {"UPPER", "x\n", "x\ny", "a b", "a/b", "é", strings.Repeat("a", 65)},
		"socket":     {"relative", "/run/x\n", "/run/\x00", "/run/x\r", "/run/\uFEFF", "/" + strings.Repeat("x", 103), "/" + strings.Repeat("é", 52)},
		"station":    {" ", "a\nb", "a\x00b", "é", strings.Repeat("s", 65), "a/b", "UPPER"},
		"secret_b64": {"%", base64.StdEncoding.EncodeToString(make([]byte, 31)), base64.StdEncoding.EncodeToString(make([]byte, 33)), strings.TrimRight(fields["secret_b64"].(string), "="), fields["secret_b64"].(string) + "\n"},
	}
	for field, values := range badValues {
		for i, v := range values {
			copyFields := make(map[string]any)
			for k, value := range fields {
				copyFields[k] = value
			}
			copyFields[field] = v
			b, _ := json.Marshal(copyFields)
			cases[fmt.Sprintf("value-%s-%d", field, i)] = string(b) + "\n"
		}
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			dir := target(t)
			err := Install(dir, strings.NewReader(input), false)
			if err != errInput {
				t.Fatal("malformed input did not return fixed input refusal")
			}
			if _, err := os.Lstat(dir); !os.IsNotExist(err) {
				t.Fatal("invalid input created enrollment")
			}
		})
	}
	if _, err := readInput(strings.NewReader(valid)); err != nil {
		t.Fatal("valid neighbor refused")
	}
}

// Proves unsafe filesystem forms refuse rotation; valid installations are
// established before each mutation. ACL isolation is not qualified.
func TestUnsafePathsAndEntries(t *testing.T) {
	cases := []string{"dir-symlink", "unsafe-parent", "unsafe-ancestor", "dir-mode", "extra", "partial", "file-symlink", "fifo", "directory-file", "hardlink", "file-mode", "special-mode", "traversal", "relative", "wrong-name"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			home := canonicalHome(t)
			dir := filepath.Join(home, ".a2a")
			if Install(dir, strings.NewReader(fixture("original", 4)), false) != nil {
				t.Fatal("setup enrollment failed")
			}
			switch name {
			case "dir-symlink":
				if err := os.Rename(dir, filepath.Join(home, "old")); err != nil {
					t.Fatal("mutation failed", err)
				}
				if err := os.Symlink("old", dir); err != nil {
					t.Fatal("mutation failed", err)
				}
			case "unsafe-parent":
				if err := os.Chmod(home, 0777); err != nil {
					t.Fatal("mutation failed", err)
				}
				defer os.Chmod(home, 0700)
			case "unsafe-ancestor":
				child := filepath.Join(home, "child")
				if err := os.Mkdir(child, 0700); err != nil {
					t.Fatal("mutation failed", err)
				}
				dir = filepath.Join(child, ".a2a")
				if err := os.Chmod(home, 0777); err != nil {
					t.Fatal("mutation failed", err)
				}
				defer os.Chmod(home, 0700)
			case "dir-mode":
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal("mutation failed", err)
				}
			case "extra":
				if err := os.WriteFile(filepath.Join(dir, "extra"), nil, 0600); err != nil {
					t.Fatal("mutation failed", err)
				}
			case "partial":
				if err := os.Remove(filepath.Join(dir, "identity")); err != nil {
					t.Fatal("mutation failed", err)
				}
			case "file-symlink":
				if err := os.Remove(filepath.Join(dir, "identity")); err != nil {
					t.Fatal("mutation failed", err)
				}
				if err := os.Symlink("courier.sock", filepath.Join(dir, "identity")); err != nil {
					t.Fatal("mutation failed", err)
				}
			case "fifo":
				if err := os.Remove(filepath.Join(dir, "identity")); err != nil {
					t.Fatal("mutation failed", err)
				}
				if err := unix.Mkfifo(filepath.Join(dir, "identity"), 0600); err != nil {
					t.Fatal("mutation failed", err)
				}
			case "directory-file":
				if err := os.Remove(filepath.Join(dir, "identity")); err != nil {
					t.Fatal("mutation failed", err)
				}
				if err := os.Mkdir(filepath.Join(dir, "identity"), 0600); err != nil {
					t.Fatal("mutation failed", err)
				}
			case "hardlink":
				if err := os.Link(filepath.Join(dir, "station.key"), filepath.Join(home, "link")); err != nil {
					t.Fatal("mutation failed", err)
				}
			case "file-mode":
				if err := os.Chmod(filepath.Join(dir, "station.key"), 0644); err != nil {
					t.Fatal("mutation failed", err)
				}
			case "special-mode":
				if err := unix.Chmod(filepath.Join(dir, "station.key"), 04600); err != nil {
					t.Fatal("mutation failed", err)
				}
			case "traversal":
				dir = home + "/x/../.a2a"
			case "relative":
				dir = "relative/.a2a"
			case "wrong-name":
				dir = filepath.Join(home, "other")
			}
			if Install(dir, strings.NewReader(fixture("new", 5)), true) == nil {
				t.Fatal("unsafe path accepted")
			}
		})
	}
}

// Proves owner metadata predicates admit owned controls and refuse foreign
// owners without privileged chown; no second kernel UID is exercised.
func TestOwnershipPredicate(t *testing.T) {
	st := unix.Stat_t{Uid: uint32(os.Geteuid()), Mode: unix.S_IFREG | 0600, Nlink: 1}
	if !safeStat(&st, unix.S_IFREG, 0600) {
		t.Fatal("owned control refused")
	}
	st.Uid++
	if safeStat(&st, unix.S_IFREG, 0600) {
		t.Fatal("foreign owner accepted")
	}
	st.Mode = unix.S_IFDIR | 0700
	if safeAncestorStat(&st, true) || safeAncestorStat(&st, false) {
		t.Fatal("foreign parent owner accepted")
	}
	st.Uid = 0
	if os.Geteuid() != 0 && safeAncestorStat(&st, true) {
		t.Fatal("foreign final parent accepted")
	}
	if !safeAncestorStat(&st, false) {
		t.Fatal("root-owned ancestor control refused")
	}
	// No chown, privilege changes or foreign-owner chmod repair is needed.
}

// Proves injected failures at every four-file write/publication phase retain
// the old generation or absence; this is an ordinary-error rollback witness.
func TestWriteFailureAndRollback(t *testing.T) {
	for _, phase := range []string{"wrote-courier.sock", "wrote-identity", "wrote-station.key", "wrote-station", "staged", "published"} {
		for _, rotate := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", phase, rotate), func(t *testing.T) {
				dir := target(t)
				if rotate && Install(dir, strings.NewReader(fixture("old", 6)), false) != nil {
					t.Fatal("setup")
				}
				err := install(dir, strings.NewReader(fixture("new", 7)), rotate, func(p string, _ *os.File) error {
					if p == phase {
						return errors.New("injected")
					}
					return nil
				})
				if err == nil {
					t.Fatal("injected failure reported success")
				}
				if rotate {
					checkGeneration(t, dir, "old", 6)
				} else if _, err := os.Lstat(dir); !os.IsNotExist(err) {
					t.Fatal("failed first enrollment was published")
				}
			})
		}
	}
}

// Proves staged mode and namespace changes cannot redirect publication,
// using deterministic mutation points rather than an adversarial scheduler.
func TestWrongModeAfterWriteAndPathRaces(t *testing.T) {
	for _, attack := range []string{"mode", "extra", "parent-renamed", "target-replaced", "staging-replaced"} {
		t.Run(attack, func(t *testing.T) {
			dir := target(t)
			home := filepath.Dir(dir)
			if Install(dir, strings.NewReader(fixture("old", 8)), false) != nil {
				t.Fatal("setup")
			}
			err := install(dir, strings.NewReader(fixture("new", 9)), true, func(p string, stage *os.File) error {
				if p != "staged" {
					return nil
				}
				switch attack {
				case "mode":
					return unix.Fchmodat(int(stage.Fd()), "station.key", 0644, 0)
				case "extra":
					fd, err := unix.Openat(int(stage.Fd()), "extra", unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY, 0600)
					if err == nil {
						unix.Close(fd)
					}
					return err
				case "parent-renamed":
					if os.Rename(home, home+"-moved") != nil {
						t.Fatal("rename setup")
					}
					t.Cleanup(func() { os.Remove(home); os.Rename(home+"-moved", home) })
					return os.Mkdir(home, 0700)
				case "target-replaced":
					if os.Rename(dir, dir+"-old") != nil {
						t.Fatal("rename setup")
					}
					return os.Symlink(dir+"-old", dir)
				case "staging-replaced":
					stagePath := filepath.Join(home, stage.Name())
					if os.Rename(stagePath, stagePath+"-moved") != nil {
						t.Fatal("rename setup")
					}
					return os.Symlink(stagePath+"-moved", stagePath)
				}
				return nil
			})
			if err == nil {
				t.Fatal("post-write mutation accepted")
			}
			if attack == "mode" || attack == "extra" || attack == "staging-replaced" {
				checkGeneration(t, dir, "old", 8)
			}
		})
	}
}

// Proves a second writer refuses the held parent lock while the admitted
// writer completes a valid generation; this does not model multiple UIDs.
func TestConcurrentWriters(t *testing.T) {
	dir := target(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- install(dir, strings.NewReader(fixture("one", 10)), false, func(p string, _ *os.File) error {
			if p == "staged" {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("first writer exited before staging: %v", err)
	}
	err := Install(dir, strings.NewReader(fixture("two", 11)), true)
	close(release)
	if err == nil {
		t.Fatal("concurrent writer ignored parent lock")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("first writer failed")
		}
	case <-ctx.Done():
		t.Fatal("first writer did not finish")
	}
	checkGeneration(t, dir, "one", 10)
}

// Proves namespace readers either return a complete generation or explicitly refuse during rotation;
// whole-machine power-loss durability is outside the test.
func TestReaderDuringRotations(t *testing.T) {
	dir := target(t)
	if Install(dir, strings.NewReader(fixture("same", 12)), false) != nil {
		t.Fatal("setup")
	}
	var wg sync.WaitGroup
	failures := make(chan bool, 1)
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			generation, present, err := Read(dir)
			// A rotation can invalidate a reader's namespace pin. It refuses
			// explicitly; it never returns a partial key as a success.
			if !present {
				select {
				case failures <- true:
				default:
				}
				return
			}
			if err != nil {
				continue
			}
			valid := generation.Identity == "same" && generation.Station == "station-1" && generation.Socket == "/run/a2a/courier.sock" && len(generation.Key) == 32
			if valid {
				fill := generation.Key[0]
				valid = fill >= 12 && fill <= 24 && bytes.Equal(generation.Key, bytes.Repeat([]byte{fill}, 32))
			}
			clear(generation.Key)
			if !valid {
				select {
				case failures <- true:
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 12; i++ {
		if Install(dir, strings.NewReader(fixture("same", byte(i+13))), true) != nil {
			t.Error("rotation failed")
			break
		}
	}
	close(stop)
	wg.Wait()
	checkGeneration(t, dir, "same", 24)
	select {
	case <-failures:
		t.Fatal("pinned reader saw incomplete generation")
	default:
	}
}

// Proves process exit before/after atomic publication leaves a complete
// old/new generation and permits explicit retry; it does not emulate power loss.
func TestCrashBoundary(t *testing.T) {
	if phase := os.Getenv("ENROLL_TEST_CRASH_PHASE"); phase != "" {
		_ = install(os.Getenv("ENROLL_TEST_DIR"), strings.NewReader(fixture("new", 21)), true, func(p string, _ *os.File) error {
			if p == phase {
				os.Exit(73)
			}
			return nil
		})
		os.Exit(74)
	}
	for _, phase := range []string{"wrote-identity", "staged", "published"} {
		t.Run(phase, func(t *testing.T) {
			dir := target(t)
			if Install(dir, strings.NewReader(fixture("old", 20)), false) != nil {
				t.Fatal("setup")
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestCrashBoundary$")
			cmd.Env = append(os.Environ(), "ENROLL_TEST_CRASH_PHASE="+phase, "ENROLL_TEST_DIR="+dir)
			err := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 73 {
				t.Fatal("crash child did not reach injection")
			}
			if phase == "published" {
				checkGeneration(t, dir, "new", 21)
			} else {
				checkGeneration(t, dir, "old", 20)
			}
			// Orphans remain outside .a2a and do not prevent an explicit retry.
			if Install(dir, strings.NewReader(fixture("recovered", 22)), true) != nil {
				t.Fatal("explicit retry refused after interruption")
			}
			checkGeneration(t, dir, "recovered", 22)
		})
	}
}
