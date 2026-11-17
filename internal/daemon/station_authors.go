package daemon

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/envelopeobserve"
	"github.com/relux-works/swarma-envelope/record"
)

const maxStationAuthorsBytes = 32768

var ErrStationAuthors = errors.New("station_authors_unavailable")

// StationAuthors is an authenticated host snapshot, keyed only by kernel UID.
// An empty snapshot is available; nil plus an error is unavailable, not empty.
type StationAuthors map[uint32]string

// ParseStationAuthors applies the roster's pinned strict JSON codec. Storage
// provenance is the caller's obligation; production must use LoadStationAuthors.
func ParseStationAuthors(raw []byte) (StationAuthors, error) {
	v, err := record.Parse(raw, maxStationAuthorsBytes, 16)
	if err != nil {
		return nil, ErrStationAuthors
	}
	m, ok := v.(map[string]any)
	if !ok || len(m) != 2 || m["version"] != "a2a-station-authors/1" {
		return nil, ErrStationAuthors
	}
	rows, ok := m["authors"].([]any)
	if !ok {
		return nil, ErrStationAuthors
	}
	out := StationAuthors{}
	for _, value := range rows {
		row, ok := value.(map[string]any)
		if !ok || len(row) != 2 {
			return nil, ErrStationAuthors
		}
		uid, ok := row["uid"].(int64)
		key, keyOK := row["author_key"].(string)
		if !ok || uid < 0 || uid > 1<<32-1 || !keyOK || !strings.HasPrefix(key, "bip340:") || record.ValidateKey(key) != nil {
			return nil, ErrStationAuthors
		}
		if _, duplicate := out[uint32(uid)]; duplicate {
			return nil, ErrStationAuthors
		}
		out[uint32(uid)] = key
	}
	return out, nil
}

// LoadStationAuthors reuses the roster's protected host-file reader, including
// protection of every ancestor. No last-known-good or unprotected fallback.
func LoadStationAuthors(path string) (StationAuthors, error) {
	raw, err := envelopeobserve.ReadProtectedFile(path, maxStationAuthorsBytes)
	if err != nil {
		return nil, ErrStationAuthors
	}
	return ParseStationAuthors(raw)
}

// StationAuthorsPath reads the daemon-owned selector, not station configuration.
// Only absence means disabled. Every other failure means configured but unknown
// and must close publishing for all UIDs, including otherwise unmapped stations.
func StationAuthorsPath(home string) (path string, configured bool, err error) {
	name := filepath.Join(home, "station-authors.json")
	st, err := os.Lstat(name)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil || !st.Mode().IsRegular() || st.Size() > 4096 {
		return "", true, ErrStationAuthors
	}
	f, err := os.Open(name)
	if err != nil {
		return "", true, ErrStationAuthors
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", true, ErrStationAuthors
	}
	v, err := record.Parse(raw, 4096, 2)
	m, ok := v.(map[string]any)
	if err != nil || !ok || len(m) != 1 {
		return "", true, ErrStationAuthors
	}
	path, ok = m["authors_path"].(string)
	if !ok || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", true, ErrStationAuthors
	}
	return path, true, nil
}
