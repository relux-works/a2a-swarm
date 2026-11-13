// Package wakemute implements local, owner-confirmed scope wake admission.
// It neither opens identity credentials nor owns mail or process lifecycle.
package wakemute

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const Version = 1
const EnvRequiredVersion = "A2A_WAKE_MUTE_REQUIRED_VERSION"
const Marker = "wake-mute.enrolled.json"
const Ledger = "wake-mute.operations.json"
const MaxBytes = 1 << 20

var ErrMuted = errors.New("scope wake muted")
var ErrUnsupported = errors.New("wake mute scope not enrolled or unsupported")
var ErrUnsafe = errors.New("unsafe or unavailable wake mute state; admission refused")
var ErrAuthority = errors.New("wake mute owner, host, run or scope mismatch")
var ErrStale = errors.New("unknown or closed wake mute operation")
var ErrUncertain = errors.New("wake mute persistence uncertain; inspect status")

type Connection struct {
	Name string `json:"name"` // Empty explicitly denotes the legacy connection.
	Path string `json:"path"` // "." or connections/<immutable connection id>.
}
type Selection struct {
	Root        string       `json:"root"`
	Connections []Connection `json:"connections"`
}
type Scope []Selection

type RootIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}
type Enrollment struct {
	Snapshot    *Store       `json:"recovery_snapshot"`
	Version     int          `json:"version"`
	Host        string       `json:"host"`
	Owner       int          `json:"owner"`
	Identity    RootIdentity `json:"root_identity"`
	Connections []Connection `json:"connections"`
}
type Operation struct {
	consent string
	roots   map[string]RootIdentity
	ID      string    `json:"id"`
	Run     string    `json:"run"`
	Host    string    `json:"host"`
	Owner   int       `json:"owner"`
	Scope   Scope     `json:"scope"`
	Created time.Time `json:"created"`
	State   string    `json:"state"` // installing, active, closing or closed
}
type Store struct {
	ConnectionGenerations map[string]uint64 `json:"connection_generations"`
	Version               int               `json:"version"`
	Generation            uint64            `json:"generation"`
	Operations            []Operation       `json:"operations"`
}
type RootStatus struct {
	ConnectionGenerations map[string]uint64 `json:"connection_generations"`
	Root                  string            `json:"root"`
	State                 string            `json:"state"`
	Generation            uint64            `json:"generation"`
	Operations            []Operation       `json:"operations"`
	Ages                  map[string]int64  `json:"age_seconds"`
	Uncertainty           string            `json:"uncertainty,omitempty"`
}
type OperationCoverage struct {
	ID         string `json:"id"`
	Covered    Scope  `json:"covered"`
	NotCovered Scope  `json:"not_covered"`
}
type Result struct {
	Operations     []OperationCoverage `json:"operation_coverage"`
	Version        int                 `json:"version"`
	Status         string              `json:"status"`
	Quiesced       bool                `json:"quiesced"` // Always false: no cancel, drain or receiver barrier.
	Scope          Scope               `json:"scope"`
	Operation      *Operation          `json:"operation,omitempty"`
	Roots          []RootStatus        `json:"roots"`
	CoverageLimits []string            `json:"coverage_limits"`
}

