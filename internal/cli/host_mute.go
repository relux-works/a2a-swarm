package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/wakemute"
)

const hostMuteUsage = "usage: a2a host mute [enroll|status] --scope FILE --budget DUR [--run RUN] | a2a host unmute --scope FILE --budget DUR --run RUN --token OPERATION_ID | a2a host mute capability"

type wakeScopeFile struct {
	Version int `json:"version"`
	Roots   []struct {
		Root        string   `json:"root"`
		Connections []string `json:"connections"`
	} `json:"roots"`
}

// This entry point is deliberately independent of ambient identity, station
// discovery, owner pins, Matrix routing environment and all credential stores.
func hostMuteInvocation(args []string) bool {
	return len(args) >= 2 && args[0] == "host" && (args[1] == "mute" || args[1] == "unmute")
}
func loadWakeScope(file string) (wakemute.Scope, error) {
	if file == "" {
		return nil, usagef("%s", hostMuteUsage)
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > wakemute.MaxBytes {
		return nil, errors.New("scope must be a bounded regular JSON file")
	}
	b, err := io.ReadAll(io.LimitReader(f, wakemute.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	var in wakeScopeFile
	if err = config.DecodeConnectionJSON(b, &in); err != nil || in.Version != wakemute.Version {
		return nil, wakemute.ErrUnsupported
	}
	var scope wakemute.Scope
	for _, r := range in.Roots {
		if !filepath.IsAbs(r.Root) {
			return nil, wakemute.ErrAuthority
		}
		root, err := filepath.EvalSymlinks(r.Root)
		if err != nil {
			return nil, err
		}
		// Only public connection metadata is read. Explicit legacy selection is "".
		registry, err := config.LoadConnections(config.Paths{Base: root})
		if err != nil {
			return nil, err
		}
		selection := wakemute.Selection{Root: root}
		for _, name := range r.Connections {
			path := "."
			if name != "" {
				if registry == nil {
					return nil, wakemute.ErrUnsupported
				}
				c, ok := registry.Profiles[name]
				if !ok {
					return nil, wakemute.ErrUnsupported
				}
				path = filepath.Join("connections", c.ID)
			}
			selection.Connections = append(selection.Connections, wakemute.Connection{Name: name, Path: path})
		}
		scope = append(scope, selection)
	}
	return wakemute.Normalize(scope)
}
func cmdHostMute(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 || a.as != "" || a.profile != "" || a.human {
		return usagef("%s", hostMuteUsage)
	}
	if len(args) == 2 && args[1] == "--help" {
		_, err := fmt.Fprintln(a.Stdout, hostMuteUsage+"\nScope and budget are required; mute/enroll require actual stdin/stdout TTYs, no TMUX/STY, and a fresh bound confirmation. Operation IDs are public, not bearer authority. Muted is not quiesced. See docs/host-mute.md.")
		return err
	}
	action := args[0]
	args = args[1:]
	if action == "mute" && len(args) > 0 {
		switch args[0] {
		case "capability":
			if len(args) != 1 {
				return usagef("%s", hostMuteUsage)
			}
			status := "scope-capability"
			if !wakemute.Supported {
				status = "unsupported"
			}
			return json.NewEncoder(a.Stdout).Encode(struct {
				Version            int      `json:"version"`
				Status             string   `json:"status"`
				Quiesced           bool     `json:"quiesced"`
				EnrollmentRequired bool     `json:"enrollment_required"`
				CoverageLimits     []string `json:"coverage_limits"`
			}{Version: wakemute.Version, Status: status, EnrollmentRequired: true, CoverageLimits: wakemute.Limits})
		case "enroll", "status":
			action = args[0]
			args = args[1:]
		}
	}
	fs := newFlagSet("host wake mute")
	file := fs.String("scope", "", "explicit finite roots and named connections JSON")
	run := fs.String("run", "", "exact approved maintenance run reference")
	token := fs.String("token", "", "public operation ID; not bearer authority")
	budget := fs.Duration("budget", 0, "required finite lock and I/O budget (1ms..5m)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *budget < time.Millisecond || *budget > 5*time.Minute || action != "status" && *run == "" || action == "unmute" && *token == "" || action != "unmute" && *token != "" || action != "mute" && action != "unmute" && action != "status" && action != "enroll" {
		return usagef("%s", hostMuteUsage)
	}
	ctx, cancel := context.WithTimeout(ctx, *budget)
	defer cancel()
	scope, err := loadWakeScope(*file)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	var result wakemute.Result
	switch action {
	case "status":
		result, err = wakemute.Status(ctx, scope)
	case "unmute":
		result, err = wakemute.Unmute(ctx, scope, *run, *token)
	default:
		operation, e := wakemute.NewOperation(scope, *run)
		if e != nil {
			return e
		}
		in, inOK := a.Stdin.(*os.File)
		out, outOK := a.Stdout.(*os.File)
		if !inOK || !outOK {
			return wakemute.ErrAuthority
		}
		operation, e = wakemute.Confirm(ctx, in, out, operation, action)
		if e != nil {
			return e
		}
		if action == "enroll" {
			err = wakemute.Enroll(ctx, operation)
			result = wakemute.Result{Version: wakemute.Version, Status: "enrolled", Scope: scope, CoverageLimits: wakemute.Limits}
			if err != nil {
				result.Status = "uncertain"
			}
		} else {
			result, err = wakemute.Mute(ctx, operation)
		}
	}
	// A lost/failed result is not rollback. The durable public run remains
	// discoverable through a later read-only status invocation.
	encoded, e := json.Marshal(result)
	if e != nil {
		return e
	}
	if _, e = wakemute.WriteOutput(ctx, a.Stdout, append(encoded, '\n')); e != nil {
		if action == "status" {
			return fmt.Errorf("status output failed: %w", e)
		}
		return fmt.Errorf("%w: result output failed", wakemute.ErrUncertain)
	}
	return err
}

// Namespace is derived from the actual selected Paths, never an ambient
// --profile label. Named identity state roots stay under the original root.
func wakeNamespace(p config.Paths) (string, error) {
	if p.Root == "" {
		return ".", nil
	}
	base := p.MetadataBase
	if base == "" {
		return "", wakemute.ErrAuthority
	}
	rel, err := filepath.Rel(p.ConfigRoot(), base)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}
func (a *App) withWakeAdmission(ctx context.Context, p config.Paths, effect func(uint64) error) error {
	namespace, err := wakeNamespace(p)
	if err != nil {
		return err
	}
	required := ""
	if p.WakeMuteVersion != 0 {
		return wakemute.AdmissionRequired(ctx, p.ConfigRoot(), namespace, p.WakeMuteVersion, effect)
	}
	if a.Getenv != nil {
		required = a.Getenv(wakemute.EnvRequiredVersion)
	}
	if required != "" {
		if required != "1" {
			return wakemute.ErrUnsupported
		}
		return wakemute.AdmissionRequired(ctx, p.ConfigRoot(), namespace, wakemute.Version, effect)
	}
	return wakemute.Admission(ctx, p.ConfigRoot(), namespace, effect)
}

// A pulse is detected from the durable root generation even when the consumer
// never sampled the mute. Invalidate observations only, retaining owned notices
// and attempts. Old uncertain drafts require explicit intervention, never an
// automatic retry/Enter under observations from before the pulse or restart.
func (a *App) requalifyWakeScope(s *session, generation string, epoch uint64) (bool, error) {
	st, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return false, err
	}
	b := st.Terminal
	if b == nil || b.Generation != generation {
		return false, errors.New("binding retired")
	}
	if b.WakeMuteGeneration == epoch {
		return b.WakeMuteHold, nil
	}
	_, err = a.updateState(s.paths, s.name, func(current *config.State) error {
		b := current.Terminal
		if b == nil || b.Generation != generation {
			return errors.New("binding retired")
		}
		if b.WakeMuteGeneration != epoch {
			b.WakeMuteGeneration = epoch
			b.Idle = nil
			b.Readiness = nil
			b.WakeMuteHold = b.WakeMuteHold || len(b.AttemptIDs) > 0 || b.NoticeOrigin != nil || b.TypingStarted
			b.Outcome = "suppressed: scope generation changed; fresh readiness required"
		}
		return nil
	})
	return true, err // Next cycle obtains the first fresh observation after thaw.
}
