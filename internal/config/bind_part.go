package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
)

const (
	bindPartMaxBytes    = 16 << 10
	bindPartFormat      = 1
	bindPartProtocol    = 1
	bindPartMarkerKey   = "bind_part"
	bindPartRollbackOK  = "normal"
	bindPartRollbackPre = "preparing"
	bindPartRollbackOut = "exported"
)

var (
	ErrBindPartMissing   = errors.New("marked bind state sidecar is missing")
	ErrBindPartInvalid   = errors.New("bind state sidecar is invalid")
	ErrBindPartUnmarked  = errors.New("bind state sidecar exists without its permanent State marker")
	ErrBindPartOwned     = errors.New("provider-owned State fields require a typed bind-state operation")
	ErrBindPartStale     = errors.New("stale bind-state revision or operation token")
	ErrBindPartOperation = errors.New("bind-state operation is held by another owner")
	ErrBindPartWrite     = errors.New("bind state sidecar write outcome is uncertain")
)

// bindPartDisk is the complete format-1 sidecar. The fixed structs deliberately
// avoid maps and omitempty so that every valid file has one explicit shape.
type bindPartDisk struct {
	Format            int                      `json:"format"`
	Identity          string                   `json:"identity"`
	Revision          uint64                   `json:"revision"`
	Generation        string                   `json:"generation"`
	SessionID         string                   `json:"session_id"`
	Stopped           bool                     `json:"stopped"`
	Lease             *bindPartLease           `json:"lease"`
	NextBatch         string                   `json:"next_batch"`
	OwnNotice         bindPartOwnNotice        `json:"own_notice"`
	NoticePublication string                   `json:"notice_publication"`
	CursorOp          *bindPartCursorOperation `json:"cursor_op"`
	StatusOp          *bindPartStatusOperation `json:"status_op"`
	ControlTxn        *bindPartControlTxn      `json:"control_txn"`
	LegacyShadow      bindPartProjection       `json:"legacy_shadow"`
	Rollback          string                   `json:"rollback"`
	Writer            bindPartWriter           `json:"writer"`
}

type bindPartLease struct {
	Holder  string    `json:"holder"`
	Host    string    `json:"host"`
	PID     int       `json:"pid"`
	Expires time.Time `json:"expires"`
}

type bindPartOwnNotice struct {
	PossiblyOrphaned *bool  `json:"possibly_orphaned"`
	Reason           string `json:"reason"`
	Since            string `json:"since"`
	ObservedAt       string `json:"observed_at"`
}

type bindPartProjection struct {
	Lease             *bindPartLease    `json:"lease"`
	NextBatch         string            `json:"next_batch"`
	Generation        string            `json:"generation"`
	SessionID         string            `json:"session_id"`
	Stopped           bool              `json:"stopped"`
	OwnNotice         bindPartOwnNotice `json:"own_notice"`
	NoticePublication string            `json:"notice_publication"`
}

type bindPartCursorOperation struct {
	Token              string `json:"token"`
	Kind               string `json:"kind"`
	Holder             string `json:"holder"`
	Host               string `json:"host"`
	PID                int    `json:"pid"`
	Birth              string `json:"birth"`
	ExpectedCursor     string `json:"expected_cursor"`
	ExpectedGeneration string `json:"expected_generation"`
}

type bindPartStatusOperation struct {
	Token            string            `json:"token"`
	Generation       string            `json:"generation"`
	SessionID        string            `json:"session_id"`
	ExpectedRevision uint64            `json:"expected_revision"`
	Notice           bindPartOwnNotice `json:"notice"`
	Holder           string            `json:"holder"`
	Host             string            `json:"host"`
	PID              int               `json:"pid"`
	Birth            string            `json:"birth"`
}

type bindPartControlTxn struct {
	Token             string            `json:"token"`
	Kind              string            `json:"kind"`
	ExpectedRevision  uint64            `json:"expected_revision"`
	OldGeneration     string            `json:"old_generation"`
	OldSessionID      string            `json:"old_session_id"`
	Generation        string            `json:"generation"`
	SessionID         string            `json:"session_id"`
	Stopped           bool              `json:"stopped"`
	Lease             *bindPartLease    `json:"lease"`
	OwnNotice         bindPartOwnNotice `json:"own_notice"`
	NoticePublication string            `json:"notice_publication"`
}

