//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/daemon"
	"github.com/relux-works/a2a-swarm/internal/daemonenroll"
	"golang.org/x/sys/unix"
)

const hookGapAttribute = "user.a2a.daemon-hook-gap"
const hookFailureLog = "daemon-hook-failures.log"
const hookFailureLineBytes = 128

type hookGap struct {
	Count uint64 `json:"count"`
	Since string `json:"since"`
}

// Pin each directory/file, refuse links and unsafe preexisting metadata, and
// lock the log itself. No sidecar, mailbox, journal or credential is written.
func hookFailureFile(base string, create bool) (*os.File, error) {
	ctx, cancel := context.WithTimeout(context.Background(), daemon.HookBudget)
	defer cancel()
	return hookFailureFileContext(ctx, base, create)
}
func hookFailureFileContext(ctx context.Context, base string, create bool) (*os.File, error) {
	canonical, err := filepath.EvalSymlinks(base)
	if err != nil || canonical != base {
		return nil, daemon.Unavailable
	}
	dir, err := os.Open(base)
	if err != nil {
		return nil, daemon.Unavailable
	}
	defer dir.Close()
	created := false
	if create {
		err := unix.Mkdirat(int(dir.Fd()), "state", 0700)
		created = err == nil
		if err != nil && err != unix.EEXIST {
			return nil, daemon.Unavailable
		}
	}
	fd, err := unix.Openat(int(dir.Fd()), "state", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if err == unix.ENOENT && !create {
			return nil, nil
		}
		return nil, daemon.Unavailable
	}
	defer unix.Close(fd)
	if created && unix.Fchmod(fd, 0700) != nil {
		return nil, daemon.Unavailable
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&07777 != 0700 {
		return nil, daemon.Unavailable
	}
	flags := unix.O_RDWR | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	logfd, err := unix.Openat(fd, hookFailureLog, flags, 0)
	if err == unix.ENOENT && create {
		logfd, err = unix.Openat(fd, hookFailureLog, flags|unix.O_CREAT|unix.O_EXCL, 0600)
		if err == unix.EEXIST {
			logfd, err = unix.Openat(fd, hookFailureLog, flags, 0)
		} else if err == nil {
			err = unix.Fchmod(logfd, 0600)
			if err != nil {
				unix.Close(logfd)
			}
		}
	}
	if err != nil {
		if err == unix.ENOENT && !create {
			return nil, nil
		}
		return nil, daemon.Unavailable
	}
	f := os.NewFile(uintptr(logfd), hookFailureLog)
	if unix.Fstat(logfd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || st.Size > daemon.HookFailureLines*hookFailureLineBytes {
		f.Close()
		return nil, daemon.Unavailable
	}
	for {
		err := unix.Flock(logfd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN || ctx.Err() != nil {
			f.Close()
			return nil, daemon.Unavailable
		}
		timer := time.NewTimer(stationInputPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, daemon.Unavailable
		case <-timer.C:
		}
	}
	return f, nil
}
func readHookGap(f *os.File) (hookGap, error) {
	var gap hookGap
	b := make([]byte, 256)
	n, err := unix.Fgetxattr(int(f.Fd()), hookGapAttribute, b)
	// Missing metadata is valid only for a new empty log.
	if err != nil {
		st, e := f.Stat()
		if e == nil && st.Size() == 0 {
			return gap, nil
		}
		return gap, daemon.Unavailable
	}
	if json.Unmarshal(b[:n], &gap) != nil {
		return gap, daemon.Unavailable
	}
	if gap.Count > 0 {
		when, err := time.Parse(time.RFC3339Nano, gap.Since)
		if err != nil || when.UTC().Format(time.RFC3339Nano) != gap.Since || gap.Count == ^uint64(0) {
			return gap, daemon.Unavailable
		}
	} else if gap.Since != "" {
		return gap, daemon.Unavailable
	}
	return gap, nil
}
func writeHookGap(f *os.File, g hookGap) error {
	data, err := json.Marshal(g)
	if err != nil {
		return daemon.Unavailable
	}
	if unix.Fsetxattr(int(f.Fd()), hookGapAttribute, data, 0) != nil {
		return daemon.Unavailable
	}
	return nil
}
func updateHookFailures(base, event string, class daemon.Diagnostic, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), daemon.HookBudget)
	defer cancel()
	return updateHookFailuresContext(ctx, base, event, class, stderr)
}
func updateHookFailuresContext(ctx context.Context, base, event string, class daemon.Diagnostic, stderr io.Writer) error {
	f, err := hookFailureFileContext(ctx, base, class != "")
	if err != nil || f == nil {
		return err
	}
	defer f.Close()
	gap, err := readHookGap(f)
	if err != nil {
		return err
	}
	if class == "" {
		if gap.Count == 0 {
			return nil
		}
		if err := writeHookGap(f, hookGap{}); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(stderr, "daemon: %d hook deliveries failed open since %s\n", gap.Count, gap.Since); err != nil {
			return daemon.Unavailable
		}
		return nil
	}
	// event is selected from hookEvents; class has a closed type and is checked.
	if daemon.Class(class) != class {
		return daemon.Unavailable
	}
	valid := event == "SessionEnd" || event == "Hook"
	for _, e := range hookEvents {
		if event == e.name {
			valid = true
		}
	}
	if !valid {
		return daemon.Unavailable
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if gap.Count == 0 {
		gap.Since = now
	}
	gap.Count++
	data, err := io.ReadAll(io.LimitReader(f, daemon.HookFailureLines*hookFailureLineBytes+1))
	if err != nil || len(data) > daemon.HookFailureLines*hookFailureLineBytes {
		return daemon.Unavailable
	}
	line := []byte(now + " " + event + " " + string(class) + "\n")
	combined := append(data, line...)
	lines := bytes.SplitAfter(combined, []byte("\n"))
	if len(lines) <= daemon.HookFailureLines+1 {
		if _, err = f.Seek(0, io.SeekEnd); err != nil {
			return daemon.Unavailable
		}
		if _, err = f.Write(line); err != nil {
			return daemon.Unavailable
		}
	} else {
		retained := bytes.Join(lines[len(lines)-daemon.HookFailureLines-1:], nil)
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return daemon.Unavailable
		}
		if _, err = f.Write(retained); err != nil || f.Truncate(int64(len(retained))) != nil {
			return daemon.Unavailable
		}
	}
	if err = writeHookGap(f, gap); err != nil {
		return err
	}
	return f.Sync()
}
func daemonStationStatus(a *App) error {
	if a.Getenv == nil {
		return daemon.EnrollmentInvalid
	}
	paths, err := config.ResolvePaths(a.Getenv)
	if err != nil {
		return daemon.EnrollmentInvalid
	}
	f, err := hookFailureFile(paths.Base, false)
	if err != nil {
		return err
	}
	var gap hookGap
	if f != nil {
		gap, err = readHookGap(f)
		f.Close()
		if err != nil {
			return err
		}
	}
	if _, err = fmt.Fprintf(a.Stdout, "hook_failures: %d\noldest_unreported: %s\n", gap.Count, gap.Since); err != nil {
		return err
	}
	e, present, err := daemonenroll.Read(filepath.Join(paths.Base, ".a2a"))
	defer clear(e.Key)
	if err != nil {
		return daemon.EnrollmentInvalid
	}
	if !present {
		_, err = io.WriteString(a.Stdout, "log_dropped: 0\n")
		return err
	}
	ctx, cancel := context.WithTimeout(a.context(), daemon.HookBudget)
	defer cancel()
	code, err := daemon.Client(ctx, e.Socket, e.Station, e.Identity, e.Key, daemon.Request{Argv: []string{"daemon", "status"}}, a.Stdout, a.Stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return daemon.Unavailable
	}
	return nil
}
