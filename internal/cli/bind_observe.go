package cli

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// The fixed line for a consumer a new install cannot reach by retargeting
// the launch alias. Status prints it verbatim, and nothing else may.
const notOnLaunchAliasLine = "not on the launch alias: a new install will not reach this binder"

// runningInfo is what the consumer process reports about itself: the image
// it actually runs and that image's build revision. Both come from the
// process itself (its own executable resolution and its loaded build info),
// never from reading the launch alias or any other file's build info: an
// alias may already have been retargeted to a newer image than the one
// this process still runs.
type runningInfo struct {
	Executable    string
	Revision      string
	OnLaunchAlias bool
}

// Seams let tests substitute the self-observation inputs; production code
// never changes them.
var (
	runningExecutablePath = selfExecutablePath
	runningBuildInfo      = debug.ReadBuildInfo
	runningArgv0          = func() string {
		if len(os.Args) > 0 {
			return os.Args[0]
		}
		return ""
	}
)

// selfExecutablePath resolves the running image through the process itself.
// EvalSymlinks only canonicalizes that own path; it does not adopt the
// launch alias spelling, whose target may have changed since exec.
func selfExecutablePath() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	if path, err = filepath.Abs(path); err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, nil
	}
	return path, nil
}

// runningRevision extracts the vcs.revision of the loaded image. A modified
// tree is marked; missing or malformed metadata is unknown, never guessed
// from another file.
func runningRevision(info *debug.BuildInfo) string {
	if info == nil {
		return "unknown"
	}
	revision, modified := "", false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return "unknown"
	}
	if modified {
		revision += "+modified"
	}
	if !safeRevisionToken(revision) {
		return "unknown"
	}
	return revision
}

var revisionTokenRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

// safeRevisionToken bounds a revision to one printable token, so stored
// state can never project arbitrary text into status or an alarm body.
func safeRevisionToken(revision string) bool {
	return revision == "unknown" || revisionTokenRE.MatchString(revision)
}

// onLaunchAlias reports whether the running image is the target of an
// admitted launch alias: argv0 is a custody-admitted symlink alias of the
// running image. A direct-image launch (argv0 is the image itself), a bare
// or relative name, and an alias that fails custody are all off the alias,
// so retargeting an installed alias cannot reach such a consumer.
func onLaunchAlias(argv0, running string) bool {
	alias, ok := admittedLaunchAlias(argv0, running)
	if !ok || alias == running {
		return false
	}
	info, err := os.Lstat(alias)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

func resolveRunningInfo() runningInfo {
	executable, err := runningExecutablePath()
	if err != nil {
		executable = ""
	}
	info, _ := runningBuildInfo()
	revision := runningRevision(info)
	return runningInfo{
		Executable:    executable,
		Revision:      revision,
		OnLaunchAlias: executable != "" && onLaunchAlias(runningArgv0(), executable),
	}
}

var errObserveUnchanged = errors.New("running observation unchanged")

// recordRunningInfo persists the consumer's self-observation under its own
// generation and PID. It runs at consumer start, so an in-place re-exec
// records the new image and revision while the CPU alarm episode fence is
// preserved. Best effort: a failed or unchanged write changes nothing else
// and is retried by a later start, never blocking delivery.
func (a *App) recordRunningInfo(s *session, generation string) {
	if s == nil || s.checkConnection() != nil {
		return
	}
	info := resolveRunningInfo()
	if info.Executable == "" {
		return
	}
	_, _ = a.updateStateBy(s.paths, s.name, time.Now(), func(st *config.State) error {
		if s.checkConnection() != nil {
			return errObserveUnchanged
		}
		b := st.Terminal
		if b == nil || b.Stopped || b.Generation != generation || b.DaemonPID != os.Getpid() {
			return errObserveUnchanged
		}
		next := config.TerminalObserve{Executable: info.Executable, Revision: info.Revision, OnLaunchAlias: info.OnLaunchAlias}
		if b.Observe != nil {
			next.CPUAlerted, next.CPUAlertAt, next.CPUJobID = b.Observe.CPUAlerted, b.Observe.CPUAlertAt, b.Observe.CPUJobID
			if *b.Observe == next {
				return errObserveUnchanged
			}
		}
		b.Observe = &next
		return nil
	})
}

// runningStatus projects the recorded self-observation for bind status.
// Unknown (a consumer that predates the record, or a corrupt record) stays
// unknown: no executable, revision or alias claim is invented from state
// the writer could not have produced.
type runningStatus struct {
	PID           int
	Executable    string
	Revision      string
	OnLaunchAlias *bool
}

func projectRunningStatus(b *config.TerminalBinding) runningStatus {
	if b == nil {
		return runningStatus{}
	}
	out := runningStatus{PID: b.DaemonPID}
	if b.Stopped || b.Observe == nil || !validObservedExecutable(b.Observe.Executable) {
		return out
	}
	out.Executable = b.Observe.Executable
	out.Revision = "unknown"
	if safeRevisionToken(b.Observe.Revision) && b.Observe.Revision != "" {
		out.Revision = b.Observe.Revision
	}
	on := b.Observe.OnLaunchAlias
	out.OnLaunchAlias = &on
	return out
}

func validObservedExecutable(path string) bool {
	if !filepath.IsAbs(path) || len(path) > 4096 {
		return false
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
