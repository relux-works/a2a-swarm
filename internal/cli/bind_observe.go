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

// runningInfoStartupWait bounds the State-lock wait for the record made
// inside the consumer's startup control transaction, so a briefly busy
// lock still lets the record commit with the start. A lock held past it
// does not block the start: the loop's per-cycle retry takes over.
const runningInfoStartupWait = 2 * time.Second

// recordRunningInfo makes one non-blocking attempt; see recordRunningInfoBy.
func (a *App) recordRunningInfo(s *session, generation string) bool {
	return a.recordRunningInfoBy(s, generation, time.Now())
}

// recordRunningInfoBy persists the consumer's self-observation under its
// own generation and PID, waiting for the State lock at most until
// deadline. It runs inside the startup control transaction and is retried
// on later cycles until it lands, so an in-place re-exec records the new
// image and revision even when one attempt meets a busy lock; the CPU
// alarm episode fence is preserved either way. It reports whether the
// current image's record is proven committed (written now, or already
// present unchanged under this generation and PID). Best effort beyond
// that: a failed write changes nothing else and never blocks delivery.
func (a *App) recordRunningInfoBy(s *session, generation string, deadline time.Time) bool {
	if s == nil || s.checkConnection() != nil {
		return false
	}
	info := resolveRunningInfo()
	if info.Executable == "" {
		return false
	}
	proven := false
	_, err := a.updateStateBy(s.paths, s.name, deadline, func(st *config.State) error {
		if s.checkConnection() != nil {
			return errObserveUnchanged
		}
		b := st.Terminal
		if b == nil || b.Stopped || b.Generation != generation || b.DaemonPID != os.Getpid() {
			return errObserveUnchanged
		}
		next := config.TerminalObserve{Executable: info.Executable, Revision: info.Revision, OnLaunchAlias: info.OnLaunchAlias, PID: os.Getpid(), Generation: generation}
		if b.Observe != nil {
			next.CPUAlerted, next.CPUAlertAt, next.CPUJobID = b.Observe.CPUAlerted, b.Observe.CPUAlertAt, b.Observe.CPUJobID
			if *b.Observe == next {
				proven = true
				return errObserveUnchanged
			}
		}
		b.Observe = &next
		proven = true
		return nil
	})
	return proven && (err == nil || errors.Is(err, errObserveUnchanged))
}

// runningInfoRecorder retries the running-info record on every cycle until
// it has committed for this consumer's generation and PID, then stops
// touching State for it. One recorder per consumer loop invocation, so an
// in-place re-exec (which re-enters the loop) records afresh.
type runningInfoRecorder struct {
	recorded bool
}

func (r *runningInfoRecorder) ensure(a *App, s *session, generation string) bool {
	if r.recorded {
		return true
	}
	r.recorded = a.recordRunningInfo(s, generation)
	return r.recorded
}

// ensureLoaded is ensure for a cycle that has already loaded the binding:
// when that snapshot already carries this process's record (written by
// the startup transaction or an earlier cycle), it costs no State work
// at all; only a missing or stale record takes the locked write.
func (r *runningInfoRecorder) ensureLoaded(a *App, s *session, generation string, b *config.TerminalBinding) bool {
	if r.recorded {
		return true
	}
	if observedRunningInfo(b, generation) {
		r.recorded = true
		return true
	}
	return r.ensure(a, s, generation)
}

// observedRunningInfo reports whether b already carries this process's
// own running record for generation. The CPU fence fields are the
// episode's, not the image's, so they do not take part in the comparison.
func observedRunningInfo(b *config.TerminalBinding, generation string) bool {
	if b == nil || b.Stopped || b.Generation != generation || b.DaemonPID != os.Getpid() || b.Observe == nil {
		return false
	}
	info := resolveRunningInfo()
	o := b.Observe
	return info.Executable != "" && o.Executable == info.Executable && o.Revision == info.Revision && o.OnLaunchAlias == info.OnLaunchAlias && o.PID == os.Getpid() && o.Generation == generation
}

// runningUnknown is the status claim for a live binding whose consumer
// has no proven record of its own yet. Status prints it verbatim, and
// nothing else may.
const runningUnknown = "unknown (not yet recorded)"

// runningStatus projects the recorded self-observation for bind status.
// Only a record written by the live consumer itself (same PID and
// generation as the binding) is projected: another consumer's record, a
// record that predates the PID/generation stamp, and a corrupt record all
// stay unknown, so no executable, revision or alias claim is invented
// from state the live writer could not have produced. A stopped binding
// makes no running claim at all, not even a PID.
type runningStatus struct {
	PID           int
	Executable    string
	Revision      string
	OnLaunchAlias *bool
	Unknown       bool
}

func projectRunningStatus(b *config.TerminalBinding) runningStatus {
	if b == nil || b.Stopped {
		return runningStatus{}
	}
	o := b.Observe
	if o == nil || o.PID <= 0 || o.PID != b.DaemonPID || o.Generation != b.Generation || !validObservedExecutable(o.Executable) {
		return runningStatus{Unknown: b.DaemonPID > 0}
	}
	out := runningStatus{PID: b.DaemonPID, Executable: o.Executable, Revision: "unknown"}
	if safeRevisionToken(o.Revision) && o.Revision != "" {
		out.Revision = o.Revision
	}
	on := o.OnLaunchAlias
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
