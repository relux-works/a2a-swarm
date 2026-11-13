//go:build linux || darwin

package wakemute

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const Supported = true

type pinnedRoot struct {
	path string
	root *os.Root
	fd   *os.File
	info os.FileInfo
}

func identity(st os.FileInfo) RootIdentity {
	s := st.Sys().(*syscall.Stat_t)
	return RootIdentity{Device: uint64(s.Dev), Inode: uint64(s.Ino)}
}
func safe(st os.FileInfo, dir bool) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok || s.Uid != uint32(os.Geteuid()) {
		return false
	}
	if dir {
		return st.IsDir() && st.Mode().Perm()&0022 == 0
	}
	return st.Mode().IsRegular() && st.Mode().Perm() == 0600 && s.Nlink == 1
}
func pin(path string) (*pinnedRoot, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != path {
		return nil, ErrUnsafe
	}
	info, err := os.Lstat(path)
	if err != nil || !safe(info, true) {
		return nil, ErrAuthority
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, ErrUnsafe
	}
	fd, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, ErrUnsafe
	}
	now, err := fd.Stat()
	if err != nil || !os.SameFile(info, now) {
		fd.Close()
		root.Close()
		return nil, ErrUnsafe
	}
	return &pinnedRoot{path, root, fd, info}, nil
}
func (r *pinnedRoot) close() { r.fd.Close(); r.root.Close() }
func (r *pinnedRoot) check() error {
	st, err := os.Lstat(r.path)
	if err != nil || !safe(st, true) || !os.SameFile(st, r.info) {
		return ErrUnsafe
	}
	return nil
}
func (r *pinnedRoot) lock(ctx context.Context, exclusive bool) error {
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(r.fd.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			return r.check()
		}
		if err != unix.EWOULDBLOCK && err != unix.EINTR {
			return ErrUnsafe
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func (r *pinnedRoot) read(name string, v any) error {
	if err := r.check(); err != nil {
		return err
	}
	st, err := r.root.Lstat(name)
	if err != nil {
		return err
	}
	if !safe(st, false) || st.Size() > MaxBytes {
		return ErrUnsafe
	}
	f, err := r.root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return ErrUnsafe
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !safe(actual, false) || !os.SameFile(st, actual) {
		return ErrUnsafe
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return ErrUnsafe
	}
	return decode(data, v)
}

// Fault hooks are package-private and only supplied by isolated tests. They are
// never driven by flags, environment variables or production configuration.
type fault func(string) error

func (r *pinnedRoot) write(ctx context.Context, name string, v any, inject fault) error {
	point := func(p string) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if inject != nil {
			return inject(p)
		}
		return nil
	}
	if err := r.check(); err != nil {
		return err
	}
	if st, err := r.root.Lstat(name); err == nil {
		if !safe(st, false) {
			return ErrUnsafe
		}
	} else if !os.IsNotExist(err) {
		return ErrUnsafe
	}
	data, err := json.Marshal(v)
	if err != nil || len(data) > MaxBytes {
		return ErrUnsafe
	}
	if err = point("before-create"); err != nil {
		return err
	}
	nonce, err := NewOperation(Scope{{Root: r.path, Connections: []Connection{{Path: "."}}}}, "temporary-write")
	if err != nil {
		return err
	}
	temp := ".wake-mute-" + nonce.ID
	f, err := r.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer r.root.Remove(temp)
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if _, err = f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err = point("before-file-sync"); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = point("after-file-sync"); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = r.check(); err != nil {
		return err
	}
	if err = point("before-rename"); err != nil {
		return err
	}
	if err = r.root.Rename(temp, name); err != nil {
		return err
	}
	if err = point("after-rename"); err != nil {
		return uncertain(err)
	}
	if err = r.fd.Sync(); err != nil {
		return uncertain(err)
	}
	if err = point("after-directory-sync"); err != nil {
		return uncertain(err)
	}
	return nil
}
func roots(ctx context.Context, s Scope, exclusive bool) ([]*pinnedRoot, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, ErrAuthority
	}
	var out []*pinnedRoot
	fail := func(err error) ([]*pinnedRoot, error) {
		for _, r := range out {
			r.close()
		}
		return nil, err
	}
	for _, v := range s {
		r, err := pin(v.Root)
		if err != nil {
			return fail(err)
		}
		out = append(out, r)
		if err = r.lock(ctx, exclusive); err != nil {
			return fail(err)
		}
	}
	return out, nil
}
func closeRoots(rs []*pinnedRoot) {
	for i := len(rs) - 1; i >= 0; i-- {
		rs[i].close()
	}
}
func (r *pinnedRoot) enrolled() (Enrollment, Store, error) {
	var e Enrollment
	var s Store
	if err := r.read(Marker, &e); err != nil {
		if os.IsNotExist(err) {
			if _, other := r.root.Lstat(Ledger); os.IsNotExist(other) {
				return e, s, ErrUnsupported
			}
		}
		return e, s, ErrUnsafe
	}
	host, err := os.Hostname()
	normalized, nerr := Normalize(Scope{{Root: r.path, Connections: e.Connections}})
	if err != nil || nerr != nil || e.Version != Version || e.Owner != os.Geteuid() || e.Host != host || e.Identity != identity(r.info) || !slices.Equal(normalized[0].Connections, e.Connections) {
		return e, s, ErrUnsafe
	}
	if e.Snapshot == nil || !e.Snapshot.valid() {
		return e, s, ErrUnsafe
	}
	validate := func(store Store) bool {
		for _, op := range store.Operations {
			if op.Host != host || op.Owner != os.Geteuid() {
				return false
			}
			i := slices.IndexFunc(op.Scope, func(v Selection) bool { return v.Root == r.path })
			if i < 0 {
				return false
			}
			for _, c := range op.Scope[i].Connections {
				if !slices.Contains(e.Connections, c) {
					return false
				}
			}
		}
		return true
	}
	if !validate(*e.Snapshot) {
		return e, s, ErrUnsafe
	}
	if err = r.read(Ledger, &s); err != nil || !s.valid() || !validate(s) {
		return e, *e.Snapshot, ErrUnsafe
	}
	if !equalStore(*e.Snapshot, s) {
		effective, err := recoveryStore(*e.Snapshot, s)
		if err != nil {
			return e, effective, err
		}
		return e, effective, ErrUncertain
	}
	return e, s, nil
}
func enrolledScope(rs []*pinnedRoot, s Scope, recovery bool) ([]Store, error) {
	stores := make([]Store, len(rs))
	for i, r := range rs {
		e, st, err := r.enrolled()
		if err != nil && !(recovery && errors.Is(err, ErrUncertain)) {
			return nil, err
		}
		for _, c := range s[i].Connections {
			if !slices.Contains(e.Connections, c) {
				return nil, ErrUnsupported
			}
		}
		stores[i] = st
	}
	return stores, nil
}

