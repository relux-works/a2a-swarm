package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// OwnerRegistry lists this config root's operator owner accounts, one full
// Matrix user ID per server the owner uses. It only selects which senders the
// owner-reachability subscriptions follow; it grants no authority. It is kept
// out of connections.json, whose strict decoding would make older clients
// refuse the whole routing registry.
type OwnerRegistry struct {
	SchemaVersion int      `json:"schema_version"`
	Owners        []string `json:"owners"`
}

// MaxOwners bounds the registry; an owner rarely has more than a few servers.
const MaxOwners = 16

var ownerLocalpart = regexp.MustCompile(`^[a-z0-9._=/+-]{1,255}$`)

// ValidOwnerID reports whether id is a full Matrix user ID @localpart:server.
// Localpart or display-name forms are refused: owners are matched by the
// complete principal only.
func ValidOwnerID(id string) bool {
	local, server, ok := strings.Cut(strings.TrimPrefix(id, "@"), ":")
	return strings.HasPrefix(id, "@") && ok && ownerLocalpart.MatchString(local) && validServerName(server) && len(id) <= 255
}

// OwnerServer returns the server name of a valid owner ID.
func OwnerServer(id string) string {
	_, server, _ := strings.Cut(id, ":")
	return server
}

func (r OwnerRegistry) Validate() error {
	if r.SchemaVersion != 1 || r.Owners == nil || len(r.Owners) > MaxOwners {
		return fmt.Errorf("owner registry requires schema_version 1 and at most %d owners", MaxOwners)
	}
	seen := map[string]bool{}
	for _, o := range r.Owners {
		if !ValidOwnerID(o) {
			return errors.New("owner registry entries must be full Matrix user IDs (@localpart:server)")
		}
		if seen[o] {
			return errors.New("duplicate owner in registry")
		}
		seen[o] = true
	}
	return nil
}

func (p Paths) OwnersFile() string { return filepath.Join(p.ConfigRoot(), "owners.json") }

// LoadOwners returns nil, nil when no registry exists. A nonregular entry,
// oversized, malformed or invalid registry is an error, never an empty list.
func LoadOwners(p Paths) (*OwnerRegistry, error) {
	path := p.OwnersFile()
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("owners.json must be a regular file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) > 64<<10 {
		return nil, errors.New("owner registry too large")
	}
	var r OwnerRegistry
	if err := DecodeConnectionJSON(b, &r); err != nil {
		return nil, errors.New("invalid owners.json (strict JSON, known fields only)")
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// UpdateOwners serializes cooperating writers and replaces the registry
// atomically after validation. A callback or validation failure leaves the
// original bytes unchanged. An absent registry starts empty.
func UpdateOwners(p Paths, fn func(*OwnerRegistry) error) error {
	unlock, err := lockFile(p.OwnersFile() + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	r, err := LoadOwners(p)
	if err != nil {
		return err
	}
	if r == nil {
		r = &OwnerRegistry{SchemaVersion: 1, Owners: []string{}}
	}
	if err := fn(r); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeSecretAtomic(p.OwnersFile(), "owners", append(b, '\n'))
}
