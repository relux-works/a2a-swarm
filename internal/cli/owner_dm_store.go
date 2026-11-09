package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type ownerDMPin struct {
	Mode  string `json:"mode"`
	Owner string `json:"owner"`
}
type ownerDMConfig struct {
	V          int    `json:"v"`
	Generation uint64 `json:"generation"`
	Active     bool   `json:"active"`
	Pending    bool   `json:"pending_join"`
	Owner      string `json:"owner"`
	Room       string `json:"room"`
	Tag        bool   `json:"tag"`
	Mirror     bool   `json:"mirror"`
	Render     string `json:"render"`
	SubID      string `json:"sub_id"`
	BoundBy    string `json:"bound_by_event"`
}
type ownerDMMutation struct {
	ID         string `json:"id"`
	Room       string `json:"room"`
	Owner      string `json:"owner"`
	TS         int64  `json:"ts"`
	Accepted   int64  `json:"accepted"`
	Generation uint64 `json:"generation"`
}
type ownerDMBindings struct {
	V          int                        `json:"v"`
	Watermarks map[string]ownerDMMutation `json:"watermarks"`
	Records    []ownerDMMutation          `json:"records"`
}
type ownerDMFiles struct{ base string }

func ownerFiles(p config.Paths, name string) ownerDMFiles {
	return ownerDMFiles{filepath.Join(p.Base, "owner-dm", name)}
}
func (f ownerDMFiles) path(ext string) string { return f.base + "." + ext }

var ownerMXID = regexp.MustCompile(`^@[^:\s]+:[^\s]+$`)
var ownerRoomID = regexp.MustCompile(`^![^\s]+$`)

// Files are independent of legacy state, and reads never provision a directory.
func ownerRead(path string) ([]byte, os.FileInfo, error) {
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		return nil, st, fmt.Errorf("%s: expected regular mode 0600 file", path)
	}
	b, err := os.ReadFile(path)
	return b, st, err
}
func (f ownerDMFiles) pin() ownerDMPin {
	b, _, err := ownerRead(f.path("pin"))
	var p ownerDMPin
	if err != nil || ownerObject(b, map[string]byte{"mode": 's', "owner": 's'}, &p) != nil || !ownerMXID.MatchString(p.Owner) || (p.Mode != "host" && p.Mode != "owner-channel") {
		return ownerDMPin{Mode: "host"}
	}
	return p
}

// Required fields are checked in the same selected raw map used for decoding;
// null must not become a valid false, zero or empty string.
func ownerObject(b []byte, shape map[string]byte, out any) error {
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil || raw == nil {
		return errors.New("expected JSON object")
	}
	for k, kind := range shape {
		v := bytes.TrimSpace(raw[k])
		if len(v) == 0 || bytes.Equal(v, []byte("null")) {
			return fmt.Errorf("missing or null %s", k)
		}
		good := false
		switch kind {
		case 's':
			var x string
			good = json.Unmarshal(v, &x) == nil
		case 'b':
			var x bool
			good = json.Unmarshal(v, &x) == nil
		case 'i':
			var x uint64
			good = json.Unmarshal(v, &x) == nil
		}
		if !good {
			return fmt.Errorf("invalid type for %s", k)
		}
	}
	// Re-marshal the selected fields so validation and consumption cannot diverge.
	selected, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(selected, out)
}
func (f ownerDMFiles) load() (*ownerDMConfig, os.FileInfo, error) {
	b, st, err := ownerRead(f.path("json"))
	if err != nil {
		return nil, st, fmt.Errorf("invalid owner-dm config %s: %w", f.path("json"), err)
	}
	if st == nil {
		return nil, nil, nil
	}
	var c ownerDMConfig
	shape := map[string]byte{"v": 'i', "generation": 'i', "active": 'b', "pending_join": 'b', "owner": 's', "room": 's', "tag": 'b', "mirror": 'b', "render": 's', "sub_id": 's', "bound_by_event": 's'}
	err = ownerObject(b, shape, &c)
	if err == nil && (c.V != 1 || c.Generation == 0 || (c.Active && c.Pending) || !ownerMXID.MatchString(c.Owner) || !ownerRoomID.MatchString(c.Room) || (c.Render != "in-frame" && c.Render != "outside-frame")) {
		err = errors.New("invalid version or field value")
	}
	if err != nil {
		return nil, st, fmt.Errorf("invalid owner-dm config %s: %w", f.path("json"), err)
	}
	return &c, st, nil
}
func ownerWrite(ctx context.Context, path string, v any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeSettings(path, append(b, '\n'), 0600)
}

// A durable reservation closes archive while an as-yet unknown principal
// room ID is being created. It is never automatically cleared after uncertainty.
const principalArchiveReservation = "*principal-dm-creation*"

func (f ownerDMFiles) exclusions() (map[string]bool, error) {
	b, st, err := ownerRead(f.path("archive-exclude"))
	if err != nil {
		return nil, fmt.Errorf("archive: %s: %w", f.path("archive-exclude"), err)
	}
	out := map[string]bool{}
	if st == nil {
		return out, nil
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		if line != principalArchiveReservation && !ownerRoomID.MatchString(line) {
			return nil, fmt.Errorf("archive: invalid %s", f.path("archive-exclude"))
		}
		out[line] = true
	}
	return out, nil
}
func (f ownerDMFiles) exclude(ctx context.Context, room string) error {
	rooms, err := f.exclusions()
	if err != nil {
		return err
	}
	rooms[room] = true
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeSettings(f.path("archive-exclude"), []byte(strings.Join(sortedKeys(rooms), "\n")+"\n"), 0600)
}
func (f ownerDMFiles) bindings() (ownerDMBindings, error) {
	out := ownerDMBindings{V: 1, Watermarks: map[string]ownerDMMutation{}}
	b, st, err := ownerRead(f.path("bindings"))
	if err != nil {
		return out, err
	}
	if st == nil {
		return out, nil
	}
	if json.Unmarshal(b, &out) != nil || out.V != 1 || out.Watermarks == nil {
		return out, errors.New("invalid owner-dm binding store")
	}
	return out, nil
}
func (b *ownerDMBindings) accept(m ownerDMMutation, now time.Time) {
	b.Watermarks[m.Owner] = m
	b.Records = append(b.Records, m)
	// Keep recent claims and at least the last mutation of each generation for audit.
	last := map[uint64]int{}
	for i, r := range b.Records {
		last[r.Generation] = i
	}
	kept := b.Records[:0]
	for i, r := range b.Records {
		if i >= len(b.Records)-1024 || r.Accepted >= now.Add(-17*time.Minute).UnixMilli() || last[r.Generation] == i {
			kept = append(kept, r)
		}
	}
	b.Records = kept
}
func ownerFingerprint(c *ownerDMConfig) string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func (a *App) warnOwnerDM() {
	name, err := a.identity()
	if err != nil {
		return
	}
	p, err := a.paths()
	if err != nil {
		return
	}
	_, _, err = ownerFiles(p, name).load()
	if err != nil {
		fmt.Fprintln(a.Stderr, "a2a: "+sanitizeLine(err.Error())+"; owner-dm feature off")
	}
}
