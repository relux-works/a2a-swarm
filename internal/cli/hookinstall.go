package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// DefaultHookSettings is where `a2a hook install --runtime claude` writes by
// default: the project's local (not committed) Claude Code settings.
const DefaultHookSettings = ".claude/settings.local.json"

// DefaultCodexHooks is where `a2a hook install --runtime codex` writes by
// default: the project layer's hooks.json, which Codex loads only in a trusted
// project (research/codex-hooks.md).
const DefaultCodexHooks = ".codex/hooks.json"

// ourHookRE recognizes a command written by `a2a hook install`, whatever the
// binary path: <path> --as <name> hook <event> --runtime <rt>. The form
// without --runtime was written by earlier versions for Claude Code only.
var ourHookRE = regexp.MustCompile(`^(?:env (?:'(?:[^']|'\\'')*'|[^\s']+) )?(?:'(?:[^']|'\\'')*'|[^\s']+)(?: --profile [a-z0-9][a-z0-9_-]{0,63} --connection-fingerprint [a-f0-9]{64})? --as ([a-z0-9][a-z0-9._=-]{0,63}) hook (stop|session-start|prompt)(?: --runtime (claude|codex|muse))?$`)

var shellSafeRE = regexp.MustCompile(`^[A-Za-z0-9_./+:@%=,-]+$`)

