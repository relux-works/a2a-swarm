package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// LoadCredentialMetadata applies the same identity/file guards as the legacy
// file store, but permits a scrubbed token. It never enumerates identities.
func LoadCredentialMetadata(p Paths, name string, human bool) (Credentials, error) {
	if err := ValidateName(name); err != nil {
		return Credentials{}, err
	}
	path := p.AgentFile(name)
	if human {
		path = p.HumanFile(name)
	}
	if err := checkSecretFile(path); err != nil {
		// Preserve the agent/human diagnostic without relaxing its boundary.
		if !human {
			_, err = LoadCredentials(p, name)
		}
		return Credentials{}, err
	}
	return readCredentialMetadata(path)
}

// RewriteCredentialMetadata holds the existing identity file lock through
// readback and atomic replacement. A failed callback leaves the old file intact.
// The callback must clear secrets only after verifying the selected store.
func RewriteCredentialMetadata(p Paths, name string, human bool, fn func(*Credentials) error) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	path := p.AgentFile(name)
	if human {
		path = p.HumanFile(name)
	}
	unlock, err := lockFile(filepath.Join(filepath.Dir(path), "."+name+".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	c, err := LoadCredentialMetadata(p, name, human)
	if err != nil {
		return err
	}
	if err := fn(&c); err != nil {
		return err
	}
	if c.AccessToken != "" || c.Password != "" {
		return fmt.Errorf("credential scrub refused: secrets remain")
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// Recheck before rename: never replace a symlink introduced by a cooperating
	// caller. External same-UID filesystem mutation is not an isolation boundary.
	if err := checkSecretFile(path); err != nil {
		return err
	}
	if err := writeSecretAtomic(path, name, append(data, '\n')); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
