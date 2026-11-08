package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestOpaqueUnawareBuildExactRevision(t *testing.T) {
	old := "bb71277ac9c4d49649a8903424d8bafb0e0c779a"
	for _, tt := range []struct {
		name, revision, modified, module, command string
		want                                      bool
	}{
		{"old", old, "false", "github.com/relux-works/a2a-swarm", "github.com/relux-works/a2a-swarm/cmd/a2a", true},
		{"fix", "5f2b8439a5e51070335cfd8ea07b27d7faa3af13", "false", "github.com/relux-works/a2a-swarm", "github.com/relux-works/a2a-swarm/cmd/a2a", false},
		{"unknown", strings.Repeat("f", 40), "false", "github.com/relux-works/a2a-swarm", "github.com/relux-works/a2a-swarm/cmd/a2a", false},
		{"abbreviated", old[:8], "false", "github.com/relux-works/a2a-swarm", "github.com/relux-works/a2a-swarm/cmd/a2a", false},
		{"dirty", old, "true", "github.com/relux-works/a2a-swarm", "github.com/relux-works/a2a-swarm/cmd/a2a", false},
		{"missing-dirty", old, "", "github.com/relux-works/a2a-swarm", "github.com/relux-works/a2a-swarm/cmd/a2a", false},
		{"foreign-module", old, "false", "example.test/tool", "github.com/relux-works/a2a-swarm/cmd/a2a", false},
		{"foreign-command", old, "false", "github.com/relux-works/a2a-swarm", "example.test/tool", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			info := &debug.BuildInfo{Path: tt.command, Main: debug.Module{Path: tt.module}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: tt.revision}, {Key: "vcs.modified", Value: tt.modified}}}
			if got := opaqueUnawareBuild(info); got != tt.want {
				t.Fatalf("known old = %v, want %v", got, tt.want)
			}
		})
	}
	if opaqueUnawareBuild(nil) {
		t.Fatal("absent metadata is not a known old writer")
	}
	for _, key := range []string{"vcs.revision", "vcs.modified"} {
		info := &debug.BuildInfo{Path: "github.com/relux-works/a2a-swarm/cmd/a2a", Main: debug.Module{Path: "github.com/relux-works/a2a-swarm"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: old}, {Key: "vcs.modified", Value: "false"}, {Key: key, Value: "malformed-duplicate"}}}
		if opaqueUnawareBuild(info) {
			t.Fatal("duplicate version metadata claimed known old")
		}
	}
}

func TestStateWriterHookSelection(t *testing.T) {
	path := "/opt/libexec/a2a/a2a-image"
	for _, tt := range []struct{ command, want string }{
		{path + " --as alice hook stop --runtime codex", path},
		{shellQuote("/opt/space and 'quote/libexec/a2a/image") + " --as alice hook prompt --runtime muse", "/opt/space and 'quote/libexec/a2a/image"},
		{"env A2A_CONFIG_DIR=/tmp/synthetic " + path + " --as alice hook stop --runtime claude", path},
		{path + " --as bob hook stop --runtime codex", ""},
		{path + " --as alice hook stop --runtime codex; touch /tmp/unwanted", ""},
		{"$IMAGE --as alice hook stop --runtime codex", ""},
		{"a2a --as alice hook stop --runtime codex", ""},
	} {
		if got := stateWriterHookPath(tt.command, "alice"); got != tt.want {
			t.Fatalf("command %q selected %q, want %q", tt.command, got, tt.want)
		}
	}
	valid, _ := json.Marshal(map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []hookCommand{{Type: "command", Command: path + " --as alice hook stop --runtime codex"}}}}}})
	if got := stateWriterHookPaths(valid, "alice"); len(got) != 1 || got[0] != path {
		t.Fatalf("valid hook paths = %q", got)
	}
	for _, data := range [][]byte{[]byte(`{"hooks":`), []byte(`{"hooks":{},"hooks":{}}`), append(valid, []byte(` {}`)...), valid} {
		if got := stateWriterHookPaths(data, "bob"); len(got) != 0 {
			t.Fatalf("malformed or foreign settings selected %q", got)
		}
	}
}

