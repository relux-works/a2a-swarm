//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"time"

	"github.com/relux-works/a2a-swarm/internal/daemon"
	"github.com/relux-works/a2a-swarm/internal/daemonsvc"
)

const serviceUsage = "usage: a2a daemon service install|uninstall|status --plan FILE; daemon prepare-run-dir --image-sha256 SHA256; daemon sweep|rescue|soak --home ABS --owner-pin FILE --owner-pin-sha256 SHA256 --image-sha256 SHA256 --service-gid GID"

// These commands are local only (excluded from daemon.Authorize). Service
// management never loads station credentials or emits supervisor raw output.
func cmdDaemonService(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 {
		return usagef("%s", serviceUsage)
	}
	op := args[0]
	rest := args[1:]
	if op == "service" {
		if len(rest) == 0 {
			return usagef("%s", serviceUsage)
		}
		action := rest[0]
		fs := flag.NewFlagSet("service", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		planPath := fs.String("plan", "", "reviewed public plan")
		if fs.Parse(rest[1:]) != nil || fs.NArg() != 0 || *planPath == "" || (action != "install" && action != "uninstall" && action != "status") {
			return usagef("%s", serviceUsage)
		}
		// Root is required even for status: no endpoint disclosure or sudo fallback.
		if os.Geteuid() != 0 {
			return daemonsvc.ErrRefused
		}
		p, e := daemonsvc.ReadPlan(*planPath)
		if e != nil || p.Platform != runtime.GOOS {
			return daemonsvc.ErrRefused
		}
		if action == "install" {
			if verifyServiceAccount(p.User(), p.UID, p.GID) != nil || daemonsvc.VerifyImage(p.Image, p.ImageSHA256) != nil {
				return daemonsvc.ErrRefused
			}
			if _, e = daemonsvc.VerifyPolicy(p.Policy, p.PolicySHA256, p.GID); e != nil {
				return e
			}
		}
		states, e := daemonsvc.NewManager(daemonsvc.NativeSupervisor()).Run(ctx, p, action)
		if e != nil {
			return e
		}
		return json.NewEncoder(a.Stdout).Encode(struct {
			States map[string]daemonsvc.State `json:"states"`
		}{states})
	}
	fs := flag.NewFlagSet(op, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	home := fs.String("home", "", "daemon home")
	pin := fs.String("owner-pin", "", "immutable policy")
	pinHash := fs.String("owner-pin-sha256", "", "policy hash")
	imageHash := fs.String("image-sha256", "", "image hash")
	gid := fs.Uint("service-gid", 0, "service group")
	var identity, layer, from, soakPlan string
	var count int
	if op == "rescue" {
		fs.StringVar(&identity, "identity", "", "local identity")
		fs.StringVar(&layer, "layer", "", "sweep|watchdog|manual|traffic")
		fs.IntVar(&count, "count", 0, "actual recovered deliveries or traffic")
	}
	if op == "soak" {
		fs.StringVar(&from, "from", "", "RFC3339 start")
		fs.StringVar(&soakPlan, "plan", "", "root-owned plan selecting soak days")
	}
	if fs.Parse(rest) != nil || fs.NArg() != 0 {
		return usagef("%s", serviceUsage)
	}
	if op == "prepare-run-dir" {
		if *home != "" || *pin != "" || *pinHash != "" || *gid != 0 || runtime.GOOS != "darwin" || os.Geteuid() != 0 {
			return daemonsvc.ErrRefused
		}
		if verifySelfImage(*imageHash) != nil || verifyServiceAccount("_a2a", 401, 401) != nil {
			return daemonsvc.ErrRefused
		}
		return daemonsvc.PrepareRunDir("/private/var/run/a2a", 401, 401)
	}
	if op != "sweep" && op != "rescue" && op != "soak" {
		return usagef("%s", serviceUsage)
	}
	if currentServiceAccount(*gid) != nil || verifySelfImage(*imageHash) != nil {
		return daemonsvc.ErrRefused
	}
	policy, e := daemonsvc.VerifyPolicy(*pin, *pinHash, uint32(*gid))
	if e != nil {
		return e
	}
	now := a.Now()
	if op == "sweep" {
		return daemonsvc.WriteReport(*home, "sweep-report.json", daemonsvc.Sweep(*home, policy, now))
	}
	if op == "rescue" {
		return daemonsvc.RecordRescue(*home, policy, daemonsvc.RescueRecord{At: now, Identity: identity, Layer: layer, Count: count})
	}
	start, e := time.Parse(time.RFC3339, from)
	if e != nil {
		return daemonsvc.ErrRefused
	}
	p, e := daemonsvc.ReadPlan(soakPlan)
	if e != nil || p.Platform != runtime.GOOS || p.Home != *home || p.Policy != *pin || p.PolicySHA256 != *pinHash || p.ImageSHA256 != *imageHash || p.UID != uint32(os.Geteuid()) || p.GID != uint32(*gid) {
		return daemonsvc.ErrRefused
	}
	r, e := daemonsvc.Soak(*home, policy, start, now, p.SoakDays)
	if e != nil {
		return e
	}
	return daemonsvc.WriteReport(*home, "soak-report.json", r)
}
func verifyServiceAccount(name string, uid, gid uint32) error {
	u, e := user.Lookup(name)
	if e != nil || u.Uid != strconv.FormatUint(uint64(uid), 10) || u.Gid != strconv.FormatUint(uint64(gid), 10) {
		return daemonsvc.ErrRefused
	}
	return nil
}
func currentServiceAccount(gid uint) error {
	if os.Geteuid() == 0 || gid == 0 || uint64(gid) > uint64(^uint32(0)) || uint32(os.Getegid()) != uint32(gid) {
		return daemonsvc.ErrRefused
	}
	name := "a2a"
	if runtime.GOOS == "darwin" {
		name = "_a2a"
		if os.Geteuid() != 401 || gid != 401 {
			return daemonsvc.ErrRefused
		}
	}
	return verifyServiceAccount(name, uint32(os.Geteuid()), uint32(gid))
}
func verifySelfImage(hash string) error {
	image, e := os.Executable()
	if e != nil {
		return daemonsvc.ErrRefused
	}
	return daemonsvc.VerifyImage(image, hash)
}

func (a *App) serveDaemonService(ctx context.Context, home, socket, pin, pinHash, imageHash string, gid uint) error {
	// Service startup is always policy-pinned. Internal C1/C5 Serve APIs remain
	// fixture/library seams, and are not reachable through the public CLI.
	if currentServiceAccount(gid) != nil || socket != daemon.DefaultSocket() {
		return daemonsvc.ErrRefused
	}
	if verifySelfImage(imageHash) != nil {
		return daemonsvc.ErrRefused
	}
	policy, e := daemonsvc.VerifyPolicy(pin, pinHash, uint32(gid))
	if e != nil {
		return e
	}
	return a.superviseDaemonService(ctx, home, socket, policy, nativeServiceRuntime())
}

type serviceHeartbeat interface {
	Pulse(time.Time) error
	Run(context.Context, <-chan time.Time) error
	Close() error
}
type serviceRuntime struct {
	serve func(context.Context, string, string, io.Writer, daemon.Handler, func() error) error
	open  func(string) (serviceHeartbeat, error)
	clock func() (<-chan time.Time, func())
	now   func() time.Time
}

func nativeServiceRuntime() serviceRuntime {
	return serviceRuntime{
		serve: daemon.ServeService,
		open:  func(home string) (serviceHeartbeat, error) { return daemonsvc.OpenHeartbeat(home) },
		clock: func() (<-chan time.Time, func()) {
			t := time.NewTicker(daemonsvc.HeartbeatSeconds * time.Second)
			return t.C, t.Stop
		},
		now: time.Now,
	}
}

// Only the guarded public service path calls this lifecycle helper. Its private
// dependencies let fixtures drive ready/cancel/join without root or a supervisor;
// no CLI argument or station request can replace account/image/policy checks.
func (a *App) superviseDaemonService(ctx context.Context, home, socket string, policy daemonsvc.Policy, rt serviceRuntime) error {
	a.daemonOwner = policy.Owner
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	var heartbeat serviceHeartbeat
	var done chan error
	a.daemonOwnerEpoch = newDaemonOwnerEpoch()
	handler := freshServiceHandler(policy, a.daemonOwnerEpoch, a.daemonHandler(home))
	err := rt.serve(child, home, socket, a.Stderr, handler, func() error {
		var e error
		heartbeat, e = rt.open(home)
		if e != nil {
			return e
		}
		if e = heartbeat.Pulse(rt.now()); e != nil {
			heartbeat.Close()
			heartbeat = nil
			return e
		}
		done = make(chan error, 1)
		ticks, stop := rt.clock()
		go func() {
			e := heartbeat.Run(child, ticks)
			stop()
			if e != nil {
				cancel()
			}
			done <- e
		}()
		return nil
	})
	cancel()
	if done != nil {
		if e := <-done; e != nil {
			err = daemon.Unavailable
		}
		heartbeat.Close()
	}
	return err
}

// Restart loses this map intentionally. Persistent owner/lease evidence alone
// cannot authorize monitor renewal until a fresh SessionStart hook arrives.
// CM retains ownership ordering/disarm and owner-only lease handover on disk.
func freshServiceHandler(policy daemonsvc.Policy, epoch *daemonOwnerEpoch, next daemon.Handler) daemon.Handler {
	return func(ctx context.Context, inv daemon.Invocation) int {
		refuse := func() int {
			if inv.Refuse != nil {
				return inv.Refuse(daemon.NotAllowed)
			}
			_, _ = io.WriteString(inv.Stderr, "daemon: not_allowed\n")
			return ExitError
		}
		if !policy.Allows(inv.Identity) {
			return refuse()
		}
		plan, e := daemon.Authorize(inv.Request.Argv, inv.Identity)
		if e != nil {
			return refuse()
		}
		if plan.Command == "watch" || plan.Command == "wait" {
			owner := epoch.owner(inv.Identity)
			if owner == "" || owner != inv.Request.Session {
				return refuse()
			}
		}
		return next(ctx, inv)
	}
}
