package config

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
)

// ErrMemberRoot never includes paths, provider errors or credential material.
var ErrMemberRoot = errors.New("fresh member root unavailable or changed; owner recovery required; no automatic retry")

type memberDirectory struct {
	root    *os.Root
	parent  *os.Root
	name    string
	info    os.FileInfo
	private bool
	entries map[string]os.FileInfo
}

// MemberRoot retains directory descriptors and the credential guard through
// registration. External path replacement can never redirect its writes. A
// failed reservation is retained; freshness, not cleanup, fences future reuse.
type MemberRoot struct {
	Connection Connection
	Paths      Paths
	Binding    ConnectionBinding
	profile    string
	name       string
	dirs       []*memberDirectory
	base       *memberDirectory
	metadata   *memberDirectory
	identity   *memberDirectory
	guard      *CredentialGuard
}

// ReserveMemberRoot requires an existing trusted parent and a nonexistent leaf.
// Exclusive mkdir reserves the whole destination before any remote effect; the
// usual credential flock and exclusive journal reserve the member transaction.
func ReserveMemberRoot(ctx context.Context, path, source, profile, name string, c Connection) (*MemberRoot, error) {
	if !postOperationPlatform() || !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 4096 || strings.IndexFunc(path, unicode.IsControl) >= 0 || ValidateName(name) != nil {
		return nil, ErrMemberRoot
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return nil, ErrMemberRoot
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return nil, ErrMemberRoot
	}
	sourceInfo, err := os.Stat(source)
	if err != nil || !sourceInfo.IsDir() {
		return nil, ErrMemberRoot
	}
	a, b := path, source
	if runtime.GOOS == "darwin" {
		a, b = strings.ToLower(a), strings.ToLower(b)
	}
	contains := func(parent, child string) bool {
		rel, e := filepath.Rel(parent, child)
		return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if contains(a, b) || contains(b, a) {
		return nil, ErrMemberRoot
	}
	// Projection is an allowlist, not a scrub of administrator configuration.
	member := Connection{ID: c.ID, URL: c.URL, ServerName: c.ServerName, CardRoom: c.CardRoom, Store: CredentialStore{Backend: "file"}}
	r := ConnectionRegistry{SchemaVersion: 1, Current: profile, Profiles: map[string]Connection{profile: member}}
	if r.Validate() != nil {
		return nil, ErrMemberRoot
	}
	p, err := member.Paths(Paths{Base: path}, name, false)
	if err != nil {
		return nil, ErrMemberRoot
	}
	binding, err := member.Binding(name, false)
	if err != nil {
		return nil, ErrMemberRoot
	}
	m := &MemberRoot{Connection: member, Paths: p, Binding: binding, profile: profile, name: name}
	fail := func() (*MemberRoot, error) { m.Close(); return nil, ErrMemberRoot }
	root, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return fail()
	}
	info, err := root.Stat(".")
	if err != nil {
		root.Close()
		return fail()
	}
	parent := &memberDirectory{root: root, info: info}
	m.dirs = append(m.dirs, parent)
	parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	for i, part := range parts {
		// Physical identity also catches platform path aliases of the source,
		// independently of string spelling or filesystem case normalization.
		if os.SameFile(parent.info, sourceInfo) {
			return fail()
		}
		fresh := i == len(parts)-1
		if fresh {
			if err := ctx.Err(); err != nil {
				return fail()
			}
			// No existing leaf, even empty, may be adopted. Exactly one caller wins.
			if parent.root.Mkdir(part, 0700) != nil {
				return fail()
			}
			if syncPostRoot(parent.root) != nil {
				return fail()
			}
		}
		next, err := m.pin(parent, part, fresh)
		if err != nil {
			return fail()
		}
		parent = next
	}
	m.base = parent
	if syncPostRoot(parent.root) != nil {
		return fail()
	}
	// Pin every member-owned directory separately. Unknown entries (including a
	// migration/state import) are refused rather than incorporated into a fresh root.
	connection, err := m.mkdir(parent, "connections")
	if err != nil {
		return fail()
	}
	connection, err = m.mkdir(connection, member.ID)
	if err != nil {
		return fail()
	}
	m.metadata, err = m.mkdir(connection, "agents")
	if err != nil {
		return fail()
	}
	identity, err := m.mkdir(connection, "identities")
	if err != nil {
		return fail()
	}
	identity, err = m.mkdir(identity, "agents")
	if err != nil {
		return fail()
	}
	m.identity, err = m.mkdir(identity, filepath.Base(p.Base))
	if err != nil {
		return fail()
	}
	lockName := "." + name + ".lock"
	lock, err := openPostOperationFile(m.metadata.root, lockName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fail()
	}
	release, err := lockPostOperationContext(ctx, lock)
	if err != nil {
		lock.Close()
		return fail()
	}
	m.guard = &CredentialGuard{p: p, name: name, release: func() { release(); lock.Close() }, publish: m.publishCredentials}
	if m.remember(m.metadata, lockName) != nil || lock.Sync() != nil || syncPostRoot(m.metadata.root) != nil {
		return fail()
	}
	reserved, err := json.Marshal(struct {
		Binding ConnectionBinding `json:"binding"`
		Stage   string            `json:"stage"`
	}{binding, "reserved"})
	if err != nil || m.write(m.base, "registration.pending.json", reserved, false) != nil {
		return fail()
	}
	if m.Validate() != nil {
		return fail()
	}
	return m, nil
}