var Limits = []string{"Only explicitly enrolled roots, selected connections and compatible consumers are covered; admitted launch configuration must require control version 1 independently of the control files.", "Other roots, OS users, old binaries and external notification routes are not covered.", "Daemon forwarding requires both daemon configuration and station roots in the approved scope.", "Already admitted asynchronous wakes may complete; muted is not quiesced.", "Readable status establishes visibility; the original caller output and final sync acknowledgement remain unverified."}
var slug = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var idRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Normalize rejects aliases rather than allowing a changed path to broaden consent.
func Normalize(s Scope) (Scope, error) {
	if len(s) == 0 || len(s) > 32 {
		return nil, ErrAuthority
	}
	out := make(Scope, 0, len(s))
	for _, v := range s {
		if !filepath.IsAbs(v.Root) || filepath.Clean(v.Root) != v.Root || strings.ContainsAny(v.Root, "\x00\r\n\t") || len(v.Root) > 4096 || len(v.Connections) == 0 || len(v.Connections) > 64 {
			return nil, ErrAuthority
		}
		v.Connections = slices.Clone(v.Connections)
		slices.SortFunc(v.Connections, func(a, b Connection) int { return strings.Compare(a.Name, b.Name) })
		paths := map[string]bool{}
		for i, c := range v.Connections {
			if c.Name == "" {
				if c.Path != "." {
					return nil, ErrAuthority
				}
			} else if !slug.MatchString(c.Name) || !strings.HasPrefix(c.Path, "connections/") || !slug.MatchString(strings.TrimPrefix(c.Path, "connections/")) {
				return nil, ErrAuthority
			}
			if paths[c.Path] || i > 0 && c.Name == v.Connections[i-1].Name {
				return nil, ErrAuthority
			}
			paths[c.Path] = true
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b Selection) int { return strings.Compare(a.Root, b.Root) })
	for i, v := range out {
		for j := 0; j < i; j++ {
			if v.Root == out[j].Root || strings.HasPrefix(v.Root, out[j].Root+string(filepath.Separator)) {
				return nil, ErrAuthority
			}
		}
	}
	return out, nil
}
func equalScope(a, b Scope) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func NewOperation(scope Scope, run string) (Operation, error) {
	scope, err := Normalize(scope)
	if err != nil {
		return Operation{}, err
	}
	if run == "" || len(run) > 128 || strings.ContainsAny(run, "\x00\r\n\t") {
		return Operation{}, ErrAuthority
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return Operation{}, ErrAuthority
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return Operation{}, err
	}
	return Operation{ID: hex.EncodeToString(nonce[:]), Run: run, Host: host, Owner: os.Geteuid(), Scope: scope, Created: time.Now().UTC(), State: "installing"}, nil
}
func (o Operation) valid() bool {
	s, e := Normalize(o.Scope)
	return e == nil && equalScope(s, o.Scope) && idRE.MatchString(o.ID) && o.Run != "" && len(o.Run) <= 128 && o.Host != "" && o.Owner >= 0 && !o.Created.IsZero() && (o.State == "installing" || o.State == "active" || o.State == "closing" || o.State == "closed")
}
func decode(b []byte, v any) error {
	if len(b) > MaxBytes {
		return ErrUnsafe
	}
	scan := json.NewDecoder(bytes.NewReader(b))
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return ErrUnsafe
		}
		token, err := scan.Token()
		if err != nil {
			return ErrUnsafe
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{':
				keys := map[string]bool{}
				for scan.More() {
					k, e := scan.Token()
					if e != nil {
						return ErrUnsafe
					}
					key, ok := k.(string)
					key = strings.ToLower(key)
					if !ok || keys[key] {
						return ErrUnsafe
					}
					keys[key] = true
					if e = value(depth + 1); e != nil {
						return e
					}
				}
			case '[':
				for scan.More() {
					if e := value(depth + 1); e != nil {
						return e
					}
				}
			default:
				return ErrUnsafe
			}
			if _, e := scan.Token(); e != nil {
				return ErrUnsafe
			}
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := scan.Token(); err != io.EOF {
		return ErrUnsafe
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return ErrUnsafe
	}
	if d.Decode(new(any)) != io.EOF {
		return ErrUnsafe
	}
	return nil
}
func (s Store) valid() bool {
	if s.Version != Version || s.Operations == nil || s.ConnectionGenerations == nil {
		return false
	}
	ids := map[string]bool{}
	for _, o := range s.Operations {
		if !o.valid() || ids[o.ID] {
			return false
		}
		ids[o.ID] = true
	}
	return true
}
func uncertain(err error) error { return fmt.Errorf("%w: %v", ErrUncertain, err) }

func applies(operation, selection Scope) bool {
	for _, o := range operation {
		for _, s := range selection {
			if o.Root == s.Root {
				for _, c := range o.Connections {
					if slices.Contains(s.Connections, c) {
						return true
					}
				}
			}
		}
	}
	return false
}

func equalStore(a, b Store) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// Union never interprets disagreement as permission to wake or as a closed
// operation. Immutable authorization bindings must agree for a shared ID.
func recoveryStore(anchor, ledger Store) (Store, error) {
	out := Store{Version: Version, Generation: max(anchor.Generation, ledger.Generation), Operations: slices.Clone(anchor.Operations), ConnectionGenerations: map[string]uint64{}}
	for path, epoch := range anchor.ConnectionGenerations {
		out.ConnectionGenerations[path] = epoch
	}
	for path, epoch := range ledger.ConnectionGenerations {
		out.ConnectionGenerations[path] = max(out.ConnectionGenerations[path], epoch)
	}
	for _, old := range ledger.Operations {
		index := slices.IndexFunc(out.Operations, func(o Operation) bool { return o.ID == old.ID })
		if index < 0 {
			out.Operations = append(out.Operations, old)
			continue
		}
		current := out.Operations[index]
		a, b := current, old
		a.State = ""
		b.State = ""
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		if !bytes.Equal(x, y) {
			return out, ErrUnsafe
		}
		if current.State != old.State {
			if current.State == "closed" || old.State == "closed" {
				out.Operations[index].State = "closing"
			} else {
				out.Operations[index].State = "installing"
			}
		}
	}
	return out, nil
}

func emptyStore(connections []Connection) Store {
	s := Store{Version: Version, Operations: []Operation{}, ConnectionGenerations: map[string]uint64{}}
	for _, c := range connections {
		s.ConnectionGenerations[c.Path] = 0
	}
	return s
}
func bumpStore(s *Store, selected []Connection) {
	s.Generation++
	for _, connection := range selected {
		s.ConnectionGenerations[connection.Path]++
	}
}
