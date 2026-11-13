// Package fixture supplies isolated terminal consent for wake-mute tests only.
// A synthetic PTY is not live owner consent; never use it with production roots.
package fixture

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/wakemute"
)

func PTY(t testing.TB) (*os.File, *os.File) {
	t.Helper()
	master, slave, err := openPTY()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close(); master.Close() })
	return master, slave
}
func Context(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}
func Scope(root string) wakemute.Scope {
	return wakemute.Scope{{Root: root, Connections: []wakemute.Connection{{Path: "."}}}}
}

// Confirm uses only the scope supplied by a test, fresh operation IDs and real
// fixture fd checks. The test owns and removes every root passed here.
func Confirm(t testing.TB, scope wakemute.Scope, run, action string) wakemute.Operation {
	t.Helper()
	operation, err := wakemute.NewOperation(scope, run)
	if err != nil {
		t.Fatal(err)
	}
	master, slave := PTY(t)
	done := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(master)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				done <- err
				return
			}
			if strings.HasPrefix(line, "Type exactly: ") {
				_, err = io.WriteString(master, strings.TrimSpace(strings.TrimPrefix(line, "Type exactly: "))+"\n")
				done <- err
				return
			}
		}
	}()
	approved, err := wakemute.Confirm(Context(t), slave, slave, operation, action)
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	return approved
}
func Enroll(t testing.TB, scope wakemute.Scope) {
	t.Helper()
	t.Setenv("TMUX", "")
	t.Setenv("STY", "")
	if err := wakemute.Enroll(Context(t), Confirm(t, scope, "enrollment", "enroll")); err != nil {
		t.Fatal(err)
	}
}
func Mute(t testing.TB, scope wakemute.Scope, run string) wakemute.Operation {
	t.Helper()
	out, err := wakemute.Mute(Context(t), Confirm(t, scope, run, "mute"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "muted" || out.Quiesced || out.Operation == nil {
		t.Fatal("invalid mute result")
	}
	return *out.Operation
}
func Unmute(t testing.TB, o wakemute.Operation) {
	t.Helper()
	out, err := wakemute.Unmute(Context(t), o.Scope, o.Run, o.ID)
	if err != nil || out.Quiesced {
		t.Fatalf("cleanup: %v", err)
	}
}
