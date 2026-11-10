//go:build darwin || linux

package daemonsvc

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

// disk's metadata/ownership seams are private: tests map fixture ownership,
// production always uses kernel metadata. No CLI can select a fake root.
type disk struct {
	stat    func(int, *unix.Stat_t) error
	chown   func(int, int, int) error
	noWrite func(int, string) bool
}

func realDisk() disk {
	return disk{stat: unix.Fstat, chown: unix.Fchown, noWrite: func(fd int, name string) bool {
		e := unix.Faccessat(fd, name, unix.W_OK, unix.AT_EACCESS)
		return e == unix.EACCES || e == unix.EPERM
	}}
}
func (d disk) directory(path string) (*os.File, error) {
	if !validPath(path) {
		return nil, ErrRefused
	}
	fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, ErrRefused
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return nil, ErrRefused
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}
func (d disk) rootDir(path string) (*os.File, error) {
	f, e := d.directory(path)
	if e != nil {
		return nil, e
	}
	var st unix.Stat_t
	if d.stat(int(f.Fd()), &st) != nil || st.Uid != 0 || st.Mode&07777 != 0755 {
		f.Close()
		return nil, ErrRefused
	}
	if os.Geteuid() != 0 && !d.noWrite(int(f.Fd()), ".") {
		f.Close()
		return nil, ErrRefused
	}
	// Every ancestor must be root-controlled; the final directory's exact mode
	// alone cannot protect a policy beneath an agent-writable ancestor.
	for parent := filepath.Dir(path); parent != "/"; parent = filepath.Dir(parent) {
		a, e := d.directory(parent)
		if e != nil {
			f.Close()
			return nil, ErrRefused
		}
		e = d.stat(int(a.Fd()), &st)
		denied := os.Geteuid() == 0 || d.noWrite(int(a.Fd()), ".")
		a.Close()
		if e != nil || st.Uid != 0 || st.Mode&0022 != 0 || !denied {
			f.Close()
			return nil, ErrRefused
		}
	}
	return f, nil
}
func (d disk) readAt(dir *os.File, name string, uid, gid, mode uint32, limit int64) ([]byte, error) {
	fd, e := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if e != nil {
		return nil, ErrRefused
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if d.stat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uid || (gid != ^uint32(0) && st.Gid != gid) || uint32(st.Mode)&07777 != mode || st.Nlink != 1 {
		return nil, ErrRefused
	}
	if uid == 0 && os.Geteuid() != 0 && !d.noWrite(int(dir.Fd()), name) {
		return nil, ErrRefused
	}
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil || int64(len(b)) > limit {
		return nil, ErrRefused
	}
	return b, nil
}

// Policy is an immutable startup snapshot, never persisted in daemon home.
// Infra supplies the root-owned JSON and its hash in the reviewed plan.
type Policy struct {
	Version    int      `json:"version"`
	Owner      string   `json:"owner"`
	Identities []string `json:"identities"`
}

var ownerRE = regexp.MustCompile(`^@[^:\s]+:[^\s]+$`)
var identityRE = regexp.MustCompile(`^[a-z0-9_=-][a-z0-9._=-]{0,63}$`)

func (p Policy) Allows(id string) bool {
	for _, v := range p.Identities {
		if v == id {
			return true
		}
	}
	return false
}
func VerifyPolicy(path, hash string, gid uint32) (Policy, error) {
	return realDisk().policy(path, hash, gid)
}
func (d disk) policy(path, hash string, gid uint32) (Policy, error) {
	var p Policy
	if !hashRE.MatchString(hash) {
		return p, ErrRefused
	}
	dir, e := d.rootDir(filepath.Dir(path))
	if e != nil {
		return p, ErrRefused
	}
	defer dir.Close()
	b, e := d.readAt(dir, filepath.Base(path), 0, gid, 0440, 65536)
	if e != nil || Digest(b) != hash || strictJSON(b, &p) != nil || p.Version != 1 || !ownerRE.MatchString(p.Owner) || len(p.Owner) > 255 || len(p.Identities) == 0 || len(p.Identities) > 1024 {
		return Policy{}, ErrRefused
	}
	seen := map[string]bool{}
	for _, id := range p.Identities {
		if !identityRE.MatchString(id) || id == "." || id == ".." || seen[id] {
			return Policy{}, ErrRefused
		}
		seen[id] = true
	}
	return p, nil
}

// VerifyImage pins executable bytes through a no-follow descriptor. Ancestors
// and the executable must remain root-controlled on an installed service.
func VerifyImage(path, hash string) error { return realDisk().image(path, hash) }

// ReadPlan refuses links, FIFOs and writable/untrusted manifests before any
// supervisor interaction. Plan content is public root-owned mode 0644.
func ReadPlan(path string) (Plan, error) {
	d := realDisk()
	dir, e := d.rootDir(filepath.Dir(path))
	if e != nil {
		return Plan{}, e
	}
	defer dir.Close()
	b, e := d.readAt(dir, filepath.Base(path), 0, ^uint32(0), 0644, 65536)
	if e != nil {
		return Plan{}, e
	}
	return DecodePlan(b)
}
func (d disk) image(path, hash string) error {
	if !validPath(path) || !hashRE.MatchString(hash) {
		return ErrRefused
	}
	dir, e := d.rootDir(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer dir.Close()
	b, e := d.readAt(dir, filepath.Base(path), 0, ^uint32(0), 0755, 256<<20)
	if e != nil || Digest(b) != hash {
		return ErrRefused
	}
	return nil
}

// PrepareRunDir creates only the final directory. Existing objects are checked
// without chmod/chown/adoption. A held root-owned parent prevents redirection.
func PrepareRunDir(path string, uid, gid uint32) error {
	if os.Geteuid() != 0 {
		return ErrRefused
	}
	return realDisk().prepare(path, uid, gid)
}
func (d disk) prepare(path string, uid, gid uint32) error {
	if uid == 0 || gid == 0 || !validPath(path) {
		return ErrRefused
	}
	parent, e := d.rootDir(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer parent.Close()
	name := filepath.Base(path)
	fd, e := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e == unix.ENOENT {
		// Build a private sibling first, then publish atomically without replacing
		// anything. Failed ownership setup never leaves an adopted final directory.
		temp, e := os.MkdirTemp(filepath.Dir(path), ".a2ad-run-")
		if e != nil {
			return ErrRefused
		}
		defer os.Remove(temp)
		f, e := d.directory(temp)
		if e != nil {
			return e
		}
		e = d.chown(int(f.Fd()), int(uid), int(gid))
		if e == nil {
			e = unix.Fchmod(int(f.Fd()), 0711)
		}
		f.Close()
		if e != nil {
			return ErrRefused
		}
		if e = renameExclusive(int(parent.Fd()), filepath.Base(temp), name); e != nil {
			return ErrRefused
		}
		fd, e = unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}
	if e != nil {
		return ErrRefused
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if d.stat(fd, &st) != nil || st.Uid != uid || st.Gid != gid || st.Mode&07777 != 0711 {
		return ErrRefused
	}
	return nil
}