// Enroll explicitly admits compatible consumers for a finite connection list.
// It never expands an existing enrollment. An interrupted initialization is
// unsafe/unsupported, never evidence of an active scope mute or safe freeze.
func Enroll(ctx context.Context, approved Operation) error {
	if !approved.valid() {
		return ErrAuthority
	}
	host, _ := os.Hostname()
	if approved.Owner != os.Geteuid() || approved.Host != host {
		return ErrAuthority
	}
	rs, err := roots(ctx, approved.Scope, true)
	if err != nil {
		return err
	}
	defer closeRoots(rs)
	if !authorized(approved, "enroll", rs) {
		return ErrAuthority
	}
	for _, r := range rs {
		for _, name := range []string{Marker, Ledger} {
			if _, e := r.root.Lstat(name); !os.IsNotExist(e) {
				return ErrUnsupported
			}
		}
	}
	for i, r := range rs {
		// Ledger precedes marker; interrupted enrollment fails closed on discovery.
		if err = r.write(ctx, Ledger, Store{Version: Version, Operations: []Operation{}}, nil); err != nil {
			return uncertain(err)
		}
		e := Enrollment{Version: Version, Host: approved.Host, Owner: approved.Owner, Identity: identity(r.info), Connections: approved.Scope[i].Connections, Snapshot: &Store{Version: Version, Operations: []Operation{}}}
		if err = r.write(ctx, Marker, e, nil); err != nil {
			return uncertain(err)
		}
	}
	return nil
}

