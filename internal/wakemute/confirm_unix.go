//go:build linux || darwin

package wakemute

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/mattn/go-isatty"
	"golang.org/x/sys/unix"
)

// Challenge includes the full displayed scope and a digest of the exact fresh
// operation, host, owner and scope. It is public correlation, not a credential.
func Challenge(o Operation, action string) string {
	b, _ := json.Marshal(struct {
		Action    string
		Operation Operation
	}{action, o})
	sum := sha256.Sum256(b)
	return action + " " + o.ID + " " + hex.EncodeToString(sum[:])
}

// Confirm requires actual terminal descriptors, with a finite input/output
// budget. It does not open /dev/tty, allocate a terminal or bypass TMUX/STY.
// Isatty and environment checks are not proof of a human or signed provenance.
func Confirm(ctx context.Context, in, out *os.File, o Operation, action string) (Operation, error) {
	if _, ok := ctx.Deadline(); !ok || (action != "mute" && action != "enroll") || !o.valid() || in == nil || out == nil || !isatty.IsTerminal(in.Fd()) || !isatty.IsTerminal(out.Fd()) || os.Getenv("TMUX") != "" || os.Getenv("STY") != "" {
		return Operation{}, ErrAuthority
	}
	rs, err := roots(ctx, o.Scope, false)
	if err != nil {
		return Operation{}, err
	}
	defer closeRoots(rs)
	// Pin before displaying the owner-approved selection, retaining pins through
	// confirmation. The mutation rechecks these inode identities after locking.
	o.roots = make(map[string]RootIdentity, len(rs))
	for _, r := range rs {
		o.roots[r.path] = identity(r.info)
	}
	scope, _ := json.MarshalIndent(o.Scope, "", "  ")
	challenge := Challenge(o, action)
	prompt := fmt.Sprintf("Scope wake %s; public operation %s\nHost: %q; OS owner: %d; run: %q\nExact roots and connections:\n%s\nMuted is not quiesced. No process control or drain.\nType exactly: %s\n", action, o.ID, o.Host, o.Owner, o.Run, scope, challenge)
	if err = terminalWrite(ctx, out, []byte(prompt)); err != nil {
		return Operation{}, err
	}
	var response []byte
	for len(response) <= 1024 {
		if err = terminalReady(ctx, in, unix.POLLIN); err != nil {
			return Operation{}, err
		}
		var one [1]byte
		n, e := in.Read(one[:])
		if e != nil {
			return Operation{}, e
		}
		if n == 0 {
			return Operation{}, io.EOF
		}
		if one[0] == '\n' {
			break
		}
		response = append(response, one[0])
	}
	if string(response) != challenge {
		return Operation{}, ErrAuthority
	}
	for _, r := range rs {
		if err = r.check(); err != nil {
			return Operation{}, err
		}
	}
	o.consent = Challenge(o, action)
	return o, nil
}
func terminalReady(ctx context.Context, f *os.File, event int16) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return ErrAuthority
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return context.DeadlineExceeded
		}
		p := []unix.PollFd{{Fd: int32(f.Fd()), Events: event}}
		_, err := unix.Poll(p, int(min(left.Milliseconds()+1, 100)))
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if p[0].Revents&event != 0 {
			return nil
		}
		if p[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			return io.EOF
		}
	}
}
func terminalWrite(ctx context.Context, out *os.File, b []byte) error {
	_, err, handled := ttyWrite(ctx, out, b)
	if !handled {
		return ErrAuthority
	}
	return err
}
func ttyWrite(ctx context.Context, out *os.File, b []byte) (int, error, bool) {
	if !isatty.IsTerminal(out.Fd()) {
		return 0, nil, false
	}
	total := 0
	for len(b) > 0 {
		if err := terminalReady(ctx, out, unix.POLLOUT); err != nil {
			return total, err, true
		}
		n, err := out.Write(b[:min(len(b), 256)])
		total += n
		if err != nil {
			return total, err, true
		}
		if n == 0 {
			return total, io.ErrShortWrite, true
		}
		b = b[n:]
	}
	return total, nil, true
}

func authorized(o Operation, action string, rs []*pinnedRoot) bool {
	if o.consent != Challenge(o, action) {
		return false
	}
	for _, r := range rs {
		if o.roots[r.path] != identity(r.info) {
			return false
		}
	}
	return true
}
