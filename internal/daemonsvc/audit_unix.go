//go:build darwin || linux

package daemonsvc

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"golang.org/x/sys/unix"
)

func privateDir(path string) (*os.File, error) {
	d := realDisk()
	f, e := d.directory(path)
	if e != nil {
		return nil, e
	}
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&07777 != 0700 {
		f.Close()
		return nil, ErrRefused
	}
	return f, nil
}
func privateOutput(dir *os.File, name string) (*os.File, error) {
	fd, e := unix.Openat(int(dir.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if e != nil {
		return nil, ErrRefused
	}
	f := os.NewFile(uintptr(fd), name)
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0600 || st.Nlink != 1 {
		f.Close()
		return nil, ErrRefused
	}
	return f, nil
}

// Heartbeat starts only after the server owns the socket and serve lock. Its
// descriptor is held throughout the run. Failure cancels service supervision.
type Heartbeat struct{ f *os.File }

func OpenHeartbeat(home string) (*Heartbeat, error) {
	dir, e := privateDir(home)
	if e != nil {
		return nil, e
	}
	defer dir.Close()
	f, e := privateOutput(dir, "heartbeat")
	if e != nil {
		return nil, e
	}
	if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		f.Close()
		return nil, ErrRefused
	}
	return &Heartbeat{f}, nil
}
func (h *Heartbeat) Close() error { return h.f.Close() }
func (h *Heartbeat) Pulse(now time.Time) error {
	stamp := unix.NsecToTimeval(now.UnixNano())
	if unix.Futimes(int(h.f.Fd()), []unix.Timeval{stamp, stamp}) != nil {
		return ErrRefused
	}
	return nil
}
func (h *Heartbeat) Run(ctx context.Context, ticks <-chan time.Time) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case now, ok := <-ticks:
			if !ok {
				return nil
			}
			if h.Pulse(now) != nil {
				return ErrRefused
			}
		}
	}
}
func daemonDown(home string, now time.Time) bool {
	dir, e := privateDir(home)
	if e != nil {
		return true
	}
	defer dir.Close()
	fd, e := unix.Openat(int(dir.Fd()), "heartbeat", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if e != nil {
		return true
	}
	f := os.NewFile(uintptr(fd), "heartbeat")
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 {
		return true
	}
	// The kernel drops the daemon's lifetime lock on death, even immediately
	// after a fresh pulse. The next sweep detects death within one cadence;
	// mtime alone could take cadence+staleness. No PID reuse or supervisor call.
	if e := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); e == nil {
		unix.Flock(fd, unix.LOCK_UN)
		return true
	} else if e != unix.EWOULDBLOCK && e != unix.EAGAIN {
		return true
	}
	info, e := f.Stat()
	if e != nil {
		return true
	}
	age := now.Sub(info.ModTime())
	return age < 0 || age >= StaleSeconds*time.Second
}

type Finding struct {
	Identity   string `json:"identity"`
	Pending    int    `json:"pending"`
	Shown      int    `json:"shown"`
	Unreadable bool   `json:"unreadable"`
}
type SweepReport struct {
	Version  int       `json:"version"`
	At       time.Time `json:"at"`
	Liveness string    `json:"liveness"`
	Findings []Finding `json:"findings"`
}

// Sweep is a local snapshot: no credential load, sync, state lock, receipt,
// lease, notice or escalation. Errors become counts-only unreadable findings.
func Sweep(home string, policy Policy, now time.Time) SweepReport {
	r := SweepReport{Version: 1, At: now, Liveness: "daemon_up", Findings: []Finding{}}
	if daemonDown(home, now) {
		r.Liveness = "daemon_down"
	}
	for _, id := range policy.Identities {
		f := Finding{Identity: id}
		base := filepath.Join(home, "identities", id)
		p := config.Paths{Base: base}
		dir, e := privateDir(base)
		if e == nil {
			dir.Close()
			var registry *config.ConnectionRegistry
			registry, e = config.LoadConnections(p)
			if e == nil && registry != nil {
				c, ok := registry.Profiles[registry.Current]
				if !ok {
					e = ErrRefused
				} else {
					p, e = c.Paths(p, id, false)
				}
			}
		}
		if e == nil {
			dir, e = privateDir(filepath.Join(p.Base, "state"))
			if e == nil {
				dir.Close()
			}
		}
		if e != nil {
			f.Unreadable = true
		} else {
			mb, e := config.OpenMailbox(p, id)
			if e != nil {
				f.Unreadable = true
			} else {
				for _, entry := range mb.Pending() {
					f.Pending++
					if entry.Shown {
						f.Shown++
					}
				}
			}
		}
		r.Findings = append(r.Findings, f)
	}
	return r
}
func WriteReport(home, name string, v any) error {
	if name != "sweep-report.json" && name != "soak-report.json" {
		return ErrRefused
	}
	dir, e := privateDir(home)
	if e != nil {
		return e
	}
	defer dir.Close()
	// Serialize writers with a separate private lock. Readers receive either a
	// complete old artifact or a complete new one after atomic rename.
	lock, e := privateOutput(dir, "audit.lock")
	if e != nil {
		return e
	}
	defer lock.Close()
	if unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		return ErrRefused
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	// Refuse an unsafe target even though rename would replace it safely.
	var st unix.Stat_t
	if e = unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); e == nil {
		f, e := privateOutput(dir, name)
		if e != nil {
			return e
		}
		f.Close()
	} else if e != unix.ENOENT {
		return ErrRefused
	}
	b, e := json.Marshal(v)
	if e != nil {
		return ErrRefused
	}
	b = append(b, '\n')
	temp, e := os.CreateTemp(home, ".a2ad-report-")
	if e != nil {
		return ErrRefused
	}
	defer os.Remove(temp.Name())
	_, e = temp.Write(b)
	if e == nil {
		e = temp.Sync()
	}
	ce := temp.Close()
	if e != nil || ce != nil {
		return ErrRefused
	}
	if unix.Renameat(int(dir.Fd()), filepath.Base(temp.Name()), int(dir.Fd()), name) != nil || dir.Sync() != nil {
		return ErrRefused
	}
	return nil
}

