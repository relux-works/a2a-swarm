//go:build darwin || linux

package devicelist

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func snapshotFixture(t testing.TB) (snapshotProfile, SnapshotConfig, []byte) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "chain-snapshot")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	chmod(t, root, 0755)
	chmod(t, dir, 0755)
	list := mustParse(t, []byte(exampleList))
	data := snapshotData(nameValue(pinOf(list)), list)
	config := configOf(data)
	config.Path = filepath.Join(dir, "snapshot.json")
	return snapshotProfile{root: root, uid: uint32(os.Getuid())}, config, encodeSnapshotFile(data)
}
func chmod(t testing.TB, path string, mode uint32) {
	t.Helper()
	if err := unix.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
func writeSnapshot(t testing.TB, path string, raw []byte, mode uint32) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	chmod(t, path, mode)
}
func skipAsRoot(t testing.TB) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("filesystem fixture requires an unprivileged process")
	}
}
func checkUnownedSnapshotRefused(profile snapshotProfile, config SnapshotConfig) error {
	if view, label, err := readSnapshot(profile, config, at); err != ErrSnapshotUnsafe || view != nil || label != LabelInvalid {
		return fmt.Errorf("checkUnownedSnapshotRefused: ownership refusal missing: %q %v", label, err)
	}
	return nil
}

