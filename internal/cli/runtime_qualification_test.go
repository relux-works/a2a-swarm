package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/codexwake"
	"github.com/relux-works/a2a-swarm/internal/config"
)

// Proves through the injected native transport that bind records a
// qualification-admitted connection on the durable binding and that
// runtime status prints "under qualification" for it, beside the same
// bind/status flow for a default-admitted 0.160.1 binding without the mark.
// The injected transport stands in for Dial; protected entry loading is
// proven separately below.
func TestRuntimeQualificationBindingStatus(t *testing.T) {
	e, _, control, _ := runtimeWorld(t)
	_ = control
	if e.state("alice").Runtime.UnderQualification {
		t.Fatal("default-admitted binding marked under qualification")
	}
	if out := e.as("alice", "runtime", "status").ok(t).stdout; strings.Contains(out, "under qualification") {
		t.Fatalf("default status claimed qualification: %s", out)
	}

	e2, _, n := runtimeUnboundWorld(t)
	n.version = "0.167.0"
	n.qualified = true
	e2.as("alice", "runtime", "bind", "--codex-thread", n.thread.ID, "--endpoint", n.endpoint, "--owner-pid", strconv.Itoa(os.Getpid()), "--heartbeat", "1m", "--max-wakes-per-hour", "2", "--gate", "ready", "--hooks", "none").ok(t)
	binding := e2.state("alice").Runtime
	if binding.ServerVersion != "0.167.0" || !binding.UnderQualification {
		t.Fatalf("qualification mark not recorded: %+v", binding)
	}
	if out := e2.as("alice", "runtime", "status").ok(t).stdout; !strings.Contains(out, "under qualification") {
		t.Fatalf("status hid qualification: %s", out)
	}
}

// Proves the production loader fails closed through the shared protected
// host-file reader: an absent, malformed or relative selector and an
// unprotected, group-writable or symlinked entry all yield no admission in
// this non-root fixture, beside a parsed valid entry supplied through the
// test seam. Root-owned acceptance of the shared reader itself is covered
// by that reader's own package tests.
func TestRuntimeNativeQualificationLoader(t *testing.T) {
	valid, err := codexwake.ParseQualification([]byte(`{"version":"a2a-native-qualification/1","qualified_versions":["0.167.0"]}`))
	if err != nil {
		t.Fatal(err)
	}
	seamed := &App{loadNativeQualification: func(config.Paths) (codexwake.Qualification, error) { return valid, nil }}
	if got := seamed.nativeQualification(config.Paths{}); !got.Admits("0.167.0") || got.Admits("0.165.0") {
		t.Fatal("valid parsed entry did not admit exactly its named version")
	}
	failing := &App{loadNativeQualification: func(config.Paths) (codexwake.Qualification, error) {
		return valid, errors.New("entry unavailable")
	}}
	if failing.nativeQualification(config.Paths{}).Admits("0.167.0") {
		t.Fatal("loader error was turned into admission")
	}

	base := t.TempDir()
	entry := filepath.Join(base, "entry.json")
	if err := os.WriteFile(entry, []byte(`{"version":"a2a-native-qualification/1","qualified_versions":["0.167.0"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	selector := filepath.Join(base, "native-qualification.json")
	writeSelector := func(body string) {
		t.Helper()
		if err := os.WriteFile(selector, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	production := &App{}
	paths := config.Paths{Base: base}
	if production.nativeQualification(paths).Admits("0.167.0") {
		t.Fatal("missing selector admitted a version")
	}
	writeSelector(`{"qualification_path":"relative.json"}`)
	if production.nativeQualification(paths).Admits("0.167.0") {
		t.Fatal("relative selector admitted a version")
	}
	writeSelector(`{`)
	if production.nativeQualification(paths).Admits("0.167.0") {
		t.Fatal("malformed selector admitted a version")
	}
	writeSelector(`{"qualification_path":"` + entry + `"}`)
	if production.nativeQualification(paths).Admits("0.167.0") {
		t.Fatal("unprotected entry admitted a version")
	}
	if err := os.Chmod(entry, 0o660); err != nil {
		t.Fatal(err)
	}
	if production.nativeQualification(paths).Admits("0.167.0") {
		t.Fatal("group-writable entry admitted a version")
	}
	link := filepath.Join(base, "entry-link.json")
	if err := os.Symlink(entry, link); err != nil {
		t.Fatal(err)
	}
	writeSelector(`{"qualification_path":"` + link + `"}`)
	if production.nativeQualification(paths).Admits("0.167.0") {
		t.Fatal("symlinked entry admitted a version")
	}
	// The selector target itself is selected exactly in the valid shape.
	writeSelector(`{"qualification_path":"` + entry + `"}`)
	if got := config.NativeQualificationPath(paths); got != entry {
		t.Fatalf("selector path = %q want %q", got, entry)
	}
}
