package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// The tmux wake adapter (`a2a bridge tmux`): for an agent session running in a
// tmux pane that sits idle at an empty prompt, it types one short line that
// makes the session take a turn, so the prompt hook delivers the events.
// It only peeks: a read-only /sync from the committed cursor; it never writes
// the delivery state, the acknowledged ring, or subscriptions. It never puts
// server text into the pane: the line holds only counts and relevance types.

// tmuxCallTimeout bounds one tmux invocation.
const tmuxCallTimeout = 5 * time.Second

// tmuxRunner runs tmux with arguments (no shell) and returns its stdout.
type tmuxRunner interface {
	run(ctx context.Context, args ...string) (string, error)
}

// execTmux is the real tmux binary.
type execTmux struct{}

func (execTmux) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, tmuxCallTimeout)
	defer cancel()
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "tmux", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tmux %s: %v: %s", args[0], err, snippet(errb.String(), 200))
	}
	return out.String(), nil
}

func (a *App) tmuxRunner() tmuxRunner {
	if a.tmux != nil {
		return a.tmux
	}
	return execTmux{}
}

// tmuxBridge is the state of one running adapter; it lives in memory only.
type tmuxBridge struct {
	a        *App
	s        *session
	tmux     tmuxRunner
	pane     string
	program  string
	prompt   *regexp.Regexp
	idle     time.Duration
	interval time.Duration

	lastSet  string    // the event set rung last
	lastRing time.Time // when
}

const bridgeUsage = "usage: a2a bridge tmux --agent NAME --pane TARGET --program P --empty-prompt REGEXP [--idle 60s] [--interval 30s] [--poll 10s] [--once]"

// cmdBridge: a2a bridge tmux ...
func cmdBridge(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 || args[0] != "tmux" {
		return usagef("%s", bridgeUsage)
	}
	fs := newFlagSet("bridge tmux")
	agent := fs.String("agent", "", "agent whose deliveries ring the pane (required)")
	pane := fs.String("pane", "", "tmux target pane, e.g. work:1.0 (required)")
	program := fs.String("program", "", "expected #{pane_current_command}, e.g. claude or node (required)")
	emptyPrompt := fs.String("empty-prompt", "", "multiline regexp over the whole captured pane that matches only when the input prompt is empty (required; calibrate with --probe)")
	probe := fs.Bool("probe", false, "print the idle checks for the pane and exit, without peeking or ringing")
	idle := fs.Duration("idle", 60*time.Second, "no tmux client activity on the session for this long")
	interval := fs.Duration("interval", 30*time.Second, "at most one ring per interval")
	poll := fs.Duration("poll", 10*time.Second, "how often to check")
	once := fs.Bool("once", false, "check once and exit")
	pos, err := parseInterspersed(fs, args[1:])
	if err != nil {
		return err
	}
	switch {
	case len(pos) != 0:
		return usagef("%s", bridgeUsage)
	case *agent == "", *pane == "", *program == "", *emptyPrompt == "":
		return usagef("--agent, --pane, --program and --empty-prompt are required (no defaults); %s", bridgeUsage)
	case *idle <= 0 || *interval <= 0 || *poll <= 0:
		return usagef("--idle, --interval and --poll must be positive")
	}
	if err := config.ValidateName(*agent); err != nil {
		return usageError{err.Error()}
	}
	if a.as != "" && a.as != *agent {
		return usagef("--as %s and --agent %s disagree", a.as, *agent)
	}
	re, err := regexp.Compile("(?m)" + *emptyPrompt)
	if err != nil {
		return usagef("--empty-prompt: %v", err)
	}
	if *probe {
		br := &tmuxBridge{a: a, tmux: a.tmuxRunner(), pane: *pane, program: *program, prompt: re, idle: *idle, interval: *interval}
		idleNow, why, err := br.paneState(ctx, a.Now())
		if err != nil {
			return err
		}
		a.printf("pane %s: idle=%t (%s)\n", sanitizeLine(*pane), idleNow, why)
		return nil
	}
	a.as = *agent
	s, err := a.session()
	if err != nil {
		return err
	}
	br := &tmuxBridge{a: a, s: s, tmux: a.tmuxRunner(), pane: *pane, program: *program, prompt: re, idle: *idle, interval: *interval}
	for {
		err := br.check(ctx)
		if *once {
			return err
		}
		if err != nil && ctx.Err() == nil {
			fmt.Fprintln(a.Stderr, "a2a bridge: "+a.secrets.Scrub(sanitizeLine(err.Error())))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*poll):
		}
	}
}

