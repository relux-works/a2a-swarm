package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Reserving a killed-owner control must exclude unrelated sweepers while its
// observer lives, without stranding records after that observer dies. No daemon
// or server is named here: removal of the private record is the observable effect.
func TestBindFixtureReaper(t *testing.T) {
	self, err := bindFixtureIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "cat")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	dead, err := bindFixtureIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	_ = in.Close()
	select {
	case <-done:
		if waitErr != nil {
			t.Fatal(waitErr)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	raw, err := json.Marshal(self)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		record bindFixtureRecord
		env    string
		keep   bool
	}{
		{"live-observer", bindFixtureRecord{Owner: dead, Reaper: self}, "", true},
		{"authorized-reaper", bindFixtureRecord{Owner: dead, Reaper: self}, string(raw), false},
		{"dead-observer", bindFixtureRecord{Owner: dead, Reaper: dead}, "", false},
		{"live-owner", bindFixtureRecord{Owner: self, Reaper: dead}, "", true},
		{"ordinary-stale-record", bindFixtureRecord{Owner: dead}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(base, "registry")
			if err = os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			dir, err := os.MkdirTemp(root, "control-")
			if err != nil {
				t.Fatal(err)
			}
			if err = writeBindFixtureRecord(dir, tc.record); err != nil {
				t.Fatal(err)
			}
			t.Setenv("A2A_RUN_BIND_FIXTURE_REAPER", tc.env)
			if err = sweepBindFixtures(root); err != nil {
				t.Fatal(err)
			}
			_, err = os.Stat(dir)
			if tc.keep && err != nil || !tc.keep && !os.IsNotExist(err) {
				t.Fatalf("record preservation: keep=%v err=%v", tc.keep, err)
			}
		})
	}
	for _, raw := range []string{"{", "{}", `{"PID":1,"Birth":"wrong"}`, `{"PID":42}`} {
		t.Run("invalid-reaper/"+raw, func(t *testing.T) {
			t.Setenv("A2A_RUN_BIND_FIXTURE_REAPER", raw)
			if _, err := bindFixtureReaper(); err == nil {
				t.Fatal("invalid reaper accepted")
			}
		})
	}
}
