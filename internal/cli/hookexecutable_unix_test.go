//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Only copied native test images opt in. Every config, binary, symlink and
// trace used below belongs to the test's private temporary directory.
func init() {
	if os.Getenv("A2A_HOOK_EXECUTABLE_FIXTURE") != "1" {
		return
	}
	if trace := os.Getenv("A2A_HOOK_EXECUTABLE_TRACE"); trace != "" {
		image, err := os.Executable()
		if err != nil || os.WriteFile(trace, []byte(image), 0600) != nil {
			os.Exit(91)
		}
	}
	a := NewApp()
	if replacement := os.Getenv("A2A_HOOK_EXECUTABLE_REPLACEMENT"); replacement != "" {
		a.hookSelection = func(c hookExecutableChoice) (hookExecutableSelection, error) {
			s, err := selectHookExecutable(c)
			if err != nil {
				return s, err
			}
			if os.Getenv("A2A_HOOK_EXECUTABLE_REPLACE_LINK") == "1" {
				err = os.Remove(c.installed)
				if err == nil {
					err = os.Symlink(replacement, c.installed)
				}
			} else {
				err = os.Rename(replacement, s.path)
			}
			return s, err
		}
	}
	os.Exit(a.Run(context.Background(), os.Args[1:]))
}

type hookExecutableFixture struct {
	root, first, second, link, settings, trace string
	data                                       []byte
}

