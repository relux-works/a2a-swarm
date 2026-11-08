package cli

import (
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Exact strict ancestors of 5f2b843, generated with:
// git rev-list 5f2b843^ | sort
// Commit ids are identities, never ordered versions. Unlisted revisions,
// missing build metadata and modified builds are unknown, not known old.
//
//go:embed state_writer_ancestors.txt
var opaqueUnawareRevisions string

func opaqueUnawareBuild(info *debug.BuildInfo) bool {
	if info == nil || info.Path != "github.com/relux-works/a2a-swarm/cmd/a2a" || info.Main.Path != "github.com/relux-works/a2a-swarm" {
		return false
	}
	revision, modified := "", ""
	revisionCount, modifiedCount := 0, 0
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
			revisionCount++
		case "vcs.modified":
			modified = setting.Value
			modifiedCount++
		}
	}
	return revisionCount == 1 && modifiedCount == 1 && modified == "false" && len(revision) == 40 && strings.Contains("\n"+opaqueUnawareRevisions, "\n"+revision+"\n")
}

const oldStateWriterWarning = "libexec image predates 5f2b843; older State writers drop newer fields; upgrade this image"

type stateWriterWarning struct {
	Source string `json:"source"`
	Detail string `json:"detail"`
}

// Match only installer-generated literal commands; never ask a shell to
// interpret settings. The full existing matcher checks identity and grammar.
var hookImageRE = regexp.MustCompile(`^(?:env (?:'(?:[^']|'\\'')*'|[^\s']+) )?('(?:[^']|'\\'')*'|[^\s']+)`)

func stateWriterHookPath(command, name string) string {
	m := ourHookRE.FindStringSubmatch(command)
	if m == nil || m[1] != name {
		return ""
	}
	word := hookImageRE.FindStringSubmatch(command)
	if len(word) != 2 {
		return ""
	}
	path := word[1]
	if strings.HasPrefix(path, "'") {
		path = strings.ReplaceAll(path[1:len(path)-1], "'\\''", "'")
	} else if !shellSafeRE.MatchString(path) {
		return ""
	}
	if !filepath.IsAbs(path) {
		return ""
	}
	return path
}

func stateWriterHookPaths(data []byte, name string) []string {
	// Reuse strict duplicate-key/trailing-data rejection. Settings are input,
	// not authority to run a command or inspect another identity's image.
	if len(data) > 1<<20 || uniqueMuseJSON(data) != nil {
		return nil
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []hookCommand `json:"hooks"`
		} `json:"hooks"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return nil
	}
	var paths []string
	for _, ev := range hookEvents {
		for _, group := range settings.Hooks[ev.name] {
			for _, hook := range group.Hooks {
				if hook.Type == "command" {
					if path := stateWriterHookPath(hook.Command, name); path != "" {
						paths = append(paths, path)
					}
				}
			}
		}
	}
	return paths
}

func (s *session) stateWriterWarnings(ctx context.Context, st config.State) []stateWriterWarning {
	var warnings []stateWriterWarning
	for _, rt := range hookRuntimes {
		// This mirrors hook install's default project settings. Custom settings
		// elsewhere cannot be discovered and are deliberately not searched.
		data := readStateWriterSettings(rt.settings)
		for _, path := range stateWriterHookPaths(data, s.name) {
			if oldLibexecStateWriter(path, "") {
				warnings = append(warnings, stateWriterWarning{"hook:" + rt.name, oldStateWriterWarning})
				break
			}
		}
	}
	if stateWriterConsumerWarning(ctx, s.name, s.paths.ConfigRoot(), st.Terminal, stateWriterProcessInspection{bindingProcessBirth, bindingArguments, bindingImage, oldLoadedLibexecStateWriter}) {
		warnings = append(warnings, stateWriterWarning{"consumer", oldStateWriterWarning})
	}
	return warnings
}

type stateWriterProcessInspection struct {
	birth     func(context.Context, int) (string, error)
	arguments func(int) ([]string, []string, error)
	image     func(context.Context, int) (string, error)
	loaded    func(context.Context, int, string) bool
}

func stateWriterConsumerWarning(ctx context.Context, name, root string, b *config.TerminalBinding, inspect stateWriterProcessInspection) bool {
	if b == nil || b.Stopped || b.Host != machineLabel() || b.DaemonPID <= 0 || b.DaemonBirth == "" {
		return false
	}
	probe, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
	defer cancel()
	birth, err := inspect.birth(probe, b.DaemonPID)
	if err != nil || birth != b.DaemonBirth {
		return false
	}
	args, env, err := inspect.arguments(b.DaemonPID)
	if err != nil || len(args) == 0 {
		return false
	}
	selected, selectedRoot, command := bindingSelectors(args, env)
	if selected != name || command != "bind" || !sameStateWriterRoot(selectedRoot, root) {
		return false
	}
	image, err := inspect.image(probe, b.DaemonPID)
	if err == nil && image != "" && inspect.loaded(probe, b.DaemonPID, image) {
		// Recheck process identity after inspecting its selected image.
		if after, err := inspect.birth(probe, b.DaemonPID); err == nil && after == birth {
			afterImage, err := inspect.image(probe, b.DaemonPID)
			return err == nil && afterImage == image
		}
	}
	return false
}

func sameStateWriterRoot(a, b string) bool {
	if !filepath.IsAbs(a) || !filepath.IsAbs(b) {
		return false
	}
	a, ea := filepath.EvalSymlinks(a)
	b, eb := filepath.EvalSymlinks(b)
	return ea == nil && eb == nil && a == b
}

// Bound settings reads before decoding; never follow a settings-file symlink.
func readStateWriterSettings(path string) []byte {
	root, name, err := openOutputParent(path)
	if err != nil {
		return nil
	}
	defer root.Close()
	f, err := openSavedInboxFile(root, name)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil
	}
	return data
}