// shellQuote quotes s for the shell the runtime runs hook commands with (Codex:
// $SHELL -lc).
func shellQuote(s string) string {
	if shellSafeRE.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// jsonObject is a JSON object that keeps its key order and the raw values of
// keys it does not touch, so a merge changes nothing else in the file.
type jsonObject struct {
	keys []string
	vals map[string]json.RawMessage
}

func newJSONObject() *jsonObject { return &jsonObject{vals: map[string]json.RawMessage{}} }

// parseJSONObject parses data, which must be one JSON object.
func parseJSONObject(data []byte) (*jsonObject, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	o := newJSONObject()
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, ok := tok.(string)
		if !ok {
			return nil, errors.New("not a JSON object")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o.set(k, v)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, errors.New("trailing data after the JSON object")
	}
	return o, nil
}

func (o *jsonObject) get(k string) (json.RawMessage, bool) {
	v, ok := o.vals[k]
	return v, ok
}

func (o *jsonObject) set(k string, v json.RawMessage) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *jsonObject) del(k string) {
	if _, ok := o.vals[k]; !ok {
		return
	}
	delete(o.vals, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

func (o *jsonObject) marshal() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(marshalNoEscape(k))
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}

// marshalNoEscape encodes v without HTML escaping (json.Marshal would turn
// "&&" in a user's hook command into &&).
func marshalNoEscape(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// rawArray joins raw values into a JSON array without re-encoding them.
func rawArray(vals []json.RawMessage) json.RawMessage {
	parts := make([][]byte, len(vals))
	for i, v := range vals {
		parts[i] = v
	}
	return append(append([]byte("["), bytes.Join(parts, []byte(","))...), ']')
}

// hookGroup is the matcher group `a2a hook install` writes.
type hookGroup struct {
	Hooks []hookCommand `json:"hooks"`
}

type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// childObject returns the object under k (a new one when absent).
func (o *jsonObject) childObject(k string) (*jsonObject, error) {
	raw, ok := o.get(k)
	if !ok {
		return newJSONObject(), nil
	}
	c, err := parseJSONObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%q is not a JSON object; refusing to change it", k)
	}
	return c, nil
}

// ourHookGroup reports whether a hook matcher group is exactly one command
// written by `a2a hook install` for runtime rt and event arg, and the agent it
// names. A command without --runtime (earlier versions) belongs to claude.
func ourHookGroup(raw json.RawMessage, rt hookRuntime, arg string) (string, bool) {
	var g map[string]json.RawMessage
	if json.Unmarshal(raw, &g) != nil {
		return "", false
	}
	for k := range g {
		if k != "hooks" && k != "matcher" {
			return "", false
		}
	}
	var hooks []struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	if json.Unmarshal(g["hooks"], &hooks) != nil || len(hooks) != 1 || hooks[0].Type != "command" {
		return "", false
	}
	m := ourHookRE.FindStringSubmatch(hooks[0].Command)
	if m == nil || m[2] != arg {
		return "", false
	}
	if m[3] != rt.name && (m[3] != "" || rt.name != "claude") {
		return "", false
	}
	return m[1], true
}

// mergeHooks installs (cmd != nil) or removes (cmd == nil) our entry for every
// event. It returns the agent names of the removed entries.
func mergeHooks(root *jsonObject, rt hookRuntime, cmd func(arg string) string) ([]string, error) {
	hooks, err := root.childObject("hooks")
	if err != nil {
		return nil, err
	}
	var removed []string
	touched := false
	for _, ev := range hookEvents {
		var groups []json.RawMessage
		if raw, ok := hooks.get(ev.name); ok {
			if json.Unmarshal(raw, &groups) != nil {
				return nil, fmt.Errorf("hooks.%s is not a JSON array; refusing to change it", ev.name)
			}
		}
		var desired json.RawMessage
		if cmd != nil {
			desired = marshalNoEscape(hookGroup{Hooks: []hookCommand{{Type: "command", Command: cmd(ev.arg)}}})
		}
		out := []json.RawMessage{}
		placed := false
		for _, g := range groups {
			if name, ok := ourHookGroup(g, rt, ev.arg); ok {
				removed = append(removed, name)
				touched = true
				if desired != nil && !placed {
					out = append(out, desired) // replace in place (a moved binary)
					placed = true
				}
				continue
			}
			out = append(out, g)
		}
		if desired != nil && !placed {
			out = append(out, desired)
			touched = true
		}
		if len(out) == 0 {
			hooks.del(ev.name)
			continue
		}
		hooks.set(ev.name, rawArray(out))
	}
	if !touched {
		return removed, nil
	}
	if len(hooks.keys) == 0 {
		root.del("hooks")
	} else {
		root.set("hooks", hooks.marshal())
	}
	return removed, nil
}

// cmdHookInstall: a2a hook install --agent NAME --runtime claude|codex|muse [--settings PATH] /
// a2a hook uninstall --runtime claude|codex|muse [--settings PATH]
func cmdHookInstall(a *App, args []string, install bool) error {
	name := "hook uninstall"
	usage := "usage: a2a hook uninstall --runtime claude|codex|muse [--settings PATH]"
	if install {
		name, usage = "hook install", "usage: a2a hook install --agent NAME --runtime claude|codex|muse [--settings PATH] [--executable PATH | --installed-command PATH]"
	}
	fs := newFlagSet(name)
	settings := fs.String("settings", "", "file to change (claude: "+DefaultHookSettings+", codex: "+DefaultCodexHooks+", muse: "+DefaultMuseHooks+")")
	runtime := fs.String("runtime", "", "agent runtime: "+runtimeNames+" (required)")
	var agent *string
	var binary, installed *string
	if install {
		agent = fs.String("agent", "", "agent name the hooks deliver for (required)")
		binary = fs.String("executable", "", "absolute path to this running binary; record its resolved image")
		installed = fs.String("installed-command", "", "absolute installed symlink to this running binary; retain its upgrade route")
	}
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	settingsSet := false
	fs.Visit(func(f *flag.Flag) { settingsSet = settingsSet || f.Name == "settings" })
	if len(pos) != 0 || (settingsSet && *settings == "") {
		return usagef("%s", usage)
	}
	rt, err := lookupRuntime(*runtime)
	if err != nil {
		return usagef("%v; %s", err, usage)
	}
	if !settingsSet {
		*settings = rt.settings
	}
	if install {
		if *agent == "" {
			return usagef("--agent is required; %s", usage)
		}
		if err := config.ValidateName(*agent); err != nil {
			return usageError{err.Error()}
		}
	}
	var selected hookExecutableSelection
	if install {
		choice := hookExecutableChoice{binary: *binary, installed: *installed}
		fs.Visit(func(f *flag.Flag) {
			choice.binarySet = choice.binarySet || f.Name == "executable"
			choice.installedSet = choice.installedSet || f.Name == "installed-command"
		})
		if err := choice.validate(); err != nil {
			return err
		}
		selected, err = a.selectHookExecutable(choice)
		if err != nil {
			return err
		}
	}
	if rt.name == "muse" {
		name := ""
		if install {
			name = *agent
		}
		return a.museHookSettings(*settings, name, install, selected)
	}
	path := *settings
	orig, mode, err := readSettings(path)
	if err != nil {
		return err
	}
	if orig == nil && !install {
		a.printf("unchanged %s (no such file)\n", sanitizeLine(path))
		return nil
	}
	root := newJSONObject()
	if orig != nil {
		if root, err = parseJSONObject(orig); err != nil {
			return fmt.Errorf("%s: %v; refusing to change it", sanitizeLine(path), err)
		}
	}
	// Codex hooks have no env: its hooks.json takes only "description" and
	// "hooks", so env is neither read nor written there.
	env := newJSONObject()
	if rt.env {
		if env, err = root.childObject("env"); err != nil {
			return fmt.Errorf("%s: %w", sanitizeLine(path), err)
		}
	}
	var removed []string
	if install {
		if rt.env {
			env.set(config.EnvAgent, marshalNoEscape(*agent))
			root.set("env", env.marshal())
		}
		pin, err := a.pinnedArgs(*agent)
		if err != nil {
			return err
		}
		prefix := shellQuote(selected.path)
		if len(pin) > 0 {
			p, err := a.pathsFor(*agent)
			if err != nil {
				return err
			}
			if _, err = a.namedMetadata(p, *agent); err != nil {
				return err
			}
			prefix = "env " + shellQuote("A2A_CONFIG_DIR="+p.ConfigRoot()) + " " + prefix
			for _, v := range pin {
				prefix += " " + shellQuote(v)
			}
		}
		removed, err = mergeHooks(root, rt, func(arg string) string {
			return prefix + " --as " + *agent + " hook " + arg + " --runtime " + rt.name
		})
		if err != nil {
			return fmt.Errorf("%s: %w", sanitizeLine(path), err)
		}
	} else {
		if removed, err = mergeHooks(root, rt, nil); err != nil {
			return fmt.Errorf("%s: %w", sanitizeLine(path), err)
		}
		if raw, ok := env.get(config.EnvAgent); ok && rt.env {
			var cur string
			if json.Unmarshal(raw, &cur) == nil && contains(removed, cur) {
				env.del(config.EnvAgent)
				if len(env.keys) == 0 {
					root.del("env")
				} else {
					root.set("env", env.marshal())
				}
			}
		}
	}
	if rt.env {
		if err := ownerMergeDenials(root, install); err != nil {
			return err
		}
	}
	// No settings or companion rules are written before the final observation.
	if install {
		if err := selected.check(); err != nil {
			return err
		}
	}
	if !rt.env {
		exe, name := "", ""
		if install {
			exe = selected.path
			name = *agent
		}
		if err := ownerCodexRules(path, exe, name, install); err != nil {
			return err
		}
	}
	var out bytes.Buffer
	if err := json.Compact(&out, root.marshal()); err != nil {
		return err
	}
	if orig != nil {
		var before bytes.Buffer
		if json.Compact(&before, orig) == nil && bytes.Equal(before.Bytes(), out.Bytes()) {
			a.printf("unchanged %s\n", sanitizeLine(path))
			if install {
				a.hookTrustNote(rt, path)
			}
			return nil
		}
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, out.Bytes(), "", "  "); err != nil {
		return err
	}
	pretty.WriteByte('\n')
	if err := writeSettings(path, pretty.Bytes(), mode); err != nil {
		return err
	}
	switch {
	case install && rt.env:
		a.printf("installed %s (%s): env.%s=%s, hooks SessionStart, UserPromptSubmit, Stop\n", sanitizeLine(path), rt.title, config.EnvAgent, *agent)
	case install:
		a.printf("installed %s (%s): hooks SessionStart, UserPromptSubmit, Stop for --as %s\n", sanitizeLine(path), rt.title, *agent)
	default:
		a.printf("removed %d a2a hook entries from %s\n", len(removed), sanitizeLine(path))
	}
	if install {
		a.hookTrustNote(rt, path)
	}
	return nil
}

// hookTrustNote prints the human step a runtime needs before newly written
// hooks run. Codex runs a non-managed hook only after a human trusted its exact
// command (hooks.state.<key>.trusted_hash, written by Codex's TUI); a2a never
// writes that approval itself (research/codex-hooks.md, (d)).
func (a *App) hookTrustNote(rt hookRuntime, path string) {
	if rt.name != "codex" {
		return
	}
	where := path
	if abs, err := filepath.Abs(path); err == nil {
		where = abs
		if filepath.Base(filepath.Dir(abs)) == ".codex" {
			where = filepath.Dir(filepath.Dir(abs))
		}
	}
	a.printf("human step: Codex runs these hooks only after a human trusts them; a2a does not trust them for you.\n")
	a.printf("  1. start codex in %s (answer the project trust prompt with trust: an untrusted project ignores its .codex/);\n", sanitizeLine(where))
	a.printf("  2. at \"Hooks need review\" choose \"Trust all and continue\" (or review them with /hooks).\n")
	a.printf("  Repeat after the command changes (a moved a2a binary makes the hooks untrusted again).\n")
}

// readSettings returns the file content (nil when missing) and its mode (0600
// for a new file). A symlink or a non-regular file is refused.
func readSettings(path string) ([]byte, fs.FileMode, error) {
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0o600, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if st.Mode()&fs.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file; refusing to write through it", sanitizeLine(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	return data, st.Mode().Perm(), nil
}

// writeSettings replaces path atomically (temp file in the same directory,
// synced, renamed) with the given mode.
func writeSettings(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".a2a-settings-*")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	ok = true
	return nil
}
