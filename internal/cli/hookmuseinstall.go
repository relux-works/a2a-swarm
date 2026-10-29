package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Phase0 R2 discovered this native config plane. It supports hooks, not an
// env export contract: identity is pinned in each command, never invented env.
const DefaultMuseHooks = ".muse/hooks.json"
const museCommandTimeout = 6 // native seconds; exceeds a2a's five-second budget

type museHookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

func ourMuseHookGroup(raw json.RawMessage, ev hookEvent) (string, bool) {
	var g struct {
		Hooks []museHookCommand `json:"hooks"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&g) != nil || len(g.Hooks) != 1 || g.Hooks[0].Type != "command" || g.Hooks[0].Timeout != museCommandTimeout {
		return "", false
	}
	m := ourHookRE.FindStringSubmatch(g.Hooks[0].Command)
	if m == nil || m[2] != ev.arg || m[3] != "muse" {
		return "", false
	}
	return m[1], true
}

func mergeMuseHooks(root *jsonObject, name, prefix string, install bool) (int, error) {
	hooks, err := root.childObject("hooks")
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, ev := range hookEvents {
		var groups []json.RawMessage
		if raw, ok := hooks.get(ev.name); ok {
			if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '[' || json.Unmarshal(raw, &groups) != nil {
				return 0, fmt.Errorf("hooks.%s is not a JSON array; refusing to change it", ev.name)
			}
		}
		out := []json.RawMessage{}
		placed := false
		for _, g := range groups {
			owner, ours := ourMuseHookGroup(g, ev)
			if !ours {
				out = append(out, g)
				continue
			}
			// --agent explicitly selects a local install, but never silently
			// replaces another identity's already owned project integration.
			if name != "" && name != owner {
				return 0, errors.New("existing Muse hooks belong to another identity; refusing to change them")
			}
			removed++
			if install && !placed {
				out = append(out, museHookGroup(prefix, name, ev))
				placed = true
			}
		}
		if install && !placed {
			out = append(out, museHookGroup(prefix, name, ev))
		}
		if len(out) == 0 {
			hooks.del(ev.name)
		} else {
			hooks.set(ev.name, rawArray(out))
		}
	}
	if len(hooks.keys) == 0 {
		root.del("hooks")
	} else {
		root.set("hooks", hooks.marshal())
	}
	return removed, nil
}

func museHookGroup(prefix, name string, ev hookEvent) json.RawMessage {
	return marshalNoEscape(struct {
		Hooks []museHookCommand `json:"hooks"`
	}{[]museHookCommand{{"command", prefix + " --as " + name + " hook " + ev.arg + " --runtime muse", museCommandTimeout}}})
}

func (a *App) museHookSettings(path, name string, install bool) error {
	if a.as != "" || a.Getenv(config.EnvAgent) != "" {
		selected, err := a.identity()
		if err != nil {
			return err
		}
		if install && selected != name {
			return usagef("this session is %s; --agent %s refused: never act as another identity", selected, name)
		}
		if !install {
			name = selected
		}
	}
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
		// Also reject duplicate keys and trailing data rather than accepting
		// ambiguous configuration with the generic legacy merger.
		if err := uniqueMuseJSON(orig); err != nil {
			return fmt.Errorf("%s: invalid Muse config; refusing to change it", sanitizeLine(path))
		}
		if root, err = parseJSONObject(orig); err != nil {
			return fmt.Errorf("%s: invalid Muse config; refusing to change it", sanitizeLine(path))
		}
	}
	var state config.State
	if install {
		p, err := a.pathsFor(name)
		if err != nil {
			return err
		}
		state, err = config.LoadState(p, name)
		if err != nil {
			return err
		}
	}
	prefix := ""
	if install {
		exe, err := a.executable()
		if err != nil {
			return err
		}
		prefix = shellQuote(exe)
		pin, err := a.pinnedArgs(name)
		if err != nil {
			return err
		}
		if len(pin) > 0 {
			p, err := a.pathsFor(name)
			if err != nil {
				return err
			}
			if _, err = a.namedMetadata(p, name); err != nil {
				return err
			}
			prefix = "env " + shellQuote("A2A_CONFIG_DIR="+p.ConfigRoot()) + " " + prefix
			for _, v := range pin {
				prefix += " " + shellQuote(v)
			}
		}
	}
	removed, err := mergeMuseHooks(root, name, prefix, install)
	if err != nil {
		return err
	}
	out := root.marshal()
	var before, after bytes.Buffer
	_ = json.Compact(&before, orig)
	_ = json.Compact(&after, out)
	if orig != nil && bytes.Equal(before.Bytes(), after.Bytes()) {
		a.printf("unchanged %s\n", sanitizeLine(path))
	} else {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, out, "", "  "); err != nil {
			return err
		}
		pretty.WriteByte('\n')
		if err := writeSettings(path, pretty.Bytes(), mode); err != nil {
			return err
		}
		if install {
			a.printf("installed %s (Muse): hooks SessionStart, UserPromptSubmit, Stop for --as %s\n", sanitizeLine(path), name)
		} else {
			a.printf("removed %d a2a Muse hook entries from %s\n", removed, sanitizeLine(path))
		}
	}
	if install {
		if state.Lease != nil && a.Now().Before(state.Lease.Expires) {
			a.printf("warning: identity lease is held; installation does not acquire, release or transfer it. Use only the owning session.\n")
		}
		if state.HooksPaused {
			a.printf("hooks remain paused; only an explicit a2a hook resume --agent %s resumes them.\n", name)
		}
		a.printf("human step: Muse project hooks run only after native human trust; a2a never writes trust approval.\n")
		a.printf("No project env export is installed. Commands pin --as; genuine shell/hook/inbox session propagation is unqualified. Stop is terminal systemMessage only; idle wake requires separate native acceptance.\n")
	}
	return nil
}

// uniqueMuseJSON rejects duplicate keys recursively, non-object roots and any
// trailing bytes. Unknown keys are preserved, not treated as our configuration.
func uniqueMuseJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value func() error
	value = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				tok, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := tok.(string)
				if !ok || seen[key] {
					return errors.New("duplicate key")
				}
				seen[key] = true
				if err := value(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case json.Delim('['):
			for dec.More() {
				if err := value(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
		return nil
	}
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return errors.New("not an object")
	}
	if err := value(); err != nil {
		return err
	}
	// Decoder.InputOffset ends at the closing delimiter; whitespace only may follow.
	if len(bytes.TrimSpace(data[dec.InputOffset():])) != 0 {
		return errors.New("trailing data")
	}
	return nil
}