// check peeks once and rings the pane when there is a new set of undelivered
// events and the pane is idle at an empty prompt.
func (br *tmuxBridge) check(ctx context.Context) error {
	st, err := config.LoadState(br.s.paths, br.s.name) // read only
	if err != nil {
		return err
	}
	if st.NextBatch == "" {
		return errors.New("no delivery cursor yet; the first hook or `a2a wait` takes the position")
	}
	list, err := br.s.getSubs(ctx) // read only: no legacy migration, no pruning
	if err != nil {
		return err
	}
	now := br.a.Now()
	var subs []Sub
	for _, sub := range list.Subs {
		if !sub.expiredAt(now) {
			subs = append(subs, sub)
		}
	}
	b, _, err := br.s.collectNow(ctx, &st, subs)
	if err != nil {
		return err
	}
	var ids, types []string
	for _, it := range b.items {
		id := it.id
		if id == "" {
			id = "invite:" + it.rec.RoomID
		}
		ids = append(ids, id)
		if !contains(types, it.rec.Type) {
			types = append(types, it.rec.Type)
		}
	}
	if len(ids) == 0 {
		br.lastSet = ""
		return nil
	}
	sort.Strings(ids)
	set := strings.Join(ids, ",")
	if set == br.lastSet || (!br.lastRing.IsZero() && now.Sub(br.lastRing) < br.interval) {
		return nil
	}
	if ok, err := br.paneIdle(ctx, now); err != nil || !ok {
		return err
	}
	line := ringLine(len(ids), types)
	if _, err := br.tmux.run(ctx, "send-keys", "-t", br.pane, "-l", line); err != nil {
		return err
	}
	if _, err := br.tmux.run(ctx, "send-keys", "-t", br.pane, "Enter"); err != nil {
		return err
	}
	br.lastSet, br.lastRing = set, now
	br.a.printf("rang %s: %s\n", sanitizeLine(br.pane), line)
	return nil
}

// ringLine is the only text the adapter ever types: counts and relevance
// types (our own constants), never server text.
func ringLine(n int, types []string) string {
	noun := "events"
	if n == 1 {
		noun = "event"
	}
	return fmt.Sprintf("a2a: %d new peer %s (%s). They are delivered by the prompt hook.", n, noun, strings.Join(types, ", "))
}

// paneIdle: the expected program runs in the pane, no tmux client touched the
// session for br.idle, and the empty-prompt regexp matches the captured screen
// (multiline, trailing blanks trimmed per line), so a pane where a human is
// typing is never touched. The regexp is matched against the whole screen
// because Claude Code draws a footer under its input box.
func (br *tmuxBridge) paneIdle(ctx context.Context, now time.Time) (bool, error) {
	idle, _, err := br.paneState(ctx, now)
	return idle, err
}

// paneState is paneIdle plus the reason, for --probe.
func (br *tmuxBridge) paneState(ctx context.Context, now time.Time) (bool, string, error) {
	out, err := br.tmux.run(ctx, "display-message", "-p", "-t", br.pane, "#{pane_current_command}\t#{session_activity}\t#{session_name}")
	if err != nil {
		return false, "", err
	}
	f := strings.Split(strings.TrimRight(out, "\r\n"), "\t")
	if len(f) != 3 {
		return false, "", fmt.Errorf("tmux display-message: unexpected output %q", snippet(out, 80))
	}
	if f[0] != br.program {
		return false, "program is " + sanitizeLine(f[0]) + ", not " + br.program, nil
	}
	latest, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return false, "", fmt.Errorf("tmux session_activity %q is not a unix time", snippet(f[1], 40))
	}
	clients, err := br.tmux.run(ctx, "list-clients", "-t", f[2], "-F", "#{client_activity}")
	if err != nil {
		return false, "", err
	}
	for _, l := range strings.Fields(clients) {
		if t, err := strconv.ParseInt(l, 10, 64); err == nil && t > latest {
			latest = t
		}
	}
	if age := now.Sub(time.Unix(latest, 0)); age < br.idle {
		return false, fmt.Sprintf("last activity %s ago < %s", age.Round(time.Second), br.idle), nil
	}
	screen, err := br.tmux.run(ctx, "capture-pane", "-p", "-t", br.pane)
	if err != nil {
		return false, "", err
	}
	lines := strings.Split(screen, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t\r")
	}
	if !br.prompt.MatchString(strings.Join(lines, "\n")) {
		return false, "empty-prompt regexp does not match the screen (prompt not empty, or regexp needs calibration)", nil
	}
	return true, "program, activity and empty prompt all match", nil
}
