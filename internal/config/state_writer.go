package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const stateProtocol = 2

// StateWriter is consistency data under a trusted UID, not authentication or
// proof of the installed images, owning session, or complete writer history.
type StateWriter struct {
	Protocol    int       `json:"protocol"`
	Seal        string    `json:"seal"`
	ForeignSeen time.Time `json:"foreign_seen,omitzero"`
	ambiguous   bool
}

func (w *StateWriter) UnmarshalJSON(data []byte) error {
	type plain StateWriter
	var v plain
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return errStateCanonical
	}
	var keys []string
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return err
		}
		key, ok := t.(string)
		if !ok {
			return errStateCanonical
		}
		for _, prior := range keys {
			if strings.EqualFold(key, prior) {
				v.ambiguous = true
			}
		}
		keys = append(keys, key)
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return err
		}
	}
	*w = StateWriter(v)
	return nil
}

var errStateCanonical = errors.New("state canonical encoding unavailable")
var ErrStateWriteUncertain = errors.New("state persistence outcome uncertain")

func syncStateDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr, closeErr := f.Sync(), f.Close()
	return errors.Join(syncErr, closeErr)
}

// Persist a newly created state-directory entry before the file, then the
// replacement entry after temp-file sync/rename. A post-rename failure is
// explicitly uncertain: new bytes may exist, but no successful durable fence
// or rollback is claimed. The Host v2 route retains its stronger pinned-root
// writer and existing uncertainty taxonomy.
func writeOrdinaryStateAtomic(path, name string, data []byte, syncDir func(string) error) error {
	dir := filepath.Dir(path)
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := writeSecretAtomic(path, name, data); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return ErrStateWriteUncertain
	}
	return nil
}

// canonicalState seals every member except writer.seal, including writer
// protocol/foreign_seen and all opaque top-level values. Objects sort recursively by
// encoding/json's UTF-8 key order; arrays retain order. UseNumber retains exact
// numeric lexemes (1, 1.0, 1e0 and -0 differ), with no float rounding. JSON
// strings use encoding/json escaping, including HTML escaping. Whitespace and
// object member order do not matter. Duplicate object keys are invalid.
func canonicalState(data []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	v, err := canonicalValue(d)
	if err != nil {
		return nil, errStateCanonical
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errStateCanonical
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errStateCanonical
	}
	writers := 0
	for k := range m {
		if strings.EqualFold(k, "writer") {
			w, ok := m[k].(map[string]any)
			if !ok {
				return nil, errStateCanonical
			}
			seals := 0
			for field := range w {
				if strings.EqualFold(field, "seal") {
					delete(w, field)
					seals++
				}
			}
			if seals > 1 {
				return nil, errStateCanonical
			}
			writers++
		}
	}
	if writers > 1 {
		return nil, errStateCanonical
	}
	return json.Marshal(m)
}

func canonicalValue(d *json.Decoder) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		m := make(map[string]any)
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return nil, err
			}
			key, ok := k.(string)
			if !ok {
				return nil, errStateCanonical
			}
			if _, exists := m[key]; exists {
				return nil, errStateCanonical
			}
			v, err := canonicalValue(d)
			if err != nil {
				return nil, err
			}
			m[key] = v
		}
		if end, err := d.Token(); err != nil || end != json.Delim('}') {
			return nil, errStateCanonical
		}
		return m, nil
	case json.Delim('['):
		a := make([]any, 0)
		for d.More() {
			v, err := canonicalValue(d)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		if end, err := d.Token(); err != nil || end != json.Delim(']') {
			return nil, errStateCanonical
		}
		return a, nil
	}
	return t, nil
}