type bindPartWriter struct {
	Protocol int    `json:"protocol"`
	Seal     string `json:"seal"`
}

// BindPartView is a read-only snapshot of the provider-owned projection.
// Revision is a CAS token only; it is never identity or ownership evidence.
type BindPartView struct {
	Revision          uint64
	Lease             *Lease
	NextBatch         string
	Generation        string
	SessionID         string
	Stopped           bool
	OwnNotice         OwnNoticeObservation
	NoticePublication string
}

// BindIdleDeliveryState is the small provider view needed to start one idle
// bind sync. It deliberately omits unrelated State fields.
type BindIdleDeliveryState struct {
	Marked                 bool
	Active                 bool
	NextBatch              string
	Generation             string
	Stopped                bool
	HasMigration           bool
	HasLegacySubscriptions bool
	SealValid              bool
	stateStamp             FileStamp
}

// BindIdleDeliveryStateFromState builds the narrow bind-loop view from a
// validated full State snapshot.
func BindIdleDeliveryStateFromState(state State) BindIdleDeliveryState {
	view := BindIdleDeliveryState{
		Marked:                 bindPartMarker(state),
		Active:                 BindPartActive(state),
		NextBatch:              state.NextBatch,
		HasMigration:           state.Migration != nil,
		HasLegacySubscriptions: len(state.LegacySubscriptions) != 0,
	}
	if state.Terminal != nil {
		view.Generation = state.Terminal.Generation
		view.Stopped = state.Terminal.Stopped
	}
	return view
}

var bindPartTimeType = reflect.TypeFor[time.Time]()

func (p Paths) BindPartFile(name string) string {
	return filepath.Join(filepath.Dir(p.StateFile(name)), name+".bind.json")
}

func bindPartMarker(s State) bool {
	return s.opaque != nil && bytes.Equal(bytes.TrimSpace(s.opaque[bindPartMarkerKey]), []byte("true"))
}

// bindPartMarkerInInput reserves exactly one top-level spelling for the
// permanent marker. Other opaque State fields retain their existing rules.
func bindPartMarkerInInput(data []byte) (bool, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	tok, err := d.Token()
	if err != nil {
		return false, err
	}
	if tok != json.Delim('{') {
		return false, nil
	}
	seen := false
	marked := false
	for d.More() {
		tok, err = d.Token()
		if err != nil {
			return false, err
		}
		key, ok := tok.(string)
		if !ok {
			return false, ErrBindPartInvalid
		}
		if strings.EqualFold(key, bindPartMarkerKey) {
			if seen || key != bindPartMarkerKey {
				return false, ErrBindPartInvalid
			}
			seen = true
			var raw json.RawMessage
			if err := d.Decode(&raw); err != nil || !bytes.Equal(bytes.TrimSpace(raw), []byte("true")) {
				return false, ErrBindPartInvalid
			}
			marked = true
			continue
		}
		var discard json.RawMessage
		if err := d.Decode(&discard); err != nil {
			return false, err
		}
	}
	if _, err := d.Token(); err != nil {
		return false, err
	}
	if _, err := d.Token(); err != io.EOF {
		return false, ErrBindPartInvalid
	}
	return marked, nil
}

