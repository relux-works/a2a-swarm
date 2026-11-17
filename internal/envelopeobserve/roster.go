package envelopeobserve

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/swarma-envelope/record"
	"github.com/relux-works/swarma-envelope/verifier"
)

const maxRosterBytes = 1 << 20

var ErrRoster = errors.New("host envelope roster unavailable")

// Load reads only an explicitly selected, root-owned, protected local file.
// Writable/symlink paths and unsupported ownership platforms are unavailable.
func Load(path string) (*Observer, error) {
	raw, err := ReadProtectedFile(path, maxRosterBytes)
	if err != nil {
		return nil, ErrRoster
	}
	return ParseRoster(raw)
}

// ReadProtectedFile shares the roster's root-owned, non-writable, no-symlink
// storage boundary with other host policy readers. It authenticates storage,
// not the schema; callers must strictly parse the bounded returned snapshot.
func ReadProtectedFile(path string, maxBytes int) ([]byte, error) {
	return readProtectedFile(path, maxBytes, os.Lstat, func(path string) (protectedFile, error) { return os.Open(path) })
}

type protectedFile interface {
	io.Reader
	Stat() (os.FileInfo, error)
	Close() error
}

func readProtectedFile(path string, maxBytes int, lstat func(string) (os.FileInfo, error), open func(string) (protectedFile, error)) ([]byte, error) {
	if maxBytes < 0 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrRoster
	}
	if err := protectedPath(path, lstat); err != nil {
		return nil, ErrRoster
	}
	f, err := open(path)
	if err != nil {
		return nil, ErrRoster
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !protectedInfo(st, false) {
		return nil, ErrRoster
	}
	raw, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil || len(raw) > maxBytes {
		return nil, ErrRoster
	}
	return raw, nil
}

func protectedPath(path string, lstat func(string) (os.FileInfo, error)) error {
	for current := path; ; current = filepath.Dir(current) {
		st, err := lstat(current)
		if err != nil || !protectedInfo(st, current != path) {
			return ErrRoster
		}
		if current == filepath.Dir(current) {
			return nil
		}
	}
}

// ParseRoster decodes a previously authenticated host snapshot. It does not
// establish storage provenance; production callers must use Load.
// Version 1 and verifier facts retain the pinned bounded codec; the larger
// version 2 author list uses strict bounded JSON.
func ParseRoster(raw []byte) (*Observer, error) {
	m, err := decodeRosterDocument(raw)
	if err != nil || len(m) != 3 {
		return nil, ErrRoster
	}
	switch m["version"] {
	case rosterVersion:
		return parseRosterV1(m)
	case rosterVersionV2:
		return parseRosterV2(m)
	default:
		return nil, ErrRoster
	}
}

func parseRosterV1(m map[string]any) (*Observer, error) {
	authors, ok := m["authors"].(map[string]any)
	if !ok || len(authors) == 0 || len(authors) > 4096 {
		return nil, ErrRoster
	}
	o := &Observer{authors: make(map[string]string)}
	keys := []any{}
	for sender, v := range authors {
		key, ok := v.(string)
		if !ok || !matrix.ValidVerificationID(sender, '@') || record.ValidateKey(key) != nil {
			return nil, ErrRoster
		}
		if !strings.HasPrefix(key, "ed25519:") && !strings.HasPrefix(key, "bip340:") {
			return nil, ErrRoster
		}
		for _, ch := range sender {
			if ch < 0x21 || ch > 0x7e {
				return nil, ErrRoster
			}
		}
		o.authors[sender] = key
		keys = append(keys, key)
	}
	return loadRosterFacts(o, m["facts"], keys)
}

func loadRosterFacts(o *Observer, value any, keys []any) (*Observer, error) {
	facts, ok := value.(map[string]any)
	if !ok {
		return nil, ErrRoster
	}
	var err error
	c := &o.context
	slots := map[string]*verifier.Evidence{
		"audience": &c.Audience, "service_root": &c.ServiceRoot,
		"agent_revocation_root": &c.AgentRevocationRoot, "operator_root": &c.OperatorRoot,
		"operator_owner": &c.OperatorOwner, "attestations": &c.Attestations,
		"operator_attestations": &c.DeviceAttestations, "active_generations": &c.CurrentGenerations,
		"revocations": &c.SignedSnapshots, "time": &c.Time,
		"watermark_available": &c.WatermarkAvailable, "watermark": &c.Watermark,
		"id_journal_available": &c.IDJournalAvailable, "seen": &c.IDJournal,
		"signer_facts": &c.SignerFacts, "commit_signing_key": &c.CommitSigningKey,
		"enrollment_publisher": &c.EnrollmentPublisher,
	}
	for name, v := range facts {
		slot, known := slots[name]
		if !known {
			return nil, ErrRoster
		}
		// Signed records are canonical unpadded base64url in the host JSON; the API takes bytes.
		if name == "attestations" || name == "operator_attestations" || name == "seen" {
			rows, ok := v.(map[string]any)
			if !ok {
				return nil, ErrRoster
			}
			for id, encoded := range rows {
				text, ok := encoded.(string)
				if !ok {
					return nil, ErrRoster
				}
				b, err := record.DecodeBase64(text, -1)
				if err != nil {
					return nil, ErrRoster
				}
				rows[id] = b
			}
		}
		if name == "revocations" {
			rows, ok := v.(map[string]any)
			if !ok {
				return nil, ErrRoster
			}
			for _, v := range rows {
				row, ok := v.(map[string]any)
				if !ok {
					return nil, ErrRoster
				}
				if encoded, present := row["snapshot"]; present {
					text, ok := encoded.(string)
					if !ok {
						return nil, ErrRoster
					}
					b, err := record.DecodeBase64(text, -1)
					if err != nil {
						return nil, ErrRoster
					}
					row["snapshot"] = b
				}
			}
		}
		e, err := verifier.Present(v)
		if err != nil {
			return nil, ErrRoster
		}
		*slot = e
	}
	c.RequiredRoles, err = verifier.Present([]any{"author"})
	if err != nil {
		return nil, ErrRoster
	}
	c.AdmittedKeys, err = verifier.Present(keys)
	if err != nil {
		return nil, ErrRoster
	}
	return o, nil
}