func newHookExecutableFixture(t *testing.T) hookExecutableFixture {
	t.Helper()
	// Keep executable fixtures under a private directory with controlled
	// ancestry, rather than relying on a shared system temporary directory.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, ".a2a-hook-executable-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	f := hookExecutableFixture{root: root}
	f.first = filepath.Join(f.root, "images", "one", "a2a")
	f.second = filepath.Join(f.root, "images", "two", "a2a")
	f.link = filepath.Join(f.root, "installed", "a2a space'quote")
	f.settings = filepath.Join(f.root, "project", "hooks.json")
	f.trace = filepath.Join(f.root, "trace")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f.data, err = os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{f.first, f.second, f.link, f.settings} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{f.first, f.second} {
		if err := os.WriteFile(path, f.data, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(f.first, f.link); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f hookExecutableFixture) environment(extra ...string) []string {
	return append(append(os.Environ(), "A2A_HOOK_EXECUTABLE_FIXTURE=1", "A2A_CONFIG_DIR="+filepath.Join(f.root, "config"), "GORACE=atexit_sleep_ms=0"), extra...)
}

func (f hookExecutableFixture) install(t *testing.T, launch, runtime string, extraEnv []string, choice ...string) (string, int) {
	t.Helper()
	args := []string{"hook", "install", "--agent", "alice", "--runtime", runtime, "--settings", f.settings}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, launch, append(args, choice...)...)
	cmd.Env = f.environment(extraEnv...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("fixture install timed out: %s", out)
	}
	code := 0
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	return string(out), code
}

func hookFixtureCommand(t *testing.T, path string) string {
	t.Helper()
	var root struct {
		Hooks map[string][]hookGroup `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(readFile(t, path)), &root); err != nil {
		t.Fatal(err)
	}
	for _, group := range root.Hooks["SessionStart"] {
		if len(group.Hooks) == 1 && strings.Contains(group.Hooks[0].Command, " --as alice hook session-start") {
			return group.Hooks[0].Command
		}
	}
	t.Fatal("missing installed SessionStart command")
	return ""
}

func (f hookExecutableFixture) executeRecorded(t *testing.T, command, wantImage string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Env = f.environment("A2A_HOOK_EXECUTABLE_TRACE=" + f.trace)
	cmd.Stdin = strings.NewReader(`{"session_id":"isolated-fixture"}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("recorded hook did not execute: %v: %s", err, out)
	}
	got, err := filepath.EvalSymlinks(readFile(t, f.trace))
	if err != nil || got != wantImage {
		t.Fatalf("recorded hook executed image %q, want %q (%v)", got, wantImage, err)
	}
}

func TestHookExecutableUpgradeRoute(t *testing.T) {
	for _, runtime := range []string{"claude", "codex", "muse"} {
		t.Run(runtime, func(t *testing.T) {
			f := newHookExecutableFixture(t)
			foreign := `{"hooks":[{"type":"command","command":"echo foreign","timeout":8}]}`
			seed := `{"description":"keep me","hooks":{"SessionStart":[` + foreign + `],"OtherEvent":[` + foreign + `]}}`
			if err := os.WriteFile(f.settings, []byte(seed), 0600); err != nil {
				t.Fatal(err)
			}
			out, code := f.install(t, f.link, runtime, nil, "--installed-command", f.link)
			if code != 0 {
				t.Fatalf("stable selection refused: %d: %s", code, out)
			}
			before := readFile(t, f.settings)
			compact := compactJSON(t, before)
			if strings.Count(compact, foreign) != 2 || !strings.Contains(compact, `"description":"keep me"`) {
				t.Fatal("unrelated hook groups or keys changed")
			}
			command := hookFixtureCommand(t, f.settings)
			out, code = f.install(t, f.link, runtime, nil, "--installed-command", f.link)
			if code != 0 || !strings.Contains(out, "unchanged") || readFile(t, f.settings) != before {
				t.Fatalf("reinstall is not byte-idempotent: %d: %s", code, out)
			}
			f.executeRecorded(t, command, f.first)
			if err := os.Rename(f.first, f.first+".retired"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.second, f.link+".next"); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(f.link+".next", f.link); err != nil {
				t.Fatal(err)
			}
			t.Log("original recorded command executed; old image moved; installed link atomically switched to the second image")
			f.executeRecorded(t, command, f.second)
			if !strings.HasPrefix(command, shellQuote(f.link)+" --as alice ") {
				t.Fatalf("installed upgrade route lost: %s", command)
			}
			if readFile(t, f.settings) != before {
				t.Fatal("upgrade rewrote the existing hook configuration")
			}
		})
	}
}

func TestHookExecutableFixedImage(t *testing.T) {
	for _, choice := range []string{"direct", "explicit", "resolved-symlink"} {
		t.Run(choice, func(t *testing.T) {
			f := newHookExecutableFixture(t)
			var args []string
			if choice == "explicit" {
				args = []string{"--executable", f.first}
			} else if choice == "resolved-symlink" {
				args = []string{"--executable", f.link}
			}
			out, code := f.install(t, f.first, "muse", nil, args...)
			if code != 0 {
				t.Fatalf("fixed selection refused: %d: %s", code, out)
			}
			command := hookFixtureCommand(t, f.settings)
			if !strings.HasPrefix(command, shellQuote(f.first)+" --as alice ") {
				t.Fatalf("fixed selection changed image path: %s", command)
			}
			if err := os.Remove(f.link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.second, f.link); err != nil {
				t.Fatal(err)
			}
			f.executeRecorded(t, command, f.first)
		})
	}
}

func TestHookExecutableMissingSettings(t *testing.T) {
	for _, runtime := range []string{"claude", "codex", "muse"} {
		t.Run(runtime, func(t *testing.T) {
			f := newHookExecutableFixture(t)
			if err := os.Remove(filepath.Dir(f.settings)); err != nil {
				t.Fatal(err)
			}
			out, code := f.install(t, f.first, runtime, nil, "--executable", f.first+".missing")
			if code != ExitError || !strings.Contains(out, "missing or unresolved") {
				t.Fatalf("missing executable was not refused: %d: %s", code, out)
			}
			if _, err := os.Lstat(filepath.Dir(f.settings)); !os.IsNotExist(err) {
				t.Fatal("refusal created a settings directory")
			}
			out, code = f.install(t, f.first, runtime, nil, "--executable", f.first)
			if code != ExitOK {
				t.Fatalf("valid neighboring selection failed: %d: %s", code, out)
			}
		})
	}
}

func TestHookExecutableRefusal(t *testing.T) {
	for _, runtime := range []string{"claude", "codex", "muse"} {
		for _, name := range []string{"ambiguous", "relative", "empty", "not-symlink", "symlink-chain", "missing", "nonexec", "mismatch", "replaced-image", "replaced-link", "replaced-same-target-link", "malformed", "foreign-config", "checkout", "checkout-link", "unsafe-image", "unsafe-directory", "truncated"} {
			t.Run(runtime+"/"+name, func(t *testing.T) {
				f := newHookExecutableFixture(t)
				seed := `{"description":"untouched","hooks":{}}`
				args := []string{"--installed-command", f.link}
				extra := []string{}
				want := ""
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				switch name {
				case "ambiguous":
					args = append(args, "--executable", f.first)
					want = "choose only one"
				case "relative":
					args = []string{"--executable", "a2a"}
					want = "absolute path"
				case "empty":
					args = []string{"--installed-command", ""}
					want = "absolute path"
				case "not-symlink":
					args = []string{"--installed-command", f.first}
					want = "requires an explicitly selected installed symlink"
				case "symlink-chain":
					must(os.Symlink(f.first, f.link+".middle"))
					must(os.Remove(f.link))
					must(os.Symlink(f.link+".middle", f.link))
					want = "symlink chains are not supported"
				case "missing":
					args = []string{"--executable", filepath.Join(f.root, "missing")}
					want = "missing or unresolved"
				case "nonexec":
					must(os.Chmod(f.second, 0600))
					args = []string{"--executable", f.second}
					want = "native executable"
				case "mismatch":
					args = []string{"--executable", f.second}
					want = "does not match the running image"
				case "replaced-image":
					args = []string{"--executable", f.first}
					extra = []string{"A2A_HOOK_EXECUTABLE_REPLACEMENT=" + f.second}
					want = "image changed"
				case "replaced-link":
					extra = []string{"A2A_HOOK_EXECUTABLE_REPLACEMENT=" + f.second, "A2A_HOOK_EXECUTABLE_REPLACE_LINK=1"}
					want = "target changed"
				case "replaced-same-target-link":
					extra = []string{"A2A_HOOK_EXECUTABLE_REPLACEMENT=" + f.first, "A2A_HOOK_EXECUTABLE_REPLACE_LINK=1"}
					want = "installed command changed"
				case "malformed":
					seed = `{"hooks":{"Stop":{}}}`
					want = "refusing to change"
				case "foreign-config":
					if runtime == "muse" {
						seed = `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/foreign/a2a --as bob hook stop --runtime muse","timeout":6}]}]}}`
						want = "another identity"
					} else if runtime == "codex" {
						rules := filepath.Join(filepath.Dir(f.settings), "rules", "a2a-owner-dm.rules")
						must(os.MkdirAll(filepath.Dir(rules), 0700))
						must(os.WriteFile(rules, []byte("foreign rules\n"), 0600))
						want = "not an a2a rules file"
					} else {
						seed = `{"permissions":{"deny":{}}}`
						want = "must be a string array"
					}
				case "checkout":
					must(os.WriteFile(filepath.Join(filepath.Dir(f.first), ".git"), []byte("gitdir: elsewhere"), 0600))
					want = "mutable source checkout"
				case "checkout-link":
					must(os.Mkdir(filepath.Join(filepath.Dir(f.link), ".git"), 0700))
					want = "mutable source checkout"
				case "unsafe-image":
					must(os.Chmod(f.first, 0722))
					want = "native executable"
				case "unsafe-directory":
					must(os.Chmod(filepath.Dir(f.link), 0777))
					want = "directory is writable"
				case "truncated":
					must(os.WriteFile(f.second, f.data[:64], 0700))
					args = []string{"--executable", f.second}
					want = "native executable"
				}
				must(os.WriteFile(f.settings, []byte(seed), 0600))
				before := hookFixtureSnapshot(t, filepath.Dir(f.settings))
				out, code := f.install(t, f.first, runtime, extra, args...)
				if code == 0 || !strings.Contains(out, want) {
					t.Fatalf("%s selection: code=%d, want refusal containing %q: %s", name, code, want, out)
				}
				if after := hookFixtureSnapshot(t, filepath.Dir(f.settings)); after != before {
					t.Fatalf("refusal partially changed configuration:\nbefore %s\nafter %s", before, after)
				}
			})
		}
	}
}

func hookFixtureSnapshot(t *testing.T, root string) string {
	t.Helper()
	var out bytes.Buffer
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		fmt.Fprintf(&out, "%s %v ", rel, info.Mode())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(&out, "%q", data)
		}
		out.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}