func readBindPart(p Paths, name string) (bindPartDisk, FileStamp, error) {
	if err := ValidateName(name); err != nil {
		return bindPartDisk{}, FileStamp{}, err
	}
	path := p.BindPartFile(name)
	if err := checkBindPartDirectory(filepath.Dir(path)); err != nil {
		return bindPartDisk{}, FileStamp{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return bindPartDisk{}, FileStamp{Known: true}, ErrBindPartMissing
		}
		return bindPartDisk{}, FileStamp{}, err
	}
	if err := checkBindPartFileInfo(info); err != nil {
		return bindPartDisk{}, FileStamp{}, err
	}
	if info.Size() > bindPartMaxBytes {
		return bindPartDisk{}, FileStamp{}, ErrBindPartInvalid
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return bindPartDisk{}, FileStamp{}, err
	}
	opened, statErr := f.Stat()
	if statErr != nil || !os.SameFile(info, opened) || checkBindPartFileInfo(opened) != nil {
		_ = f.Close()
		return bindPartDisk{}, FileStamp{}, ErrBindPartInvalid
	}
	data, readErr := io.ReadAll(io.LimitReader(f, bindPartMaxBytes+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil {
		return bindPartDisk{}, FileStamp{}, errors.Join(readErr, closeErr)
	}
	if len(data) > bindPartMaxBytes {
		return bindPartDisk{}, FileStamp{}, ErrBindPartInvalid
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, current) || checkBindPartFileInfo(current) != nil {
		return bindPartDisk{}, FileStamp{}, ErrBindPartInvalid
	}
	stamp := StampFile(path)
	if !stamp.Known || !stamp.Equal(stampInfo(opened)) {
		return bindPartDisk{}, FileStamp{}, ErrBindPartInvalid
	}
	part, err := decodeBindPart(name, data)
	if err != nil {
		return bindPartDisk{}, FileStamp{}, err
	}
	return part, stamp, nil
}

func decodeBindPart(name string, data []byte) (bindPartDisk, error) {
	var zero bindPartDisk
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	value, err := readUniqueJSONValue(dec)
	if err != nil {
		return zero, fmt.Errorf("%w: %v", ErrBindPartInvalid, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return zero, ErrBindPartInvalid
	}
	if err := validateJSONShape(value, reflect.TypeFor[bindPartDisk]()); err != nil {
		return zero, fmt.Errorf("%w: %v", ErrBindPartInvalid, err)
	}
	var part bindPartDisk
	strict := json.NewDecoder(bytes.NewReader(data))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&part); err != nil {
		return zero, fmt.Errorf("%w: %v", ErrBindPartInvalid, err)
	}
	if err := validateBindPart(name, part); err != nil {
		return zero, fmt.Errorf("%w: %v", ErrBindPartInvalid, err)
	}
	return part, nil
}

func readUniqueJSONValue(d *json.Decoder) (any, error) {
	tok, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch value := tok.(type) {
	case json.Delim:
		switch value {
		case '{':
			out := make(map[string]any)
			var keys []string
			for d.More() {
				keyToken, err := d.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, ErrBindPartInvalid
				}
				for _, prior := range keys {
					if strings.EqualFold(key, prior) {
						return nil, fmt.Errorf("duplicate or case alias key %q", key)
					}
				}
				keys = append(keys, key)
				child, err := readUniqueJSONValue(d)
				if err != nil {
					return nil, err
				}
				out[key] = child
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return nil, ErrBindPartInvalid
			}
			return out, nil
		case '[':
			var out []any
			for d.More() {
				child, err := readUniqueJSONValue(d)
				if err != nil {
					return nil, err
				}
				out = append(out, child)
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return nil, ErrBindPartInvalid
			}
			return out, nil
		default:
			return nil, ErrBindPartInvalid
		}
	case json.Number, string, bool, nil:
		return value, nil
	default:
		return nil, ErrBindPartInvalid
	}
}

func validateJSONShape(value any, typ reflect.Type) error {
	if typ.Kind() == reflect.Pointer {
		if value == nil {
			return nil
		}
		return validateJSONShape(value, typ.Elem())
	}
	if typ == bindPartTimeType {
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be a string", typ)
		}
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be an object", typ)
		}
		fields := make(map[string]reflect.StructField, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			fields[name] = field
		}
		if len(object) != len(fields) {
			return fmt.Errorf("%s has an incomplete field set", typ)
		}
		for name, child := range object {
			field, ok := fields[name]
			if !ok {
				return fmt.Errorf("unknown or noncanonical field %q", name)
			}
			if err := validateJSONShape(child, field.Type); err != nil {
				return fmt.Errorf("%s.%s: %w", typ, name, err)
			}
		}
	case reflect.String:
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be a string", typ)
		}
	case reflect.Bool:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be a boolean", typ)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if _, ok := value.(json.Number); !ok {
			return fmt.Errorf("%s must be a number", typ)
		}
	default:
		return fmt.Errorf("unsupported fixed JSON type %s", typ)
	}
	return nil
}

