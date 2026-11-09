package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Muse 1.4.14 Phase0 R3 accepts PascalCase context on these two events.
// Stop accepts only a terminal message: this is NOT model receipt or idle wake.
func renderMuseHook(ev hookEvent, text string) any {
	if ev.arg == "stop" {
		return struct {
			SystemMessage string `json:"systemMessage"`
		}{text}
	}
	return codexContextOutput{HookSpecificOutput: codexHookSpecific{HookEventName: ev.name, AdditionalContext: text}}
}

const museHookGap = " a2a hook gap: an earlier check failed. Run `a2a read` to check the room history."

const museInboxApproval = " with per-command approval, outside the default Muse sandbox"

// Diagnostics are local, counts-only metadata. An append-only error journal
// and a successfully surfaced byte offset retain failures across empty hooks,
// pauses, crashes and concurrent checks. They are not mailbox receipts.
type museHookJournal struct {
	root *os.Root
	name string
}

func openMuseHookJournal(name string) (*museHookJournal, error) {
	return openMuseHookJournalAt(".", name)
}

func openMuseHookJournalAt(base, name string) (*museHookJournal, error) {
	if config.ValidateName(name) != nil {
		name = "unselected"
	}
	dir := base
	for _, part := range []string{".temp", "a2a-muse-hooks"} {
		dir = filepath.Join(dir, part)
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		st, err := os.Lstat(dir)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("diagnostic directory is not a regular directory")
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &museHookJournal{root: root, name: name}, nil
}

func (j *museHookJournal) regular(path string) error {
	st, err := j.root.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("diagnostic is not a regular file")
	}
	return nil
}

func (j *museHookJournal) failure(now time.Time, ev hookEvent, class string) error {
	path := j.name + ".jsonl"
	if err := j.regular(path); err != nil {
		return err
	}
	f, err := j.root.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line := struct {
		Timestamp string `json:"timestamp"`
		Event     string `json:"event"`
		Class     string `json:"class"`
	}{now.UTC().Format(time.RFC3339Nano), ev.name, class}
	if err := json.NewEncoder(f).Encode(line); err != nil {
		return err
	}
	return f.Sync()
}

func (j *museHookJournal) pending() (int64, bool, error) {
	if err := j.regular(j.name + ".jsonl"); err != nil {
		return 0, false, err
	}
	st, err := j.root.Stat(j.name + ".jsonl")
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if err := j.regular(j.name + ".seen"); err != nil {
		return 0, false, err
	}
	data, err := j.root.ReadFile(j.name + ".seen")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, false, err
	}
	seen, _ := strconv.ParseInt(string(data), 10, 64)
	return st.Size(), seen != st.Size(), nil
}

func museFailureClass(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	var held *config.LeaseHeldError
	if errors.As(err, &held) {
		return "lease_conflict"
	}
	return "delivery_error"
}

// Muse deliberately bypasses terminal binding, Claude env-file export, owner
// Stop mirroring and runtime liveness claims. Genuine native session ownership
// and shell propagation still require acceptance; absent stdin session identity
// must never silently acquire a PID/ambient lease.
func (a *App) runMuseHook(ctx context.Context, rt hookRuntime, ev hookEvent) {
	j, journalErr := a.museHookJournal()
	if j != nil {
		defer j.root.Close()
	}
	fail := func(class string) {
		if j != nil {
			journalErr = j.failure(a.Now(), ev, class)
		}
		extra := ""
		if journalErr != nil {
			extra = "; project diagnostic unavailable"
		}
		fmt.Fprintf(a.Stderr, "a2a hook %s: %s%s; nothing new was acknowledged\n", ev.arg, class, extra)
	}
	if a.as == "" {
		fail("explicit_identity_required")
		return
	}
	name, err := a.identity() // retain the established --as versus A2A_AGENT guard
	if err != nil {
		fail("identity_refused")
		return
	}
	p, err := a.pathsFor(name)
	if err != nil {
		fail("identity_refused")
		return
	}
	budget := hookBudget
	if a.hookBudget > 0 {
		budget = a.hookBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	prior := a.ctx
	a.ctx = ctx
	defer func() { a.ctx = prior }()
	err = func() error {
		st, err := config.LoadState(p, name)
		if err != nil {
			return err
		}
		if st.HostEnrollment != nil {
			return nil
		}
		in, err := readHookInput(a.Stdin)
		if err != nil || len(bytes.TrimSpace(in.Payload)) == 0 || bytes.TrimSpace(in.Payload)[0] != '{' {
			fail("invalid_input")
			return nil
		}
		if in.SessionID == "" || !claudeSessionRE.MatchString(in.SessionID) || (in.HookEventName != "" && in.HookEventName != ev.name) {
			fail("invalid_input")
			return nil
		}
		if err := a.recordHookOwner(ctx, rt, ev, in); err != nil {
			return err
		}
		if st.HooksPaused || ev.arg == "session-end" || ev.arg == "stop" && in.StopReason == "exit" {
			return nil
		}
		if journalErr != nil {
			return errors.New("project diagnostic unavailable")
		}
		offset, gap, err := j.pending()
		if err != nil {
			return err
		}
		rendered := false
		rt.render = func(ev hookEvent, text string) any {
			rendered = true
			if gap {
				text += museHookGap
			}
			return renderMuseHook(ev, text)
		}
		blocked := false
		if err = a.hookDeliver(ctx, rt, ev, in, &blocked); err != nil {
			return err
		}
		if gap && rendered {
			// Only a successful write AND durable rung/notification completion
			// surfaces this offset. Later failures append past it and stay pending.
			return j.root.WriteFile(j.name+".seen", []byte(strconv.FormatInt(offset, 10)), 0o600)
		}
		return nil
	}()
	if err != nil {
		fail(museFailureClass(err))
	}
}

// Courier hook journals share the resolved identity's store, never daemon cwd.
func (a *App) museHookJournal() (*museHookJournal, error) {
	if !a.courierEngine {
		return openMuseHookJournal(a.as)
	}
	p, err := a.paths()
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(p.Base, 0700); err != nil {
		return nil, err
	}
	return openMuseHookJournalAt(p.Base, a.as)
}