// Claim: the descriptor reader rejects listed unsafe objects and malformed files beside restored valid reads.
func TestReadSnapshotSafety(t *testing.T) {
	skipAsRoot(t)
	profile, config, valid := snapshotFixture(t)
	path, dir := config.Path, filepath.Dir(config.Path)
	read := func() (*Snapshot, Freshness, error) { return readSnapshot(profile, config, at) }
	wantValid := func(t *testing.T) {
		t.Helper()
		v, label, err := read()
		if err != nil || v == nil || label != LabelUnknown {
			t.Fatalf("control: %q %v", label, err)
		}
		s := newScenario(t)
		s.chain = v
		wantObservedInstruct(t, s)
	}
	if v, label, err := read(); err != ErrSnapshotUnavailable || v != nil || label != LabelUnknown {
		t.Fatalf("absent: %q %v", label, err)
	}
	writeSnapshot(t, path, valid, 0644)
	wantValid(t)
	for _, mode := range []uint32{0666, 0664, 0600, 0640, 0755, 0444, 04644} {
		t.Run(fmt.Sprintf("file-mode-%04o", mode), func(t *testing.T) {
			wantValid(t)
			defer wantValid(t)
			defer chmod(t, path, 0644)
			chmod(t, path, mode)
			if _, _, err := read(); err != ErrSnapshotUnsafe {
				t.Fatal(err)
			}
		})
	}
	for _, target := range []string{dir, profile.root} {
		for _, mode := range []uint32{0711, 0700, 0750, 0555, 0775, 0777, 01755} {
			t.Run(fmt.Sprintf("directory-%s-%04o", filepath.Base(target), mode), func(t *testing.T) {
				wantValid(t)
				defer wantValid(t)
				defer chmod(t, target, 0755)
				chmod(t, target, mode)
				v, _, err := read()
				accepted := target == profile.root && mode&0022 == 0
				if accepted {
					if err != nil || v == nil {
						t.Fatal("safe ancestor:", err)
					}
				} else if err != ErrSnapshotUnsafe {
					t.Fatal("unsafe directory:", err)
				}
			})
		}
	}
	for _, kind := range []string{"hard-link", "symlinked-file", "fifo", "directory", "trailing-LF", "overflow"} {
		t.Run(kind, func(t *testing.T) {
			wantValid(t)
			defer wantValid(t)
			switch kind {
			case "hard-link":
				link := filepath.Join(profile.root, "link")
				if err := os.Link(path, link); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(link)
			case "symlinked-file", "fifo", "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				defer writeSnapshot(t, path, valid, 0644)
				defer os.Remove(path)
				switch kind {
				case "symlinked-file":
					target := filepath.Join(profile.root, "other")
					writeSnapshot(t, target, valid, 0644)
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				case "fifo":
					if err := unix.Mkfifo(path, 0644); err != nil {
						t.Fatal(err)
					}
					chmod(t, path, 0644)
				case "directory":
					if err := os.Mkdir(path, 0755); err != nil {
						t.Fatal(err)
					}
				}
			case "trailing-LF":
				writeSnapshot(t, path, append(append([]byte(nil), valid...), '\n'), 0644)
				defer writeSnapshot(t, path, valid, 0644)
			case "overflow":
				writeSnapshot(t, path, append(maxSnapshotPayload(t), ' '), 0644)
				defer writeSnapshot(t, path, valid, 0644)
			}
			done := make(chan error, 1)
			go func() { _, _, err := read(); done <- err }()
			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("read blocked")
			}
			want := ErrSnapshotUnsafe
			if kind == "trailing-LF" {
				want = ErrSnapshotInvalid
			}
			if kind == "overflow" {
				want = ErrCapacity
			}
			if err != want {
				t.Fatalf("want %v, got %v", want, err)
			}
		})
	}
	t.Run("path boundaries", func(t *testing.T) {
		wantValid(t)
		defer wantValid(t)
		link := filepath.Join(profile.root, "linked")
		if err := os.Symlink(dir, link); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			path string
			err  error
		}{{filepath.Join(link, "snapshot.json"), ErrSnapshotUnsafe}, {filepath.Join(path, "child"), ErrSnapshotUnsafe}, {"/outside/snapshot.json", ErrSnapshotUnsafe}, {filepath.Join(profile.root, "missing", "snapshot.json"), ErrSnapshotUnavailable}, {dir + "/../chain-snapshot/snapshot.json", ErrInvalid}} {
			other := config
			other.Path = c.path
			v, label, err := readSnapshot(profile, other, at)
			wantLabel := LabelInvalid
			if c.err == ErrSnapshotUnavailable {
				wantLabel = LabelUnknown
			}
			if err != c.err || v != nil || label != wantLabel {
				t.Fatalf("%q %v", label, err)
			}
		}
	})
	t.Run("ownership components", func(t *testing.T) {
		wantValid(t)
		defer wantValid(t)
		original := snapshotOwnerCheck
		t.Cleanup(func() { snapshotOwnerCheck = original })
		for _, target := range []string{profile.root, dir, path} {
			snapshotOwnerCheck = func(st *unix.Stat_t, uid uint32) bool {
				var targetStat unix.Stat_t
				if err := unix.Stat(target, &targetStat); err != nil {
					t.Fatal(err)
				}
				return st.Ino != targetStat.Ino && original(st, uid)
			}
			if _, _, err := read(); err != ErrSnapshotUnsafe {
				t.Fatalf("component ownership not checked: %v", err)
			}
			snapshotOwnerCheck = original
			wantValid(t)
		}
	})
	t.Run("bindings and labels", func(t *testing.T) {
		wantValid(t)
		defer wantValid(t)
		foreign := config
		foreign.Owner = "id/example"
		foreign.Names = []string{"id/example"}
		if _, label, err := readSnapshot(profile, foreign, at); err != ErrSnapshotInvalid || label != LabelInvalid {
			t.Fatal(label, err)
		}
		if v, label, err := readSnapshot(profile, config, Check{Now: now.Add(time.Minute), ClockTrusted: true}); err != nil || v == nil || label != LabelStale {
			t.Fatal(label, err)
		}
		if v, label, err := readSnapshot(profile, config, Check{Now: time.Unix(int64(writtenAt)-1, 0), ClockTrusted: true}); err != ErrSnapshotInvalid || v != nil || label != LabelInvalid {
			t.Fatal(label, err)
		}
	})
}

// Claim: the exported reader applies the root-owned host profile, while the unprivileged fixture profile accepts the same file.
func TestReadSnapshotHostProfile(t *testing.T) {
	skipAsRoot(t)
	profile, config, raw := snapshotFixture(t)
	writeSnapshot(t, config.Path, raw, 0644)
	if _, _, err := readSnapshot(profile, config, at); err != nil {
		t.Fatal("control:", err)
	}
	if v, label, err := ReadSnapshot(config, at); err != ErrSnapshotUnsafe || v != nil || label != LabelInvalid {
		t.Fatalf("host profile: %q %v", label, err)
	}
}

