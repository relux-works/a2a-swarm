package e2ee

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/relux-works/a2a-swarm/internal/redact"
)

// Inventory is operation-scoped. A caller must discard it with the working
// snapshot; it must not accumulate historical snapshots in a daemon-wide cache.
// Raw secrets are copied once and deduplicated. Encodings are generated only
// during scrub and only if they can fit in the diagnostic, so a 64MiB snapshot
// doesn't allocate all its encodings on every seal. No API returns secret values.
// Go cannot promise RAM erasure. U2 must route CLI diagnostics through Scrub.
type Inventory struct {
	mu     sync.Mutex
	values []string
	seen   map[[32]byte]bool
}

func (s *Inventory) String() string   { return "<e2ee-secret-inventory>" }
func (s *Inventory) GoString() string { return s.String() }

func (s *Inventory) Register(secret []byte) {
	if s == nil || len(secret) == 0 {
		return
	}
	hash := sha256.Sum256(secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[[32]byte]bool{}
	}
	if !s.seen[hash] {
		s.seen[hash] = true
		s.values = append(s.values, string(secret))
	}
}
func (s *Inventory) Scrub(message string) string {
	if s == nil {
		return redact.String(message)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	encodings := make([]string, 0, len(s.values))
	for _, raw := range s.values {
		if len(raw) > len(message) {
			continue
		}
		// redact.Set intentionally ignores values under six bytes; crypto secrets
		// are at least 16 bytes. Suppress short library diagnostics separately.
		encodings = append(encodings, raw)
		quoted, _ := json.Marshal(raw)
		encodings = append(encodings, string(quoted), strings.TrimSuffix(strings.TrimPrefix(string(quoted), `"`), `"`), url.QueryEscape(raw))
		b := []byte(raw)
		if base64.RawStdEncoding.EncodedLen(len(b)) <= len(message) {
			encodings = append(encodings, base64.StdEncoding.EncodeToString(b), base64.RawStdEncoding.EncodeToString(b), base64.URLEncoding.EncodeToString(b), base64.RawURLEncoding.EncodeToString(b))
		}
		if hex.EncodedLen(len(b)) <= len(message) {
			encodings = append(encodings, hex.EncodeToString(b), strings.ToUpper(hex.EncodeToString(b)))
		}
	}
	sort.Slice(encodings, func(i, j int) bool { return len(encodings[i]) > len(encodings[j]) })
	var set redact.Set
	set.Add(encodings...)
	return set.Scrub(message)
}

// ObserveJSON inventories serialized fields and decoded byte fields (Go encodes
// []byte as standard base64). Public fields are harmless extras. It does not
// reflect through private crypto objects or expose library error strings.
func (s *Inventory) ObserveJSON(data []byte) {
	if s == nil {
		return
	}
	s.Register(data)
	var v any
	if json.Unmarshal(data, &v) != nil {
		return
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			s.Register([]byte(x))
			if b, e := base64.StdEncoding.DecodeString(x); e == nil {
				s.Register(b)
			}
		case []any:
			// Fixed byte arrays (not []byte) serialize as JSON numbers, e.g.
			// Megolm's 128-byte ratchet data. Register their binary encodings.
			if len(x) >= 16 {
				decoded := make([]byte, len(x))
				valid := true
				for i, v := range x {
					n, ok := v.(float64)
					if !ok || n < 0 || n > 255 || n != float64(byte(n)) {
						valid = false
						break
					}
					decoded[i] = byte(n)
				}
				if valid {
					s.Register(decoded)
				}
			}
			for _, v := range x {
				walk(v)
			}
		case map[string]any:
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(v)
}