func Mute(ctx context.Context, approved Operation) (Result, error) { return mute(ctx, approved, nil) }
func mute(ctx context.Context, o Operation, inject fault) (result Result, err error) {
	result = Result{Version: Version, Status: "uncertain", Scope: o.Scope, Operation: &o, CoverageLimits: Limits}
	host, _ := os.Hostname()
	if !o.valid() || o.State != "installing" || o.Owner != os.Geteuid() || o.Host != host {
		return result, ErrAuthority
	}
	rs, err := roots(ctx, o.Scope, true)
	if err != nil {
		return result, err
	}
	defer closeRoots(rs)
	defer func() { result.Roots = statusRoots(rs, result.Scope) }()
	if !authorized(o, "mute", rs) {
		return result, ErrAuthority
	}
	stores, err := enrolledScope(rs, o.Scope, false)
	if err != nil {
		return result, err
	}
	for _, s := range stores {
		for _, old := range s.Operations {
			if old.ID == o.ID || old.Run == o.Run {
				return result, ErrStale
			}
		}
	}
	// All roots are pinned and locked. Coordinator intent is durable before any
	// other root changes. Installing is itself an effective mute on that root.
	for i, r := range rs {
		stores[i].Operations = append(stores[i].Operations, o)
		stores[i].Generation++
		if err = r.writeStore(ctx, stores[i], inject); err != nil {
			return result, uncertain(err)
		}
	}
	o.State = "active"
	for i, r := range rs {
		stores[i].Operations[len(stores[i].Operations)-1] = o
		if err = r.writeStore(ctx, stores[i], inject); err != nil {
			return result, uncertain(err)
		}
	}
	result.Status = "muted"
	result.Operation = &o
	result.Roots = statusRoots(rs, result.Scope)
	return result, nil
}