func stateDigest(data []byte) (string, error) {
	b, err := canonicalState(data)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func validWriterSeal(s State) bool {
	if s.Writer == nil || s.Writer.ambiguous || s.Writer.Protocol < stateProtocol || !lowerHex(s.Writer.Seal, 64) {
		return false
	}
	digest, err := stateDigest(s.rawState)
	return err == nil && s.Writer.Seal == digest
}

func ordinaryWriterFence(old State, now time.Time) time.Time {
	fence, _ := ordinaryWriterFenceSealed(old, now)
	return fence
}

// ordinaryWriterFenceSealed is ordinaryWriterFence that also reports the result
// of its own seal verification of this exact disk input. The fence decision
// is unchanged; the result is not cached beyond the caller's lock.
func ordinaryWriterFenceSealed(old State, now time.Time) (time.Time, bool) {
	// Verify every disk input before a callback/effect, including inputs that
	// already carry a sticky fence. Retaining the timestamp is not a shortcut
	// around the compatible-writer verification rule.
	valid := validWriterSeal(old)
	// Only LoadState's missing-file result has no disk input. An existing
	// empty/null/unsealed State is foreign history, even with no typed fields.
	if len(old.rawState) == 0 {
		return time.Time{}, false
	}
	if old.Writer != nil && !old.Writer.ForeignSeen.IsZero() {
		return old.Writer.ForeignSeen.UTC(), valid
	}
	if !valid {
		return now.UTC(), false
	}
	return time.Time{}, true
}

// unchangedSealedState reports whether sealing s would reproduce old's exact
// disk input. The caller must already have verified old's seal under the same
// lock. canonicalState drops writer.seal, so marshalling s with old's seal
// yields bytes whose digest equals that of old's input exactly when they are
// byte-identical; that digest is old's verified seal, so sealedState(s) would
// emit the same bytes. Any difference, or a marshal error, reports false and
// leaves the ordinary sealed write (and its error) to the caller.
func unchangedSealedState(old, s State) bool {
	if old.Writer == nil || s.Writer == nil {
		return false
	}
	w := *s.Writer
	w.Seal = old.Writer.Seal
	s.Writer = &w
	data, err := marshalState(s)
	if err != nil {
		return false
	}
	n := len(data)
	return len(old.rawState) == n+1 && old.rawState[n] == '\n' && bytes.Equal(old.rawState[:n], data)
}

// stateSyncer holds the durability operations of an elided ordinary write.
type stateSyncer struct {
	file func(*os.File) error
	dir  func(string) error
}

// ordinaryStateSync is a package-private seam: production code never replaces
// it, and no flag or environment value can reach it. Package tests swap it to
// observe or fail the file and directory syncs of the elided path.
var ordinaryStateSync = stateSyncer{file: (*os.File).Sync, dir: syncStateDirectory}

// elideUnchangedOrdinaryState establishes, under the state lock, the
// postconditions an ordinary replacement with byte-identical content would:
//   - the path names a 0600 regular file whose content, read through one
//     descriptor opened without following links, is exactly input;
//   - that file's content is synced through the same verified descriptor, since
//     a valid seal and equal bytes do not prove that an earlier writer's file
//     sync completed;
//   - the path still names that synced file;
//   - the parent and state directory entries are synced as
//     writeOrdinaryStateAtomic does.
//
// It returns elided=false with a nil error when the unchanged premise does not
// hold or the file sync fails. Nothing is then claimed durable, and the caller
// runs the ordinary replacement. Directory sync failures keep the replacement's
// taxonomy: a parent failure is returned as is, and a state-directory failure
// is ErrStateWriteUncertain.
func elideUnchangedOrdinaryState(path string, input []byte, sync stateSyncer) (bool, error) {
	if checkSecretFile(path) != nil {
		return false, nil
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return false, nil
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode().Perm() != SecretFileMode || opened.Size() != int64(len(input)) {
		return false, nil
	}
	// One spare byte detects growth after the size check: only a short read of
	// exactly len(input) bytes, ending at EOF, can match.
	data := make([]byte, len(input)+1)
	n, err := io.ReadFull(f, data)
	if err != io.ErrUnexpectedEOF || n != len(input) || !bytes.Equal(data[:n], input) {
		return false, nil
	}
	if sync.file(f) != nil {
		return false, nil
	}
	if current, err := os.Lstat(path); err != nil || !os.SameFile(opened, current) {
		return false, nil
	}
	dir := filepath.Dir(path)
	if err := sync.dir(filepath.Dir(dir)); err != nil {
		return false, err
	}
	if err := sync.dir(dir); err != nil {
		return false, ErrStateWriteUncertain
	}
	return true, nil
}

func sealedState(s State) ([]byte, error) {
	data, err := marshalState(s)
	if err != nil {
		return nil, err
	}
	digest, err := stateDigest(data)
	if err != nil {
		return nil, err
	}
	w := *s.Writer
	w.Seal = digest
	s.Writer = &w
	return marshalState(s)
}

// rejectAuthorityAliases retains the existing non-authority typed case-fold
// semantics, but refuses duplicate writer/monitor spellings before decoding.
func rejectAuthorityAliases(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	t, err := d.Token()
	if err != nil {
		return err
	}
	if t != json.Delim('{') {
		return nil
	} // legacy decoder handles null/types
	var seen []string
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		k, ok := key.(string)
		if !ok {
			return ErrMonitorState
		}
		if strings.EqualFold(k, "monitor") || strings.EqualFold(k, "writer") || strings.EqualFold(k, "monitor_owner") || strings.EqualFold(k, "monitor_config") || strings.EqualFold(k, "migration") {
			for _, prior := range seen {
				if strings.EqualFold(prior, k) {
					return ErrMonitorState
				}
			}
			seen = append(seen, k)
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return err
		}
		if (strings.EqualFold(k, "monitor") || strings.EqualFold(k, "monitor_owner") || strings.EqualFold(k, "monitor_config") || strings.EqualFold(k, "migration")) && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return ErrMonitorState
		}
	}
	return nil
}
