package e2ee

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/relux-works/a2a-swarm/internal/redact"
)

// Inventory has bounded retained bytes/entries and an explicit Reset. Reset is
// permitted only after diagnostics finish and the old working set is retired.
// On overflow it retains no new values and suppresses ALL diagnostics until Reset,
// rather than forgetting an older secret or emitting unregistered new values.
// Raw secrets are copied once and deduplicated. Encodings are generated only
// during scrub and only if they can fit in the diagnostic, so a 64MiB snapshot
// doesn't allocate all its encodings on every seal. No API returns secret values.
// Go cannot promise RAM erasure. U2 must route CLI diagnostics through Scrub.
type Inventory struct {
	inventoryFormat
	mu     sync.Mutex
	values []string
	seen   map[[32]byte]bool
	used   int
	full   bool
}

const MaxInventoryBytes = 4 * MaxSnapshot
const MaxInventoryEntries = 1 << 20

func (s *Inventory) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.values)
	s.values, s.seen, s.used, s.full = nil, nil, 0, false
}

func (s *Inventory) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.full {
		return ErrCapacity
	}
	return nil
}

func (s *Inventory) Register(secret []byte) {
	if s == nil || len(secret) == 0 {
		return
	}
	hash := sha256.Sum256(secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.full {
		return
	}
	if s.seen == nil {
		s.seen = map[[32]byte]bool{}
	}
	if !s.seen[hash] {
		if len(secret) > MaxInventoryBytes-s.used || len(s.values) >= MaxInventoryEntries {
			s.full = true
			return
		}
		s.seen[hash] = true
		s.values = append(s.values, string(secret))
		s.used += len(secret)
	}
}
func (s *Inventory) Scrub(message string) string {
	if s == nil {
		return redact.String(message)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.full {
		return "<e2ee-diagnostic-suppressed>"
	}
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
		if 2*len(b)+1 <= len(message) {
			encodings = append(encodings, numericBytes(b))
		}
	}
	sort.Slice(encodings, func(i, j int) bool { return len(encodings[i]) > len(encodings[j]) })
	var set redact.Set
	set.Add(encodings...)
	return set.Scrub(message)
}

// numericBytes is the compact JSON representation of a fixed byte array, which
// differs from encoding/json's base64 representation of a []byte slice.
func numericBytes(raw []byte) string {
	out := make([]byte, 0, 4*len(raw)+2)
	out = append(out, '[')
	for i, v := range raw {
		if i > 0 {
			out = append(out, ',')
		}
		out = strconv.AppendUint(out, uint64(v), 10)
	}
	return string(append(out, ']'))
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

// observeSnapshot inventories the already-typed storage value. It preserves the
// generic JSON observer's string/base64 and fixed-array coverage without decoding
// a second full snapshot into map[string]any. Only opaque evidence needs that
// walk; the schema reflection metadata is shared with the canonical size counter.
func (s *Inventory) observeSnapshot(snapshot Snapshot, raw []byte) {
	if s == nil {
		return
	}
	s.Register(raw)
	var walk func(reflect.Value)
	walk = func(v reflect.Value) {
		if v.Type() == rawJSONType {
			s.ObserveJSON(v.Bytes())
			return
		}
		switch v.Kind() {
		case reflect.Pointer:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Struct:
			for _, field := range snapshotFields[v.Type()] {
				walk(v.Field(field.index))
			}
		case reflect.String:
			value := v.String()
			s.Register([]byte(value))
			if !utf8.ValidString(value) {
				value = string([]rune(value))
				s.Register([]byte(value))
			}
			if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
				s.Register(decoded)
			}
		case reflect.Slice:
			if v.Type().Elem().Kind() == reflect.Uint8 {
				if !v.IsNil() {
					s.Register(v.Bytes())
					s.Register([]byte(base64.StdEncoding.EncodeToString(v.Bytes())))
				}
				return
			}
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
		case reflect.Array:
			if v.Type().Elem().Kind() == reflect.Uint8 && v.Len() >= 16 {
				raw := make([]byte, v.Len())
				for i := range raw {
					raw[i] = byte(v.Index(i).Uint())
				}
				s.Register(raw)
				return
			}
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i))
			}
		case reflect.Map:
			iter := v.MapRange()
			for iter.Next() {
				walk(iter.Value())
			}
		}
	}
	walk(reflect.ValueOf(snapshot))
}