func Unmute(ctx context.Context, s Scope, run, id string) (Result, error) {
	return unmute(ctx, s, run, id, nil)
}
func unmute(ctx context.Context, scope Scope, run, id string, inject fault) (result Result, err error) {
	s, err := Normalize(scope)
	result = Result{Version: Version, Status: "uncertain", Scope: s, CoverageLimits: Limits}
	if err != nil || !idRE.MatchString(id) || run == "" {
		return result, ErrAuthority
	}
	rs, err := roots(ctx, s, true)
	if err != nil {
		return result, err
	}
	defer closeRoots(rs)
	defer func() { result.Roots = statusRoots(rs, result.Scope) }()
	stores, err := enrolledScope(rs, s, true)
	if err != nil {
		return result, err
	}
	found := false
	active := false
	for _, st := range stores {
		for _, o := range st.Operations {
			if o.ID == id {
				if o.Run != run || !equalScope(o.Scope, s) {
					return result, ErrAuthority
				}
				found = true
				active = active || o.State != "closed"
			}
		}
	}
	if !found || !active {
		return result, ErrStale
	}
	// Persist cleanup intent everywhere before closing any present row. A crash
	// can leave a partial thaw, but every remaining mute retains exact authority.
	for i, r := range rs {
		for j := range stores[i].Operations {
			o := &stores[i].Operations[j]
			if o.ID == id && o.State != "closed" {
				o.State = "closing"
			}
		}
		if err = r.writeStore(ctx, stores[i], inject); err != nil {
			return result, uncertain(err)
		}
	}
	for i, r := range rs {
		for j := range stores[i].Operations {
			o := &stores[i].Operations[j]
			if o.ID == id {
				o.State = "closed"
				result.Operation = o
			}
		}
		stores[i].Generation++
		if err = r.writeStore(ctx, stores[i], inject); err != nil {
			return result, uncertain(err)
		}
	}
	result.Status = "operation-closed"
	result.Roots = statusRoots(rs, result.Scope)
	return result, nil
}
func statusRoots(rs []*pinnedRoot, scope Scope) []RootStatus {
	out := make([]RootStatus, 0, len(rs))
	for _, r := range rs {
		_, s, err := r.enrolled()
		st := RootStatus{Root: r.path, State: "unmuted", Generation: s.Generation, Operations: s.Operations, Ages: map[string]int64{}}
		for _, o := range s.Operations {
			st.Ages[o.ID] = max(0, int64(time.Since(o.Created)/time.Second))
		}
		if err != nil {
			st.State = "uncertain"
			st.Uncertainty = err.Error()
			if errors.Is(err, ErrUnsupported) {
				st.State = "unsupported"
			}
		} else {
			for _, o := range s.Operations {
				if o.State != "closed" && applies(o.Scope, scope) {
					st.State = "muted"
				}
				if o.State == "installing" || o.State == "closing" {
					st.Uncertainty = "partial operation; inspect every root in its recorded scope"
				}
			}
		}
		out = append(out, st)
	}
	return out
}

// Status acquires shared directory locks without creating a lock file. It never
// initializes, repairs, expires or rewrites state, including after corruption.
func Status(ctx context.Context, scope Scope) (Result, error) {
	s, err := Normalize(scope)
	out := Result{Version: Version, Status: "unmuted", Scope: s, CoverageLimits: Limits}
	if err != nil {
		return out, err
	}
	if _, ok := ctx.Deadline(); !ok {
		return out, ErrAuthority
	}
	// Preserve discovery on readable roots even if a neighbor is unavailable.
	// These observations cannot certify complete scope and never repair it.
	var rs []*pinnedRoot
	rows := map[string]RootStatus{}
	for _, selection := range s {
		r, e := pin(selection.Root)
		if e == nil {
			if e = r.lock(ctx, false); e != nil {
				r.close()
			} else {
				rs = append(rs, r)
			}
		}
		if e != nil {
			rows[selection.Root] = RootStatus{Root: selection.Root, State: "uncertain", Uncertainty: e.Error()}
		}
	}
	defer closeRoots(rs)
	for _, row := range statusRoots(rs, s) {
		rows[row.Root] = row
	}
	bad, unsupported := false, false
	for _, selection := range s {
		row := rows[selection.Root]
		out.Roots = append(out.Roots, row)
		bad = bad || row.State == "uncertain"
		unsupported = unsupported || row.State == "unsupported"
		if row.State == "muted" {
			out.Status = "muted"
		}
	}
	seen := map[string]bool{}
	partial := false
	for _, root := range out.Roots {
		for _, o := range root.Operations {
			if seen[o.ID] || o.State == "closed" || !applies(o.Scope, s) {
				continue
			}
			seen[o.ID] = true
			coverage := OperationCoverage{ID: o.ID, Covered: Scope{}, NotCovered: Scope{}}
			for _, selection := range o.Scope {
				covered := false
				for _, observed := range out.Roots {
					if observed.Root != selection.Root {
						continue
					}
					for _, other := range observed.Operations {
						if other.ID == o.ID && other.State != "closed" && equalScope(other.Scope, o.Scope) {
							covered = true
							if other.State != "active" {
								partial = true
							}
						}
					}
				}
				if covered {
					coverage.Covered = append(coverage.Covered, selection)
				} else {
					coverage.NotCovered = append(coverage.NotCovered, selection)
					partial = true
				}
			}
			out.Operations = append(out.Operations, coverage)
		}
	}
	if bad {
		out.Status = "uncertain"
		return out, ErrUnsafe
	}
	if unsupported {
		out.Status = "unsupported"
		return out, ErrUnsupported
	}
	if partial {
		out.Status = "partial"
		return out, ErrUncertain
	}
	if _, err = enrolledScope(rs, s, false); err != nil {
		out.Status = "unsupported"
		return out, err
	}
	return out, nil
}