func validateBindPart(name string, part bindPartDisk) error {
	if part.Format != bindPartFormat || part.Identity != name || part.Revision == 0 || part.Writer.Protocol != bindPartProtocol || !lowerHex(part.Writer.Seal, 64) {
		return ErrBindPartInvalid
	}
	if part.Rollback != bindPartRollbackOK && part.Rollback != bindPartRollbackPre && part.Rollback != bindPartRollbackOut {
		return ErrBindPartInvalid
	}
	if err := validateBindProjection(part.projection()); err != nil {
		return err
	}
	if err := validateBindProjection(part.LegacyShadow); err != nil {
		return err
	}
	if part.CursorOp != nil {
		op := part.CursorOp
		if !lowerHex(op.Token, 32) || (op.Kind != "bind" && op.Kind != "delivery" && op.Kind != "monitor" && op.Kind != "projector" && op.Kind != "runtime") || !hostRef(op.Holder, 200) || !hostRef(op.Host, 128) || op.PID <= 1 || !hostRef(op.Birth, 128) || op.ExpectedGeneration != "" && !hostRef(op.ExpectedGeneration, 128) {
			return ErrBindPartInvalid
		}
	}
	if part.StatusOp != nil {
		op := part.StatusOp
		if !lowerHex(op.Token, 32) || !hostRef(op.Generation, 128) || !hostRef(op.SessionID, 200) || op.ExpectedRevision != part.Revision || !hostRef(op.Holder, 200) || !hostRef(op.Host, 128) || op.PID <= 1 || !hostRef(op.Birth, 128) || validateBindOwnNotice(op.Notice) != nil || !reflect.DeepEqual(op.Notice, part.OwnNotice) {
			return ErrBindPartInvalid
		}
	}
	if part.ControlTxn != nil {
		txn := part.ControlTxn
		if !lowerHex(txn.Token, 32) || (txn.Kind != "create" && txn.Kind != "stop" && txn.Kind != "handover" && txn.Kind != "recover") || txn.ExpectedRevision == 0 || txn.ExpectedRevision > part.Revision {
			return ErrBindPartInvalid
		}
		if (txn.OldGeneration == "") != (txn.OldSessionID == "") || txn.OldGeneration != "" && (!hostRef(txn.OldGeneration, 128) || !hostRef(txn.OldSessionID, 200)) || (txn.Generation == "") != (txn.SessionID == "") || txn.Generation != "" && (!hostRef(txn.Generation, 128) || !hostRef(txn.SessionID, 200) || txn.Generation != txn.OldGeneration && !lowerHex(txn.Generation, 32)) {
			return ErrBindPartInvalid
		}
		if err := validateBindProjection(bindPartProjection{Lease: txn.Lease, NextBatch: part.NextBatch, Generation: txn.Generation, SessionID: txn.SessionID, Stopped: txn.Stopped, OwnNotice: txn.OwnNotice, NoticePublication: txn.NoticePublication}); err != nil {
			return err
		}
	}
	pending := 0
	if part.CursorOp != nil {
		pending++
	}
	if part.StatusOp != nil {
		pending++
	}
	if part.ControlTxn != nil {
		pending++
	}
	if pending > 1 {
		return ErrBindPartInvalid
	}
	if part.Writer.Seal != bindPartSeal(part) {
		return ErrBindPartInvalid
	}
	return nil
}

func validateBindProjection(projection bindPartProjection) error {
	if projection.Lease != nil {
		l := projection.Lease
		if !hostRef(l.Holder, 200) || l.PID < 0 || l.Expires.IsZero() || l.Host != "" && !hostRef(l.Host, 128) {
			return ErrBindPartInvalid
		}
	}
	if (projection.Generation == "") != (projection.SessionID == "") || projection.Generation != "" && !hostRef(projection.Generation, 128) {
		return ErrBindPartInvalid
	}
	if projection.SessionID != "" && !hostRef(projection.SessionID, 200) {
		return ErrBindPartInvalid
	}
	if !validBindNoticePublication(projection.NoticePublication) {
		return ErrBindPartInvalid
	}
	if err := validateBindOwnNotice(projection.OwnNotice); err != nil {
		return ErrBindPartInvalid
	}
	return nil
}

func validBindNoticePublication(value string) bool {
	switch value {
	case "", "published", "unavailable: own notice changed", "unavailable: no liveness record (a2a status set)", "unavailable: invalid notice retry policy", "unavailable: own liveness publication retry pending", "unavailable: own liveness publication failed":
		return true
	default:
		return false
	}
}

