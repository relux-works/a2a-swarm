package config

import (
	"os"
	"path/filepath"
)

// CredentialGuard owns the stable agent credential lock. Human mutations also
// take that lock first, so moving a file cannot bypass an agent migration fence.
// Callers retain it over provider writes and all reservation cleanup.
type CredentialGuard struct {
	p       Paths
	name    string
	human   bool
	release func()
}

func LockCredentialMutation(p Paths, name string, human bool) (*CredentialGuard, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	dir := filepath.Dir(p.AgentFile(name))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	release, err := lockFile(filepath.Join(dir, "."+name+".lock"))
	if err != nil {
		return nil, err
	}
	g := &CredentialGuard{p: p, name: name, human: human, release: release}
	if human {
		humanDir := filepath.Dir(p.HumanFile(name))
		if err := os.MkdirAll(humanDir, 0700); err != nil {
			g.Close()
			return nil, err
		}
		releaseHuman, err := lockFile(filepath.Join(humanDir, "."+name+".lock"))
		if err != nil {
			g.Close()
			return nil, err
		}
		g.release = func() { releaseHuman(); release() }
	}
	if err := CheckIdentityMigration(p, name); err != nil {
		g.Close()
		return nil, err
	}
	return g, nil
}

func (g *CredentialGuard) Close() {
	if g.release != nil {
		g.release()
		g.release = nil
	}
}

// PublishBoundCredentials requires this still-held guard, rather than opening
// a second flock while registration already owns the credential transaction.
func (g *CredentialGuard) PublishBoundCredentials(c Credentials) error {
	if g.release == nil {
		return ErrMigrationStateUnavailable
	}
	return publishBoundCredentials(g.p, g.name, g.human, c)
}

// CredentialFile retains the guard from exclusive reservation through write or
// abort. Embedding the file preserves fixture callers' direct Write/Close use.
type CredentialFile struct {
	*os.File
	guard *CredentialGuard
}

func (f *CredentialFile) Close() error {
	if f.guard.release == nil {
		return nil
	}
	err := f.File.Close()
	f.guard.Close()
	return err
}

// Abort removes only this reservation, while its credential lock is held.
func (f *CredentialFile) Abort() {
	if f.guard.release == nil {
		return
	}
	_ = os.Remove(f.Name())
	_ = f.Close()
}
