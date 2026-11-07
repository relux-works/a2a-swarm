package config

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

var hostStateOld = []byte("{\"next_batch\":\"old\"}\n")
var hostStateNew = []byte("{\"next_batch\":\"new\"}\n")

func hostStateFixture(t *testing.T) Paths {
	t.Helper()
	p := Paths{Base: t.TempDir()}
	if err := SaveState(p, "member", State{NextBatch: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.StateFile("member"), hostStateOld, SecretFileMode); err != nil {
		t.Fatal(err)
	}
	return p
}

// Proves the v2 Host-enrolled public SaveState path preserves actual data/custody
// and private mode through reload; legacy non-enrolled writing remains valid.
func TestHostStatePublicSave(t *testing.T) {
	p := hostStateFixture(t)
	scope, attachment, now := hostFixture()
	s := State{NextBatch: "new", Lease: &attachment.Lease}
	if err := s.EnrollHost(attachment, scope, now); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableHostNotifications(attachment, scope, now); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(p, "member", s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(p, "member")
	if err != nil || got.NextBatch != "new" || got.HostEnrollment == nil || !got.HostEnrollment.same(*s.HostEnrollment) {
		t.Fatalf("reload: %+v %v", got, err)
	}
	info, err := os.Stat(p.StateFile("member"))
	if err != nil || info.Mode().Perm() != SecretFileMode {
		t.Fatalf("private file: %v %v", info, err)
	}
}

// Proves opt-in persistence does not create an unprovisioned config namespace;
// the adjacent public-save control uses an already existing admitted base.
func TestHostStatePublicMissingNamespace(t *testing.T) {
	p := Paths{Base: filepath.Join(t.TempDir(), "unprovisioned")}
	scope, attachment, now := hostFixture()
	s := State{Lease: &attachment.Lease}
	if err := s.EnrollHost(attachment, scope, now); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableHostNotifications(attachment, scope, now); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(p, "member", s); !errors.Is(err, ErrHostStateScope) {
		t.Fatalf("unprovisioned namespace accepted: %v", err)
	}
	if _, err := os.Lstat(p.Base); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("namespace created: %v", err)
	}
}

// Main-era v1 enrollment saves still accept an existing 0755 directory. The
// same namespace refuses v2 admission without changing the v1 file; after the
// owner provisions private mode, the identical v2 candidate can be saved.
func TestHostStateV1WriterCompatibility(t *testing.T) {
	p := hostStateFixture(t)
	if err := os.Chmod(filepath.Join(p.Base, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	scope, attachment, now := hostFixture()
	s := State{Lease: &attachment.Lease, NextBatch: "v1"}
	if err := s.EnrollHost(attachment, scope, now); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(p, "member", s); err != nil {
		t.Fatalf("v1 writer compatibility: %v", err)
	}
	got, err := LoadState(p, "member")
	if err != nil || !reflect.DeepEqual(got, s) {
		t.Fatalf("v1 reload: %v", err)
	}
	before, err := os.ReadFile(p.StateFile("member"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnableHostNotifications(attachment, scope, now); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(p, "member", s); !errors.Is(err, ErrHostStateScope) {
		t.Fatalf("v2 accepted nonprivate directory: %v", err)
	}
	after, err := os.ReadFile(p.StateFile("member"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refused v2 upgrade changed the v1 file")
	}
	if err := os.Chmod(filepath.Join(p.Base, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(p, "member", s); err != nil {
		t.Fatalf("private v2 control: %v", err)
	}
	got, err = LoadState(p, "member")
	if err != nil || !reflect.DeepEqual(got, s) {
		t.Fatalf("v2 reload: %v", err)
	}
}

// Proves real sync operations occur in directory/file/directory order, and each
// injected sync failure blocks success with the intended uncertainty category.
func TestHostStateSyncBoundaries(t *testing.T) {
	for failAt := 0; failAt <= 3; failAt++ {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			p := hostStateFixture(t)
			var kinds []string
			err := writeHostStateWith(p, "member", hostStateNew, hostStateOps{sync: func(f *os.File) error {
				info, err := f.Stat()
				if err != nil {
					return err
				}
				kind := "file"
				if info.IsDir() {
					kind = "directory"
				}
				kinds = append(kinds, kind)
				if failAt == len(kinds) {
					return errors.New("injected fsync failure")
				}
				return f.Sync()
			}})
			if failAt == 0 {
				if err != nil || !reflect.DeepEqual(kinds, []string{"directory", "file", "directory"}) {
					t.Fatalf("missing required sync: %v %v", kinds, err)
				}
			} else if len(kinds) != failAt || !errors.Is(err, ErrHostStatePersistence) || errors.Is(err, ErrHostStateUncertain) != (failAt == 3) {
				t.Fatalf("wrong persistence result at sync%d: %v %v", failAt, kinds, err)
			}
			want := hostStateOld
			if failAt == 0 || failAt == 3 {
				want = hostStateNew
			}
			got, readErr := os.ReadFile(p.StateFile("member"))
			if readErr != nil || !bytes.Equal(got, want) {
				t.Fatalf("state after sync%d: %s %v", failAt, got, readErr)
			}
		})
	}
}

// Proves returned failures never leave a partial file or an owned temporary;
// after rename the new bytes may exist but the result remains uncertain.
func TestHostStateReturnedFaults(t *testing.T) {
	for _, stage := range []string{"directory-opened", "temp-created", "file-written", "file-synced", "file-closed", "renamed", "directory-synced"} {
		t.Run(stage, func(t *testing.T) {
			p := hostStateFixture(t)
			err := writeHostStateWith(p, "member", hostStateNew, hostStateOps{checkpoint: func(at string) error {
				if at == stage {
					return errors.New("injected boundary failure")
				}
				return nil
			}})
			uncertain := stage == "renamed" || stage == "directory-synced"
			if !errors.Is(err, ErrHostStatePersistence) || errors.Is(err, ErrHostStateUncertain) != uncertain {
				t.Fatalf("wrong boundary refusal: %v", err)
			}
			want := hostStateOld
			if uncertain {
				want = hostStateNew
			}
			got, readErr := os.ReadFile(p.StateFile("member"))
			if readErr != nil || !bytes.Equal(got, want) {
				t.Fatalf("non-atomic state: %s %v", got, readErr)
			}
			entries, err := os.ReadDir(filepath.Join(p.Base, "state"))
			if err != nil || len(entries) != 1 || entries[0].Name() != "member.json" {
				t.Fatalf("temporary leaked: %v %v", entries, err)
			}
		})
	}
}

// Proves directory-relative operations stay in the opened state root if the
// pathname is replaced; the replacement directory's sentinel is untouched.
func TestHostStatePinnedDirectory(t *testing.T) {
	p := hostStateFixture(t)
	err := writeHostStateWith(p, "member", hostStateNew, hostStateOps{checkpoint: func(at string) error {
		if at != "directory-opened" {
			return nil
		}
		if err := os.Rename(filepath.Join(p.Base, "state"), filepath.Join(p.Base, "original-state")); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(p.Base, "state"), 0o700); err != nil {
			return err
		}
		return os.WriteFile(p.StateFile("member"), []byte("replacement sentinel"), SecretFileMode)
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p.StateFile("member"))
	if err != nil || string(got) != "replacement sentinel" {
		t.Fatalf("replacement written: %s %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(p.Base, "original-state", "member.json"))
	if err != nil || !bytes.Equal(got, hostStateNew) {
		t.Fatalf("opened directory lost: %s %v", got, err)
	}
}

// Proves unsafe paths/modes and oversized payloads refuse before mutation,
// retaining the original file and an isolated outside sentinel.
func TestHostStateScopeAndCapacity(t *testing.T) {
	for _, scenario := range []string{"name", "file-link", "directory-link", "file-mode", "directory-mode", "capacity"} {
		t.Run(scenario, func(t *testing.T) {
			p := hostStateFixture(t)
			outside := filepath.Join(t.TempDir(), "sentinel")
			if err := os.WriteFile(outside, []byte("outside"), SecretFileMode); err != nil {
				t.Fatal(err)
			}
			name, data := "member", hostStateNew
			switch scenario {
			case "name":
				name = "../sentinel"
			case "file-link":
				if err := os.Remove(p.StateFile(name)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, p.StateFile(name)); err != nil {
					t.Fatal(err)
				}
			case "directory-link":
				if err := os.Rename(filepath.Join(p.Base, "state"), filepath.Join(p.Base, "original-state")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(outside), filepath.Join(p.Base, "state")); err != nil {
					t.Fatal(err)
				}
			case "file-mode":
				if err := os.Chmod(p.StateFile(name), 0o644); err != nil {
					t.Fatal(err)
				}
			case "directory-mode":
				if err := os.Chmod(filepath.Join(p.Base, "state"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "capacity":
				data = make([]byte, hostStateMaxBytes+1)
			}
			err := writeHostStateAtomic(p, name, data)
			if scenario == "capacity" {
				if !errors.Is(err, ErrHostStateCapacity) {
					t.Fatalf("wrong capacity refusal: %v", err)
				}
			} else if !errors.Is(err, ErrHostStateScope) {
				t.Fatalf("wrong scope refusal: %v", err)
			}
			got, err := os.ReadFile(outside)
			if err != nil || string(got) != "outside" {
				t.Fatalf("outside changed: %s %v", got, err)
			}
			if scenario != "name" && scenario != "file-link" && scenario != "directory-link" {
				got, err = os.ReadFile(p.StateFile("member"))
				if err != nil || !bytes.Equal(got, hostStateOld) {
					t.Fatalf("prior state changed: %s %v", got, err)
				}
			}
		})
	}
}

// Proves abrupt process exit at these actual boundaries leaves a whole old/new
// file; this is process-crash evidence, not a simulated power-loss guarantee.
func TestHostStateProcessCrash(t *testing.T) {
	for _, stage := range []string{"temp-created", "file-synced", "renamed", "directory-synced"} {
		t.Run(stage, func(t *testing.T) {
			p := hostStateFixture(t)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestHostStateCrashHelper$")
			cmd.Env = []string{"A2A_TEST_HOST_STATE_CRASH=1", "A2A_TEST_HOST_STATE_BASE=" + p.Base, "A2A_TEST_HOST_STATE_STAGE=" + stage}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Logf("isolated crash helper pid=%d", cmd.Process.Pid)
			err = cmd.Wait()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 37 || ctx.Err() != nil {
				t.Fatalf("wrong crash: %v", err)
			}
			want := hostStateOld
			if stage == "renamed" || stage == "directory-synced" {
				want = hostStateNew
			}
			got, err := os.ReadFile(p.StateFile("member"))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("partial crash state: %s %v", got, err)
			}
		})
	}
}

func TestHostStateCrashHelper(t *testing.T) {
	if os.Getenv("A2A_TEST_HOST_STATE_CRASH") != "1" {
		return
	}
	// This isolated child receives only its fixture base/stage, no caller identity.
	p := Paths{Base: os.Getenv("A2A_TEST_HOST_STATE_BASE")}
	err := writeHostStateWith(p, "member", hostStateNew, hostStateOps{checkpoint: func(at string) error {
		if at == os.Getenv("A2A_TEST_HOST_STATE_STAGE") {
			os.Exit(37)
		}
		return nil
	}})
	t.Fatalf("crash plant was not applied: %v", err)
}