func TestStateWriterStatusUnknownImage(t *testing.T) {
	e, _ := world(t)
	// There is no bound consumer. A malformed image in the default hook file
	// is unknown, and status must not execute it or rewrite settings/State.
	dir := outputTestDir(t)
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCWD) })
	if err := os.MkdirAll(".codex", 0700); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(dir, "libexec", "a2a", "a2a-image")
	if err := os.MkdirAll(filepath.Dir(image), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(image, []byte("#!/bin/sh\nexit 97\n"), 0700); err != nil {
		t.Fatal(err)
	}
	settings, _ := json.Marshal(map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []hookCommand{{Type: "command", Command: image + " --as alice hook stop --runtime codex"}}}}}})
	if err := os.WriteFile(DefaultCodexHooks, settings, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"bind", "status"}, {"bind", "status", "--json"}} {
		r := e.as("alice", args...).ok(t)
		if strings.Contains(r.stdout, "older State writers") || strings.Contains(r.stdout, "state_writer_warnings") {
			t.Fatalf("unknown image claimed known old: %s", r.stdout)
		}
	}
	after, err := os.ReadFile(DefaultCodexHooks)
	if err != nil || string(after) != string(settings) {
		t.Fatal("status rewrote hook settings")
	}
}

func TestStateWriterPinnedAndQuotedHooks(t *testing.T) {
	path := "/opt/space and 'quote/libexec/a2a/image.Ab2xY9/a2a"
	pin := " --profile production --connection-fingerprint " + strings.Repeat("a", 64)
	quoted := "env " + shellQuote("A2A_CONFIG_DIR=/opt/state and 'quote") + " " + shellQuote(path)
	for _, tt := range []struct{ command, want string }{
		{shellQuote(path) + pin + " --as alice hook stop --runtime codex", path},
		{quoted + " --as alice hook prompt --runtime muse", path},
		{quoted + pin + " --as alice hook session-start --runtime claude", path},
		{quoted + pin + " --as bob hook stop --runtime codex", ""},
		{quoted + " --profile production --as alice hook stop --runtime codex", ""},
		{quoted + strings.ReplaceAll(pin, strings.Repeat("a", 64), strings.Repeat("g", 64)) + " --as alice hook stop --runtime codex", ""},
		{quoted + pin + " --as alice hook stop --runtime codex; echo bad", ""},
	} {
		if got := stateWriterHookPath(tt.command, "alice"); got != tt.want {
			t.Fatalf("pinned/quoted hook selected %q, want %q", got, tt.want)
		}
	}
}

func TestStateWriterLsofTextVnode(t *testing.T) {
	valid := "p42\nftxt\nD0x1000004\ni123\nn/opt/libexec/a2a/image.Ab2xY9/a2a\n"
	for _, tt := range []struct{ name, raw, image, want string }{
		{"valid", valid, "0x1000004:123", "/opt/libexec/a2a/image.Ab2xY9/a2a"},
		{"unrelated-first-record", "p42\nfmem\nD0x9\ni9\nn/ignored\n" + valid, "0x1000004:123", "/opt/libexec/a2a/image.Ab2xY9/a2a"},
		{"next-record", valid + "fmem\nD0x9\ni9\nn/ignored\n", "0x1000004:123", "/opt/libexec/a2a/image.Ab2xY9/a2a"},
		{"different-loaded-vnode", valid, "0x1000004:124", ""},
		{"missing-device", strings.ReplaceAll(valid, "D0x1000004\n", ""), "0x1000004:123", ""},
		{"missing-inode", strings.ReplaceAll(valid, "i123\n", ""), "0x1000004:123", ""},
		{"missing-path", strings.ReplaceAll(valid, "n/opt/libexec/a2a/image.Ab2xY9/a2a\n", ""), "0x1000004:123", ""},
		{"relative-path", strings.ReplaceAll(valid, "n/opt/", "nopt/"), "0x1000004:123", ""},
		{"duplicate-inode", valid + "i124\n", "0x1000004:123", ""},
		{"duplicate-name", valid + "n/replacement\n", "0x1000004:123", ""},
		{"not-text", strings.ReplaceAll(valid, "ftxt", "fmem"), "0x1000004:123", ""},
		{"no-fallback-to-second-text", "ftxt\nD0x9\ni9\nn/other\n" + valid, "0x1000004:123", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := stateWriterLsofPath(tt.raw, tt.image); got != tt.want {
				t.Fatalf("path = %q, want %q", got, tt.want)
			}
		})
	}
}