func (m *MemberRoot) pin(parent *memberDirectory, name string, private bool) (*memberDirectory, error) {
	st, err := parent.root.Lstat(name)
	if err != nil || !st.IsDir() || !postOperationOwner(st, private) {
		return nil, ErrMemberRoot
	}
	root, err := parent.root.OpenRoot(name)
	if err != nil {
		return nil, ErrMemberRoot
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(st, opened) {
		root.Close()
		return nil, ErrMemberRoot
	}
	d := &memberDirectory{root: root, parent: parent.root, name: name, info: st, private: private, entries: map[string]os.FileInfo{}}
	m.dirs = append(m.dirs, d)
	if parent.private {
		parent.entries[name] = st
	}
	return d, nil
}

func (m *MemberRoot) mkdir(parent *memberDirectory, name string) (*memberDirectory, error) {
	if parent.root.Mkdir(name, 0700) != nil || syncPostRoot(parent.root) != nil {
		return nil, ErrMemberRoot
	}
	return m.pin(parent, name, true)
}

func (m *MemberRoot) remember(d *memberDirectory, name string) error {
	st, err := d.root.Lstat(name)
	if err != nil || !st.Mode().IsRegular() || !postOperationOwner(st, true) {
		return ErrMemberRoot
	}
	d.entries[name] = st
	return nil
}

// Validate checks the path-to-inode chain and a closed inventory of member-owned
// entries. It never reads arbitrary imported state or follows substituted links.
func (m *MemberRoot) Validate() error {
	if m == nil || len(m.dirs) == 0 {
		return ErrMemberRoot
	}
	for _, d := range m.dirs {
		st, err := d.root.Stat(".")
		if err != nil || !os.SameFile(st, d.info) || !postOperationOwner(st, d.private) {
			return ErrMemberRoot
		}
		if d.parent != nil {
			linked, err := d.parent.Lstat(d.name)
			if err != nil || !linked.IsDir() || !os.SameFile(linked, d.info) {
				return ErrMemberRoot
			}
		}
		if !d.private {
			continue
		}
		f, err := d.root.Open(".")
		if err != nil {
			return ErrMemberRoot
		}
		entries, err := f.ReadDir(-1)
		f.Close()
		if err != nil || len(entries) != len(d.entries) {
			return ErrMemberRoot
		}
		for _, entry := range entries {
			want, ok := d.entries[entry.Name()]
			got, err := d.root.Lstat(entry.Name())
			if !ok || err != nil || !os.SameFile(want, got) || !postOperationOwner(got, true) || got.Mode()&os.ModeSymlink != 0 {
				return ErrMemberRoot
			}
		}
	}
	return nil
}

// write uses the existing fsync/link-without-replacement pattern for publication;
// only our secret-free journal is atomically replaced within its pinned directory.
func (m *MemberRoot) write(d *memberDirectory, name string, data []byte, replace bool) error {
	if err := m.Validate(); err != nil {
		return err
	}
	tmp := ".member-" + rand.Text()
	f, err := openPostOperationFile(d.root, tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrMemberRoot
	}
	defer d.root.Remove(tmp)
	defer f.Close()
	if _, err = f.Write(data); err != nil || f.Sync() != nil || f.Close() != nil {
		return ErrMemberRoot
	}
	if replace {
		err = d.root.Rename(tmp, name)
	} else {
		err = d.root.Link(tmp, name)
	}
	if err != nil || m.remember(d, name) != nil {
		return ErrMemberRoot
	}
	if d.root.Remove(tmp) != nil && !replace {
		return ErrMemberRoot
	}
	if syncPostRoot(d.root) != nil {
		return ErrMemberRoot
	}
	return m.Validate()
}

func (m *MemberRoot) Guard() *CredentialGuard { return m.guard }

func (m *MemberRoot) WriteJournal(data []byte) error {
	return m.write(m.base, "registration.pending.json", data, true)
}

// CheckMemberRegistration fences ordinary use of an incompletely published
// member root. Existing registrations keep their per-identity journals; only
// the opt-in fresh-root transaction owns this config-root completion journal.
func CheckMemberRegistration(p Paths, want ConnectionBinding) error {
	path := filepath.Join(p.ConfigRoot(), "registration.pending.json")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if checkSecretFile(path) != nil {
		return ErrMemberRoot
	}
	data, err := os.ReadFile(path)
	var j struct {
		Binding    ConnectionBinding `json:"binding"`
		Stage      string            `json:"stage"`
		HTTPStatus int               `json:"http_status,omitempty"`
		ErrCode    string            `json:"errcode,omitempty"`
	}
	if err != nil || DecodeConnectionJSON(data, &j) != nil || j.Stage != "complete" || j.Binding.Fingerprint() != want.Fingerprint() {
		return ErrMemberRoot
	}
	return nil
}

func (m *MemberRoot) publishCredentials(c Credentials) error {
	if ValidateBoundCredentials(c, m.Binding) != nil || !m.minimal(c.ConnectionBinding.Connection) {
		return ErrMemberRoot
	}
	data, err := marshalCredentials(c)
	if err != nil {
		return ErrMemberRoot
	}
	return m.write(m.metadata, m.name+".json", append(data, '\n'), false)
}

func (m *MemberRoot) minimal(c Connection) bool {
	return c.AdminRegistration == nil && c.RegistrationTokenRef == nil && c.Store == (CredentialStore{Backend: "file"}) && c.CardRoom == m.Connection.CardRoom
}

func (m *MemberRoot) PublishRegistry() error {
	r := ConnectionRegistry{SchemaVersion: 1, Current: m.profile, Profiles: map[string]Connection{m.profile: m.Connection}}
	if r.Validate() != nil {
		return ErrMemberRoot
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return ErrMemberRoot
	}
	return m.write(m.base, "connections.json", append(data, '\n'), false)
}

// Verify uses the pinned files independently of the values sent for publication.
func (m *MemberRoot) Verify() error {
	if m.Validate() != nil {
		return ErrMemberRoot
	}
	data, err := m.base.root.ReadFile("connections.json")
	var r ConnectionRegistry
	if err != nil || DecodeConnectionJSON(data, &r) != nil || r.Validate() != nil || r.Current != m.profile || len(r.Profiles) != 1 {
		return ErrMemberRoot
	}
	b, err := r.Profiles[r.Current].Binding(m.name, false)
	if err != nil || b.Fingerprint() != m.Binding.Fingerprint() || !m.minimal(b.Connection) {
		return ErrMemberRoot
	}
	data, err = m.metadata.root.ReadFile(m.name + ".json")
	var c Credentials
	if err != nil || DecodeConnectionJSON(data, &c) != nil || ValidateBoundCredentials(c, b) != nil || c.AccessToken == "" || !m.minimal(c.ConnectionBinding.Connection) {
		return ErrMemberRoot
	}
	return m.Validate()
}

func (m *MemberRoot) Close() {
	if m.guard != nil {
		m.guard.Close()
	}
	for i := len(m.dirs) - 1; i >= 0; i-- {
		m.dirs[i].root.Close()
	}
	m.dirs = nil
}