// Admission is held through the bounded local effect, not just reservation.
// Caller lock order is identity control -> root admission -> state/mailbox.
// Mute takes only roots (sorted), never identity/consumer/state/transport locks.
// A root absent from an enrollment remains legacy/unsupported, outside coverage.
// AdmissionRequired is the admitted launch contract. Expected participation is
// independent of the root's mutable files, so loss of BOTH enrollment and ledger
// cannot be mistaken for an uninstalled legacy namespace, including on restart.
func AdmissionRequired(ctx context.Context, root, connectionPath string, version int, effect func(uint64) error) error {
	if version != Version {
		return ErrUnsupported
	}
	return admission(ctx, root, connectionPath, true, effect)
}
func Admission(ctx context.Context, root, connectionPath string, effect func(uint64) error) error {
	return admission(ctx, root, connectionPath, false, effect)
}
func admission(ctx context.Context, root, connectionPath string, required bool, effect func(uint64) error) error {
	if _, ok := ctx.Deadline(); !ok {
		return ErrAuthority
	}
	if connectionPath != "." && (!strings.HasPrefix(connectionPath, "connections/") || !slug.MatchString(strings.TrimPrefix(connectionPath, "connections/"))) {
		return ErrAuthority
	}
	root = filepath.Clean(root)
	// A nested A2A_CONFIG_DIR override cannot hide an admitted ancestor control.
	for parent := filepath.Dir(root); parent != root; parent = filepath.Dir(parent) {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, n := range []string{Marker, Ledger} {
			if _, e := os.Lstat(filepath.Join(parent, n)); e == nil || !os.IsNotExist(e) {
				return ErrUnsafe
			}
		}
		if parent == filepath.Dir(parent) {
			break
		}
	}
	// Even a not-yet-enrolled compatible consumer takes the directory fence:
	// enrollment and first mute cannot race an unchecked legacy admission.
	r, err := pin(root)
	if err != nil {
		return err
	}
	defer r.close()
	if err = r.lock(ctx, false); err != nil {
		return err
	}
	_, me := r.root.Lstat(Marker)
	_, le := r.root.Lstat(Ledger)
	if os.IsNotExist(me) && os.IsNotExist(le) {
		if required {
			return ErrUnsafe
		}
		return effect(0)
	}
	e, s, err := r.enrolled()
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(e.Connections, func(c Connection) bool { return c.Path == connectionPath }) {
		return ErrUnsupported
	}
	for _, o := range s.Operations {
		if o.State != "closed" {
			for _, v := range o.Scope {
				if v.Root == root && slices.ContainsFunc(v.Connections, func(c Connection) bool { return c.Path == connectionPath }) {
					return ErrMuted
				}
			}
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return effect(s.Generation)
}

// The enrollment's recovery snapshot is durable BEFORE the operational copy.
// A crash or replacement of either copy cannot silently remove a recorded mute.
// Disagreement fails admission and retains the conservative union for exact undo.
func (r *pinnedRoot) writeStore(ctx context.Context, s Store, inject fault) error {
	var enrollment Enrollment
	if err := r.read(Marker, &enrollment); err != nil {
		return ErrUnsafe
	}
	enrollment.Snapshot = &s
	if err := r.write(ctx, Marker, enrollment, inject); err != nil {
		return err
	}
	return r.write(ctx, Ledger, s, inject)
}
