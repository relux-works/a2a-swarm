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
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Only copied native test images opt in. Config, images, links and traces live
// in a private directory under the developer's real home, whose ancestry is
// checked before setup. Child HOME/config/environment never select live state.
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
	// Shared /tmp ancestry must refuse under production policy. The real home
	// supplies a candidate base, not a promise that its ancestry is safe.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := hookExecutableParents(filepath.Join(home, "fixture")); err != nil {
		t.Skipf("native fixture requires home ancestry without Git markers or unsafe mode bits: %v", err)
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
		if err := os.WriteFile(path, f.data, 0555); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(f.first, f.link); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f hookExecutableFixture) environment(extra ...string) []string {
	return append([]string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + f.root,
		"TMPDIR=" + f.root, "TZ=UTC", "A2A_AGENT=worker",
		"A2A_HOOK_EXECUTABLE_FIXTURE=1",
		"A2A_CONFIG_DIR=" + filepath.Join(f.root, "config"),
		"GORACE=atexit_sleep_ms=0",
	}, extra...)
}

func (f hookExecutableFixture) install(t *testing.T, launch, runtime string, extraEnv []string, choice ...string) (string, int) {
	t.Helper()
	args := []string{"hook", "install", "--agent", "worker", "--runtime", runtime, "--settings", f.settings}
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
		if len(group.Hooks) == 1 && strings.Contains(group.Hooks[0].Command, " --as worker hook session-start") {
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

// Proves migration from a legacy checkout hook preserves foreign hooks, then
// executes the recorded installed link before and after an atomic retarget.
func TestHookExecutableUpgradeRoute(t *testing.T) {
	for _, runtime := range []string{"claude", "codex", "muse"} {
		t.Run(runtime, func(t *testing.T) {
			f := newHookExecutableFixture(t)
			foreign := `{"hooks":[{"type":"command","command":"echo foreign","timeout":8}]}`
			checkout := filepath.Join(f.root, "checkout")
			if err := os.MkdirAll(filepath.Join(checkout, "bin"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(checkout, ".git"), []byte("gitdir: private-fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			legacy := filepath.Join(checkout, "bin", "a2a")
			if err := os.Symlink(f.link, legacy); err != nil {
				t.Fatal(err)
			}
			old := `{"hooks":[{"type":"command","command":` + string(marshalNoEscape(shellQuote(legacy)+" --as worker hook session-start --runtime "+runtime)) + `,"timeout":6}]}`
			seed := `{"description":"keep me","hooks":{"SessionStart":[` + foreign + `,` + old + `],"OtherEvent":[` + foreign + `]}}`
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
			if strings.Contains(before, legacy) {
				t.Fatal("legacy checkout hook survived migration")
			}
			if runtime == "codex" {
				assertHookFixtureRules(t, f, f.link, f.first)
			}
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
			if !strings.HasPrefix(command, shellQuote(f.link)+" --as worker ") {
				t.Fatalf("installed upgrade route lost: %s", command)
			}
			if readFile(t, f.settings) != before {
				t.Fatal("upgrade rewrote the existing hook configuration")
			}
		})
	}
}

// Proves all runtimes pin fixed paths after direct and symlink launches;
// retargeting the launch alias does not redirect the recorded hook.
func TestHookExecutableFixedImage(t *testing.T) {
	for _, rt := range []string{"claude", "codex", "muse"} {
		for _, choice := range []string{"direct", "explicit", "resolved-symlink", "launch-symlink"} {
			t.Run(rt+"/"+choice, func(t *testing.T) {
				f := newHookExecutableFixture(t)
				launch := f.first
				var args []string
				switch choice {
				case "explicit":
					args = []string{"--executable", f.first}
				case "resolved-symlink":
					args = []string{"--executable", f.link}
				case "launch-symlink":
					launch = f.link
				}
				out, code := f.install(t, launch, rt, nil, args...)
				if code != ExitOK {
					t.Fatalf("fixed selection refused: %d: %s", code, out)
				}
				command := hookFixtureCommand(t, f.settings)
				if !strings.HasPrefix(command, shellQuote(f.first)+" --as worker ") {
					t.Fatalf("fixed selection changed image path: %s", command)
				}
				if rt == "codex" && (choice == "resolved-symlink" || choice == "launch-symlink") {
					assertHookFixtureRules(t, f, f.link, f.first)
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
}

// Checks the generated deny alternatives for known spellings, not Codex's
// interpretation of rules or protection against arbitrary undiscovered aliases.
func assertHookFixtureRules(t *testing.T, f hookExecutableFixture, aliases ...string) {
	t.Helper()
	text := readFile(t, filepath.Join(filepath.Dir(f.settings), "rules", "a2a-owner-dm.rules"))
	for _, alias := range aliases {
		if strings.Count(text, string(marshalNoEscape(alias))) != 10 {
			t.Fatalf("known executable spelling missing from DM terminal denials: %q", alias)
		}
	}
	if strings.Contains(text, `"status"`) || strings.Contains(text, `"bind"`) || strings.Contains(text, `"unbind"`) {
		t.Fatal("rules deny permitted owner-DM actions")
	}
}

// Proves direct relative targets and symlinked directory spellings are accepted
// without accepting a second command/image symlink (covered by refusal tests).
func TestHookExecutableDirectTargetSpellings(t *testing.T) {
	for _, spelling := range []string{"relative", "absolute-parent-alias", "relative-parent-alias"} {
		t.Run(spelling, func(t *testing.T) {
			f := newHookExecutableFixture(t)
			target := f.first
			if spelling != "relative" {
				alias := filepath.Join(f.root, "image-alias")
				if err := os.Symlink(filepath.Dir(f.first), alias); err != nil {
					t.Fatal(err)
				}
				target = filepath.Join(alias, "a2a")
			}
			if spelling != "absolute-parent-alias" {
				var err error
				target, err = filepath.Rel(filepath.Dir(f.link), target)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(f.link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, f.link); err != nil {
				t.Fatal(err)
			}
			out, code := f.install(t, f.link, "muse", nil, "--installed-command", f.link)
			if code != ExitOK {
				t.Fatalf("direct target refused: %d: %s", code, out)
			}
			f.executeRecorded(t, hookFixtureCommand(t, f.settings), f.first)
		})
	}
}

// Documents the OS-dependent no-flag checkout-alias result, while explicit
// checkout selection refuses on both platforms without changing settings.
func TestHookExecutableLegacyLaunchAlias(t *testing.T) {
	f := newHookExecutableFixture(t)
	checkout := filepath.Join(f.root, "checkout")
	if err := os.MkdirAll(filepath.Join(checkout, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(checkout, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(checkout, "bin", "a2a")
	if err := os.Symlink(f.link, alias); err != nil {
		t.Fatal(err)
	}
	out, code := f.install(t, alias, "muse", nil)
	if runtime.GOOS == "linux" {
		if code != ExitOK {
			t.Fatalf("resolved Linux image refused: %d: %s", code, out)
		}
	} else if code != ExitError || !strings.Contains(out, "mutable source checkout") {
		t.Fatalf("macOS checkout launch spelling was not refused: %d: %s", code, out)
	}
	before := hookFixtureSnapshot(t, filepath.Dir(f.settings))
	out, code = f.install(t, f.first, "muse", nil, "--executable", alias)
	if code != ExitError || !strings.Contains(out, "mutable source checkout") || hookFixtureSnapshot(t, filepath.Dir(f.settings)) != before {
		t.Fatalf("explicit checkout selection was not preserved/refused: %d: %s", code, out)
	}
	out, code = f.install(t, f.link, "muse", nil, "--installed-command", f.link)
	if code != ExitOK {
		t.Fatalf("explicit installed migration refused: %d: %s", code, out)
	}
}

// Proves missing executable refusal creates no settings directory, with a valid
// neighboring selection accepted for each runtime.
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

// Proves the named invalid selections return the intended status/reason and
// preserve every settings/rules byte and mode; it does not exercise live hooks.
func TestHookExecutableRefusal(t *testing.T) {
	for _, runtime := range []string{"claude", "codex", "muse"} {
		for _, name := range []string{"ambiguous", "relative", "empty", "not-symlink", "symlink-chain", "missing", "nonexec", "mismatch", "replaced-image", "replaced-link", "replaced-same-target-link", "malformed", "foreign-config", "checkout", "checkout-link", "target-spelling-checkout", "target-spelling-unsafe", "unsafe-image", "unsafe-directory", "unsafe-higher-ancestor", "setuid", "setgid", "root-owned", "root-owned-link", "dangling-git-marker", "truncated"} {
			t.Run(runtime+"/"+name, func(t *testing.T) {
				f := newHookExecutableFixture(t)
				seed := `{"description":"untouched","hooks":{}}`
				args := []string{"--installed-command", f.link}
				extra := []string{}
				want := ""
				wantCode := ExitError
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				switch name {
				case "ambiguous":
					wantCode = ExitUsage
					args = append(args, "--executable", f.first)
					want = "choose only one"
				case "relative":
					wantCode = ExitUsage
					args = []string{"--executable", "a2a"}
					want = "absolute path"
				case "empty":
					wantCode = ExitUsage
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
						seed = `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/foreign/a2a --as other-worker hook stop --runtime muse","timeout":6}]}]}}`
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
				case "target-spelling-checkout", "target-spelling-unsafe":
					aliasDir := filepath.Join(f.root, "target-spelling")
					must(os.Mkdir(aliasDir, 0700))
					alias := filepath.Join(aliasDir, "images")
					must(os.Symlink(filepath.Dir(f.first), alias))
					must(os.Remove(f.link))
					must(os.Symlink(filepath.Join(alias, "a2a"), f.link))
					if name == "target-spelling-checkout" {
						must(os.Mkdir(filepath.Join(aliasDir, ".git"), 0700))
						want = "mutable source checkout"
					} else {
						must(os.Chmod(aliasDir, 0777))
						want = "directory is writable"
					}
				case "unsafe-image":
					must(os.Chmod(f.first, 0722))
					want = "native executable"
				case "unsafe-directory":
					must(os.Chmod(filepath.Dir(f.link), 0777))
					want = "directory is writable"
				case "unsafe-higher-ancestor":
					must(os.Chmod(filepath.Join(f.root, "images"), 0777))
					want = "directory is writable"
				case "setuid", "setgid":
					mode := os.ModeSetuid
					if name == "setgid" {
						mode = os.ModeSetgid
					}
					must(os.Chmod(f.second, 0555|mode))
					args = []string{"--executable", f.second}
					want = "native executable"
				case "dangling-git-marker":
					must(os.Symlink("missing-git-dir", filepath.Join(filepath.Dir(f.link), ".git")))
					want = "mutable source checkout"
				case "root-owned", "root-owned-link":
					rootImage, err := filepath.EvalSymlinks("/bin/sh")
					must(err)
					info, err := os.Stat(rootImage)
					must(err)
					if os.Getuid() == 0 || info.Sys().(*syscall.Stat_t).Uid != 0 {
						t.Skip("requires a non-root caller and a root-owned system shell")
					}
					if name == "root-owned" {
						args = []string{"--executable", rootImage}
					} else {
						must(os.Remove(f.link))
						must(os.Symlink(rootImage, f.link))
					}
					want = "own-UID native executable"
				case "truncated":
					must(os.Chmod(f.second, 0700))
					must(os.WriteFile(f.second, f.data[:64], 0700))
					args = []string{"--executable", f.second}
					want = "native executable"
				}
				must(os.WriteFile(f.settings, []byte(seed), 0600))
				before := hookFixtureSnapshot(t, filepath.Dir(f.settings))
				out, code := f.install(t, f.first, runtime, extra, args...)
				if code != wantCode || !strings.Contains(out, want) {
					t.Fatalf("%s selection: code=%d, want refusal containing %q: %s", name, code, want, out)
				}
				if after := hookFixtureSnapshot(t, filepath.Dir(f.settings)); after != before {
					t.Fatalf("refusal partially changed configuration:\nbefore %s\nafter %s", before, after)
				}
			})
		}
	}
}

// Proves neither macOS's public system-temp spellings nor their /private
// counterparts evade writable-ancestor refusal. No system files are created.
func TestHookExecutableSystemTemporaryAncestry(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS directory aliases only")
	}
	for _, dir := range []string{"/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp"} {
		t.Run(dir, func(t *testing.T) {
			info, err := os.Stat(dir)
			if err != nil || info.Mode().Perm()&0022 == 0 {
				t.Skip("requires the standard writable macOS temporary directory")
			}
			err = hookExecutableParents(filepath.Join(dir, "a2a"))
			if err == nil || !strings.Contains(err.Error(), "directory is writable") {
				t.Fatalf("system temporary ancestry accepted or unrelated refusal: %v", err)
			}
		})
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