func validateBindOwnNotice(n bindPartOwnNotice) error {
	var observed bindNoticeTimestamp
	observedOK := false
	if n.ObservedAt != "" {
		observed, observedOK = parseBindNoticeTimestamp(n.ObservedAt)
	}
	if n.ObservedAt != "" && !observedOK {
		return ErrBindPartInvalid
	}
	suspected := n.PossiblyOrphaned != nil && *n.PossiblyOrphaned
	if suspected || n.Reason != "" || n.Since != "" {
		since, sinceOK := parseBindNoticeTimestamp(n.Since)
		if !sinceOK || n.Reason != "owned-notice-orphan-suspected" || !observedOK || observed.before(since) || n.PossiblyOrphaned != nil && !*n.PossiblyOrphaned {
			return ErrBindPartInvalid
		}
	}
	if n.PossiblyOrphaned != nil && !observedOK {
		return ErrBindPartInvalid
	}
	return nil
}

var bindNoticeTimestampGrammar = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt][0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?([Zz]|[+-]00:00)$`)

type bindNoticeTimestamp struct {
	second   time.Time
	fraction string
}

func parseBindNoticeTimestamp(value string) (bindNoticeTimestamp, bool) {
	if !bindNoticeTimestampGrammar.MatchString(value) {
		return bindNoticeTimestamp{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.ToUpper(value))
	if err != nil {
		return bindNoticeTimestamp{}, false
	}
	fraction := ""
	if value[19] == '.' {
		end := strings.IndexAny(value[20:], "Zz+-") + 20
		fraction = strings.TrimRight(value[20:end], "0")
	}
	return bindNoticeTimestamp{second: parsed.Truncate(time.Second), fraction: fraction}, true
}

func (t bindNoticeTimestamp) before(other bindNoticeTimestamp) bool {
	if !t.second.Equal(other.second) {
		return t.second.Before(other.second)
	}
	for i := 0; i < len(t.fraction) || i < len(other.fraction); i++ {
		left, right := byte('0'), byte('0')
		if i < len(t.fraction) {
			left = t.fraction[i]
		}
		if i < len(other.fraction) {
			right = other.fraction[i]
		}
		if left != right {
			return left < right
		}
	}
	return false
}

func (part bindPartDisk) projection() bindPartProjection {
	publication := part.NoticePublication
	if part.StatusOp != nil {
		publication = ""
	}
	return bindPartProjection{
		Lease: cloneBindPartLease(part.Lease), NextBatch: part.NextBatch,
		Generation: part.Generation, SessionID: part.SessionID, Stopped: part.Stopped,
		OwnNotice: cloneBindPartNotice(part.OwnNotice), NoticePublication: publication,
	}
}

func bindPartSeal(part bindPartDisk) string {
	part.Writer.Seal = ""
	data, err := json.Marshal(part)
	if err != nil {
		return ""
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return ""
	}
	var writer map[string]json.RawMessage
	if json.Unmarshal(fields["writer"], &writer) != nil {
		return ""
	}
	delete(writer, "seal")
	fields["writer"], err = json.Marshal(writer)
	if err != nil {
		return ""
	}
	unsigned, err := json.Marshal(fields)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(unsigned)
	return hex.EncodeToString(digest[:])
}

func sealBindPart(part bindPartDisk) (bindPartDisk, error) {
	part.Writer = bindPartWriter{Protocol: bindPartProtocol}
	part.Writer.Seal = bindPartSeal(part)
	if part.Writer.Seal == "" {
		return bindPartDisk{}, ErrBindPartInvalid
	}
	return part, nil
}

func encodeBindPart(part bindPartDisk) (bindPartDisk, []byte, error) {
	sealed, err := sealBindPart(part)
	if err != nil {
		return bindPartDisk{}, nil, err
	}
	data, err := json.MarshalIndent(sealed, "", "  ")
	return sealed, data, err
}

func checkBindPartDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || !bindPartCurrentUserOwns(info) {
		return fmt.Errorf("%w: unsafe state directory", ErrBindPartInvalid)
	}
	return nil
}

func checkBindPartFileInfo(info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode().Perm() != SecretFileMode || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky|os.ModeSymlink) != 0 || !bindPartCurrentUserOwns(info) {
		return fmt.Errorf("%w: unsafe sidecar file", ErrBindPartInvalid)
	}
	return nil
}

var bindPartSyncDirectory = syncStateDirectory
var bindPartRename = os.Rename

func writeBindPart(p Paths, name string, part bindPartDisk) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	path := p.BindPartFile(name)
	if err := checkBindPartDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if err := checkSecretFile(path); err != nil {
			return err
		}
		if err := checkBindPartFileInfo(info); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	part.Identity = name
	part.Format = bindPartFormat
	part.Revision++
	sealed, data, err := encodeBindPart(part)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+name+".bind.*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpPath) }
	if err := tmp.Chmod(SecretFileMode); err != nil {
		cleanup()
		return err
	}
	n, err := tmp.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = tmp.Sync()
	}
	if err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := bindPartRename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	dirErr := bindPartSyncDirectory(filepath.Dir(path))
	stored, _, readErr := readBindPart(p, name)
	if readErr != nil || !reflect.DeepEqual(stored, sealed) {
		return fmt.Errorf("%w: readback mismatch: %v", ErrBindPartWrite, errors.Join(dirErr, readErr))
	}
	if dirErr != nil {
		// Exact readback is the only basis for continuing after a post-rename
		// directory-sync uncertainty.
		return nil
	}
	return nil
}

func cloneBindPartLease(l *bindPartLease) *bindPartLease {
	if l == nil {
		return nil
	}
	c := *l
	return &c
}

func cloneBindPartNotice(n bindPartOwnNotice) bindPartOwnNotice {
	if n.PossiblyOrphaned != nil {
		v := *n.PossiblyOrphaned
		n.PossiblyOrphaned = &v
	}
	return n
}

func bindProjectionFromState(s State) bindPartProjection {
	projection := bindPartProjection{NextBatch: s.NextBatch}
	if s.Lease != nil {
		projection.Lease = &bindPartLease{Holder: s.Lease.Holder, Host: s.Lease.Host, PID: s.Lease.PID, Expires: s.Lease.Expires}
	}
	if s.Terminal != nil {
		projection.Generation = s.Terminal.Generation
		projection.SessionID = s.Terminal.SessionID
		projection.Stopped = s.Terminal.Stopped
		projection.OwnNotice = bindPartOwnNotice{
			PossiblyOrphaned: s.Terminal.OwnNotice.PossiblyOrphaned,
			Reason:           s.Terminal.OwnNotice.Reason, Since: s.Terminal.OwnNotice.Since,
			ObservedAt: s.Terminal.OwnNotice.ObservedAt,
		}
		projection.NoticePublication = s.Terminal.NoticePublication
	}
	return cloneBindPartProjection(projection)
}

func cloneBindPartProjection(p bindPartProjection) bindPartProjection {
	p.Lease = cloneBindPartLease(p.Lease)
	p.OwnNotice = cloneBindPartNotice(p.OwnNotice)
	return p
}

func applyBindProjection(s *State, projection bindPartProjection) {
	if projection.Lease == nil {
		s.Lease = nil
	} else {
		s.Lease = &Lease{Holder: projection.Lease.Holder, Host: projection.Lease.Host, PID: projection.Lease.PID, Expires: projection.Lease.Expires}
	}
	s.NextBatch = projection.NextBatch
	if s.Terminal == nil && (projection.Generation != "" || projection.SessionID != "" || projection.Stopped || projection.NoticePublication != "" || projection.OwnNotice.PossiblyOrphaned != nil || projection.OwnNotice.Reason != "" || projection.OwnNotice.Since != "" || projection.OwnNotice.ObservedAt != "") {
		s.Terminal = &TerminalBinding{}
	}
	if s.Terminal != nil {
		s.Terminal.Generation = projection.Generation
		s.Terminal.SessionID = projection.SessionID
		s.Terminal.Stopped = projection.Stopped
		s.Terminal.OwnNotice = OwnNoticeObservation{
			PossiblyOrphaned: projection.OwnNotice.PossiblyOrphaned,
			Reason:           projection.OwnNotice.Reason, Since: projection.OwnNotice.Since,
			ObservedAt: projection.OwnNotice.ObservedAt,
		}
		s.Terminal.OwnNotice = cloneOwnNotice(s.Terminal.OwnNotice)
		s.Terminal.NoticePublication = projection.NoticePublication
	}
}