// Claim: effective UID zero refuses unprivileged fixture bytes, with valid controls before and after the UID seam.
func TestReadSnapshotRootAgent(t *testing.T) {
	skipAsRoot(t)
	profile, config, raw := snapshotFixture(t)
	writeSnapshot(t, config.Path, raw, 0644)
	control := func() {
		t.Helper()
		if _, _, err := readSnapshot(profile, config, at); err != nil {
			t.Fatal(err)
		}
	}
	control()
	original := effectiveUID
	t.Cleanup(func() { effectiveUID = original })
	effectiveUID = func() int { return 0 }
	if v, label, err := readSnapshot(profile, config, at); err != ErrSnapshotUnsafe || v != nil || label != LabelInvalid {
		t.Fatal(label, err)
	}
	effectiveUID = original
	control()
}

// Claim: bypassing ownership kills the named ownership assertion, with controls before and after restoration.
func TestSnapshotOwnershipGatePlantKilled(t *testing.T) {
	skipAsRoot(t)
	profile, config, raw := snapshotFixture(t)
	writeSnapshot(t, config.Path, raw, 0644)
	rootOwned := snapshotProfile{root: profile.root, uid: 0}
	if err := checkUnownedSnapshotRefused(rootOwned, config); err != nil {
		t.Fatal(err)
	}
	original := snapshotOwnerCheck
	t.Cleanup(func() { snapshotOwnerCheck = original })
	snapshotOwnerCheck = func(*unix.Stat_t, uint32) bool { return true }
	if err := checkUnownedSnapshotRefused(rootOwned, config); err == nil {
		t.Fatal("plant survived")
	}
	snapshotOwnerCheck = original
	if err := checkUnownedSnapshotRefused(rootOwned, config); err != nil {
		t.Fatal(err)
	}
}

// Claim: an unlinked opened snapshot reopens once, accepts its replacement, and refuses a second lost inode.
func TestSnapshotRenameReopensOnce(t *testing.T) {
	skipAsRoot(t)
	for _, replacements := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(replacements), func(t *testing.T) {
			profile, config, raw := snapshotFixture(t)
			writeSnapshot(t, config.Path, raw, 0644)
			original := snapshotOpenat
			t.Cleanup(func() { snapshotOpenat = original })
			calls := 0
			snapshotOpenat = func(dirfd int, path string, flags int, mode uint32) (int, error) {
				fd, err := original(dirfd, path, flags, mode)
				if err == nil && path == "snapshot.json" {
					calls++
					if calls <= replacements {
						temp := config.Path + ".replacement"
						writeSnapshot(t, temp, raw, 0644)
						if err := os.Rename(temp, config.Path); err != nil {
							t.Fatal(err)
						}
					}
				}
				return fd, err
			}
			view, _, err := readSnapshot(profile, config, at)
			if replacements < 2 {
				if err != nil || view == nil || calls != replacements+1 {
					t.Fatalf("calls %d, %v", calls, err)
				}
			} else if err != ErrSnapshotUnsafe || view != nil || calls != 2 {
				t.Fatalf("calls %d, %v", calls, err)
			}
			snapshotOpenat = original
			if _, _, err := readSnapshot(profile, config, at); err != nil {
				t.Fatal("control after:", err)
			}
		})
	}
}

// Claim: the descriptor reader accepts a canonical 65536-byte file and rejects its 65537-byte extension.
func TestReadSnapshotExactSize(t *testing.T) {
	skipAsRoot(t)
	profile, config, _ := snapshotFixture(t)
	raw := maxSnapshotPayload(t)
	data, err := parseSnapshotFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := config.Path
	config = configOf(data)
	config.Path = path
	writeSnapshot(t, path, raw, 0644)
	if v, _, err := readSnapshot(profile, config, at); err != nil || v == nil {
		t.Fatal("control:", err)
	}
	writeSnapshot(t, path, append(raw, ' '), 0644)
	if _, _, err := readSnapshot(profile, config, at); err != ErrCapacity {
		t.Fatal(err)
	}
	writeSnapshot(t, path, raw, 0644)
	if _, _, err := readSnapshot(profile, config, at); err != nil {
		t.Fatal(err)
	}
}