// RescueRecord records an actual recovered delivery, never a detection. The
// controller must record sweep/watchdog rescues after it delivers via inbox.
// Traffic is an explicit counts-only observation for qualification, not a
// claim inferred from elapsed uptime or from sweep findings.
type RescueRecord struct {
	At       time.Time `json:"at"`
	Identity string    `json:"identity"`
	Layer    string    `json:"layer"`
	Count    int       `json:"count"`
}

func RecordRescue(home string, p Policy, r RescueRecord) error {
	if !p.Allows(r.Identity) || r.Count <= 0 || r.Count > 1000000 || r.At.IsZero() || (r.Layer != "sweep" && r.Layer != "watchdog" && r.Layer != "manual" && r.Layer != "traffic") {
		return ErrRefused
	}
	dir, e := privateDir(home)
	if e != nil {
		return e
	}
	defer dir.Close()
	f, e := privateOutput(dir, r.Identity+".rescue.jsonl")
	if e != nil {
		return e
	}
	defer f.Close()
	if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		return ErrRefused
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	if _, e = f.Seek(0, io.SeekEnd); e != nil {
		return ErrRefused
	}
	b, _ := json.Marshal(r)
	b = append(b, '\n')
	n, e := f.Write(b)
	if e != nil || n != len(b) || f.Sync() != nil {
		return ErrRefused
	}
	return nil
}

type SoakFinding struct {
	Identity  string `json:"identity"`
	Traffic   int    `json:"traffic"`
	Rescues   int    `json:"rescues"`
	Qualified bool   `json:"qualified"`
}
type SoakReport struct {
	Version   int           `json:"version"`
	From      time.Time     `json:"from"`
	To        time.Time     `json:"to"`
	Days      int           `json:"days"`
	Findings  []SoakFinding `json:"findings"`
	Qualified bool          `json:"qualified"`
}

// Qualification requires an owner-selected interval, real traffic observations
// and zero recorded rescues for EVERY identity. Missing/corrupt logs refuse;
// this artifact never removes hooks or independently certifies native delivery.
func Soak(home string, p Policy, from, to time.Time, days int) (SoakReport, error) {
	r := SoakReport{Version: 1, From: from, To: to, Days: days, Qualified: true, Findings: []SoakFinding{}}
	if days < 1 || days > 365 || from.IsZero() || to.Before(from) || to.Sub(from) < time.Duration(days)*24*time.Hour {
		return r, ErrRefused
	}
	dir, e := privateDir(home)
	if e != nil {
		return r, e
	}
	defer dir.Close()
	for _, id := range p.Identities {
		b, e := realDisk().readAt(dir, id+".rescue.jsonl", uint32(os.Geteuid()), ^uint32(0), 0600, 16<<20)
		if e != nil {
			return r, ErrRefused
		}
		f := SoakFinding{Identity: id}
		records, e := decodeRecords(b)
		if e != nil {
			return r, e
		}
		for _, v := range records {
			if v.Identity != id || v.At.IsZero() || v.Count <= 0 || v.Count > 1000000 {
				return r, ErrRefused
			}
			if v.At.Before(from) || v.At.After(to) {
				continue
			}
			switch v.Layer {
			case "traffic":
				f.Traffic += v.Count
			case "manual", "sweep", "watchdog":
				f.Rescues += v.Count
			default:
				return r, ErrRefused
			}
		}
		f.Qualified = f.Traffic > 0 && f.Rescues == 0
		r.Qualified = r.Qualified && f.Qualified
		r.Findings = append(r.Findings, f)
	}
	return r, nil
}
