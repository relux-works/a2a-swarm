package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	PostRequestLimit     = 64 << 10
	PostRecordLimit      = 128 << 10
	PostRecordCountLimit = 128
	PostJournalLimit     = 16 << 20
)

var ErrPostOperation = errors.New("post operation journal refused; retained evidence unchanged")

// ErrPostOperationCapacity identifies a full journal without exposing local paths.
var ErrPostOperationCapacity = fmt.Errorf("%w: capacity reached; run a2a post-operation prune --acknowledged", ErrPostOperation)
var operationIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var operationDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func ValidPostOperationID(id string) bool { return operationIDPattern.MatchString(id) }
func PostOperationDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// PostBinding contains public operational metadata only, never credential
// values or secret-store references. DeviceID is not device-key evidence.
type PostBinding struct {
	Fingerprint string `json:"fingerprint"`
	UserID      string `json:"user_id"`
	DeviceID    string `json:"device_id"`
	Homeserver  string `json:"homeserver"`
}

// PostOperation is a dedicated journal, outside State's older serialization.
// Request is retained only until a complete ACK has been durably recorded.
type PostOperation struct {
	Schema         int             `json:"schema"`
	RecordDigest   string          `json:"record_digest,omitempty"`
	ID             string          `json:"operation_id"`
	Binding        PostBinding     `json:"binding"`
	RoomID         string          `json:"room_id"`
	RootID         string          `json:"root_id"`
	EventType      string          `json:"event_type"`
	TransactionID  string          `json:"transaction_id"`
	IntentDigest   string          `json:"intent_digest"`
	ContentDigest  string          `json:"content_digest"`
	RequestLength  int             `json:"request_length"`
	Request        json.RawMessage `json:"request,omitempty"`
	Outcome        string          `json:"outcome"`
	EventID        string          `json:"event_id,omitempty"`
	HTTPStatus     int             `json:"http_status,omitempty"`
	ResponseLength int64           `json:"response_length,omitempty"`
}

func operationPublic(s string, prefix byte) bool {
	if len(s) < 2 || len(s) > 255 || s[0] != prefix || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	if prefix == '@' {
		local, server, ok := strings.Cut(s[1:], ":")
		return ok && local != "" && server != ""
	}
	return true
}

func (r PostOperation) digest() string {
	r.RecordDigest = ""
	data, _ := json.Marshal(r)
	return PostOperationDigest(data)
}

func (r PostOperation) valid() bool {
	if r.RecordDigest != r.digest() {
		return false
	}
	if r.Schema != 1 || !ValidPostOperationID(r.ID) || !operationDigestPattern.MatchString(r.Binding.Fingerprint) ||
		!operationPublic(r.Binding.UserID, '@') || !operationPublic(r.RoomID, '!') || !operationPublic(r.RootID, '$') ||
		r.Binding.DeviceID == "" || !utf8.ValidString(r.Binding.DeviceID) || len(r.Binding.DeviceID) > 4096 || r.Binding.Homeserver == "" || len(r.Binding.Homeserver) > 4096 ||
		r.EventType != "m.room.message" || !operationIDPattern.MatchString(r.TransactionID) ||
		!operationDigestPattern.MatchString(r.IntentDigest) || !operationDigestPattern.MatchString(r.ContentDigest) ||
		r.RequestLength < 2 || r.RequestLength > PostRequestLimit {
		return false
	}
	switch r.Outcome {
	case "not_submitted", "unknown":
		if r.EventID != "" || r.HTTPStatus != 0 || r.ResponseLength != 0 || len(r.Request) != r.RequestLength || PostOperationDigest(r.Request) != r.ContentDigest {
			return false
		}
		// Refuse duplicate/unknown keys in the record (below), and duplicate keys
		// inside its request. A generic tree intentionally permits content fields.
		var request map[string]any
		if !utf8.Valid(r.Request) || DecodeConnectionJSON(r.Request, &request) != nil || request == nil {
			return false
		}
	case "acknowledged":
		if len(r.Request) != 0 || !operationPublic(r.EventID, '$') || r.HTTPStatus < 200 || r.HTTPStatus > 299 || r.ResponseLength < -1 {
			return false
		}
	default:
		return false
	}
	return true
}

// PostJournal pins its directory with os.Root; no lock is held over remote I/O.
// fault is package-test-only, for injected write/sync/publish failures.
type PostJournal struct {
	root  *os.Root
	fault func(string) error
	lock  func(*os.File) (func(), error)
}

func (j *PostJournal) Close() error { return j.root.Close() }

// OpenPostJournalContext retains the pinned-directory and evidence contract,
// while cancellation bounds every later read/write lock acquisition.
func OpenPostJournalContext(ctx context.Context, p Paths, name string, human, create bool) (*PostJournal, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	j, err := OpenPostJournal(p, name, human, create)
	if err != nil {
		return nil, err
	}
	j.lock = func(f *os.File) (func(), error) { return lockPostOperationContext(ctx, f) }
	return j, nil
}
func (j *PostJournal) step(stage string) error {
	if j.fault != nil {
		if err := j.fault(stage); err != nil {
			return ErrPostOperation
		}
	}
	return nil
}

// OpenPostJournal does not create anything for status. The identity's state
// root is already connection-scoped; the extra class/name prevents collisions.
func OpenPostJournal(p Paths, name string, human, create bool) (*PostJournal, error) {
	if !postOperationPlatform() || ValidateName(name) != nil {
		return nil, ErrPostOperation
	}
	class := "agents"
	if human {
		class = "humans"
	}
	path, err := filepath.Abs(filepath.Join(p.Base, "post-operations", class, name))
	if err != nil {
		return nil, ErrPostOperation
	}
	root, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, ErrPostOperation
	}
	parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	journal, err := filepath.Abs(filepath.Join(p.Base, "post-operations"))
	if err != nil {
		root.Close()
		return nil, ErrPostOperation
	}
	current := ""
	for _, part := range parts {
		current += string(filepath.Separator) + part
		// The state root is caller-owned; only the journal's own subtree
		// requires exact private permissions.
		strict := current == journal || strings.HasPrefix(current, journal+string(filepath.Separator))
		st, e := root.Lstat(part)
		if errors.Is(e, os.ErrNotExist) && create {
			if e = root.Mkdir(part, 0700); e != nil && !errors.Is(e, os.ErrExist) {
				root.Close()
				return nil, ErrPostOperation
			}
			if e = syncPostRoot(root); e != nil {
				root.Close()
				return nil, ErrPostOperation
			}
			st, e = root.Lstat(part)
		}
		if e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || !postOperationOwner(st, strict) {
			root.Close()
			return nil, ErrPostOperation
		}
		next, e := root.OpenRoot(part)
		if e != nil {
			root.Close()
			return nil, ErrPostOperation
		}
		opened, e := next.Stat(".")
		root.Close()
		if e != nil || !os.SameFile(st, opened) {
			next.Close()
			return nil, ErrPostOperation
		}
		root = next
	}
	if create {
		f, err := openPostOperationLock(root, true, openPostOperationFile)
		if err != nil {
			root.Close()
			return nil, ErrPostOperation
		}
		syncErr := f.Sync()
		closeErr := f.Close()
		if syncErr != nil || closeErr != nil || syncPostRoot(root) != nil {
			root.Close()
			return nil, ErrPostOperation
		}
	}
	return &PostJournal{root: root}, nil
}

func syncPostRoot(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Only journal initialization may create the lock. EEXIST selects one
// noncreating open; deletion in between refuses rather than recreating it.
// The opener parameter lets tests witness the exact flags without global hooks.
func openPostOperationLock(root *os.Root, create bool, open func(*os.Root, string, int, os.FileMode) (*os.File, error)) (*os.File, error) {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE | os.O_EXCL
	}
	f, err := open(root, ".lock", flags, 0600)
	if create && errors.Is(err, os.ErrExist) {
		f, err = open(root, ".lock", os.O_RDWR, 0)
	}
	if err != nil {
		return nil, ErrPostOperation
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || !postOperationOwner(st, true) || st.Size() != 0 {
		f.Close()
		return nil, ErrPostOperation
	}
	return f, nil
}

func (j *PostJournal) withLock(fn func() error) error {
	f, err := openPostOperationLock(j.root, false, openPostOperationFile)
	if err != nil {
		return ErrPostOperation
	}
	defer f.Close()
	lock := lockPostOperation
	if j.lock != nil {
		lock = j.lock
	}
	unlock, err := lock(f)
	if err != nil {
		return ErrPostOperation
	}
	defer unlock()
	if f.Sync() != nil {
		return ErrPostOperation
	}
	return fn()
}

func (j *PostJournal) locked(fn func() error) error {
	return j.withLock(func() error {
		if j.step("lock_sync") != nil || syncPostRoot(j.root) != nil {
			return ErrPostOperation
		}
		if err := j.cleanTemps(); err != nil {
			return err
		}
		return fn()
	})
}

func (j *PostJournal) read(id string) (PostOperation, bool, error) {
	var r PostOperation
	if !ValidPostOperationID(id) {
		return r, false, ErrPostOperation
	}
	name := id + ".json"
	f, err := openPostOperationFile(j.root, name, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return r, false, nil
	}
	if err != nil {
		return r, false, ErrPostOperation
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || !postOperationOwner(st, true) || st.Size() > PostRecordLimit {
		return r, false, ErrPostOperation
	}
	data, err := io.ReadAll(io.LimitReader(f, PostRecordLimit+1))
	if err != nil || len(data) > PostRecordLimit || !utf8.Valid(data) || DecodeConnectionJSON(data, &r) != nil || !r.valid() || r.ID != id {
		return PostOperation{}, false, ErrPostOperation
	}
	return r, true, nil
}

// Read creates no files and never writes record content. An existing lock
// excludes in-progress replacements. For an ACK, directory sync also seals a
// successor renamed by a prior writer that died before directory sync.
func (j *PostJournal) Read(id string) (PostOperation, bool, error) {
	var empty PostOperation
	if !ValidPostOperationID(id) {
		return empty, false, ErrPostOperation
	}
	var r PostOperation
	var exists bool
	err := j.withLock(func() error {
		if _, err := j.inventory(true); err != nil {
			return err
		}
		var err error
		r, exists, err = j.read(id)
		if err == nil && exists && r.Outcome == "acknowledged" && syncPostRoot(j.root) != nil {
			return ErrPostOperation
		}
		return err
	})
	if err != nil {
		return empty, false, err
	}
	return r, exists, nil
}

func samePostIntent(a, b PostOperation) bool {
	return a.ID == b.ID && a.Binding == b.Binding && a.RoomID == b.RoomID && a.RootID == b.RootID && a.IntentDigest == b.IntentDigest
}

// samePostRecord pins the complete verified phase, not just semantic intent.
// Callers retain the actual saved digest after each state transition. In
// particular, ACK compares against the reserved UNKNOWN, not the earlier
// prepared digest. Bytes are checked independently of the content digest.
func samePostRecord(a, b PostOperation) bool {
	return a.valid() && b.valid() && a.Schema == b.Schema && samePostIntent(a, b) &&
		a.ContentDigest == b.ContentDigest && a.RequestLength == b.RequestLength &&
		PostOperationDigest(a.Request) == PostOperationDigest(b.Request) && bytes.Equal(a.Request, b.Request) &&
		a.EventType == b.EventType && a.TransactionID == b.TransactionID && a.RecordDigest == b.RecordDigest &&
		a.Outcome == b.Outcome && a.EventID == b.EventID && a.HTTPStatus == b.HTTPStatus && a.ResponseLength == b.ResponseLength
}

type postJournalEntry struct {
	name   string
	info   os.FileInfo
	record PostOperation
}

// inventory runs under flock and validates the entire directory before any
// removal. Metadata exceptions are name- and type-specific; Lstat never follows
// them. Readers continue to ignore recognized temporary names, as before.
func (j *PostJournal) inventory(ignoreTemps bool) ([]postJournalEntry, error) {
	dir, err := j.root.Open(".")
	if err != nil {
		return nil, ErrPostOperation
	}
	defer dir.Close()
	var retained []postJournalEntry
	for {
		entries, err := dir.ReadDir(128)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, ErrPostOperation
		}
		for _, ent := range entries {
			name := ent.Name()
			if name == ".lock" {
				continue // the open lock descriptor is verified by withLock
			}
			metadata := name == ".DS_Store" || strings.HasPrefix(name, "._")
			temporary := !metadata && strings.HasSuffix(name, ".tmp") && ValidPostOperationID(strings.TrimSuffix(name, ".tmp"))
			if temporary && ignoreTemps {
				continue
			}
			st, e := j.root.Lstat(name)
			if e != nil || !st.Mode().IsRegular() {
				return nil, ErrPostOperation
			}
			if metadata {
				continue
			}
			if !postOperationOwner(st, true) || st.Size() < 0 || st.Size() > PostRecordLimit {
				return nil, ErrPostOperation
			}
			entry := postJournalEntry{name: name, info: st}
			if strings.HasSuffix(name, ".json") {
				r, ok, e := j.read(strings.TrimSuffix(name, ".json"))
				if e != nil || !ok {
					return nil, ErrPostOperation
				}
				entry.record = r
			} else if !temporary {
				return nil, ErrPostOperation
			}
			retained = append(retained, entry)
			if len(retained) > 2*PostRecordCountLimit {
				return nil, ErrPostOperation
			}
		}
		if errors.Is(err, io.EOF) {
			return retained, nil
		}
	}
}

// cleanTemps validates all entries first, then removes private stale temporary
// files under flock. An unsafe neighbor leaves even safe temporaries unchanged.
func (j *PostJournal) cleanTemps() error {
	entries, err := j.inventory(false)
	if err != nil {
		return err
	}
	removed := false
	for _, ent := range entries {
		if ent.record.ID != "" {
			continue
		}
		if j.root.Remove(ent.name) != nil {
			return ErrPostOperation
		}
		removed = true
	}
	if removed && syncPostRoot(j.root) != nil {
		return ErrPostOperation
	}
	return nil
}

// capacity counts retained records and replacement peak bytes after stale
// temporary cleanup. Unknowns are never evicted; unexpected entries refuse.
func (j *PostJournal) capacity(replacing string, next int) error {
	entries, err := j.inventory(false)
	if err != nil {
		return err
	}
	count, total := 0, int64(next)
	for _, ent := range entries {
		if ent.record.ID == "" {
			return ErrPostOperation // writers must clean temporaries first
		}
		count++
		if ent.name != replacing {
			total += ent.info.Size()
		}
	}
	if replacing == "" {
		count++
	}
	if count > PostRecordCountLimit || total > PostJournalLimit {
		return ErrPostOperationCapacity
	}
	return nil
}

// PruneAcknowledged explicitly retires ACK evidence only. It validates every
// entry before unlinking anything, uses the writer's flock, and syncs the
// directory after atomic per-record unlinks. Pending/UNKNOWN bytes and stale
// temporaries are unchanged. A crash can retire a subset; retry is safe.
func (j *PostJournal) PruneAcknowledged() (removed, retained int, err error) {
	err = j.withLock(func() error {
		entries, e := j.inventory(false)
		if e != nil {
			return e
		}
		for _, ent := range entries {
			if ent.record.ID == "" {
				continue
			}
			if ent.record.Outcome != "acknowledged" {
				retained++
				continue
			}
			if j.step("prune_remove") != nil || j.root.Remove(ent.name) != nil {
				return ErrPostOperation
			}
			removed++
		}
		if j.step("prune_directory_sync") != nil || syncPostRoot(j.root) != nil {
			return ErrPostOperation
		}
		return nil
	})
	return
}

func (j *PostJournal) save(r PostOperation, replace bool) error {
	r.RecordDigest = r.digest()
	if !r.valid() {
		return ErrPostOperation
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > PostRecordLimit {
		return ErrPostOperation
	}
	target := r.ID + ".json"
	old := ""
	if replace {
		old = target
	}
	budget := len(data)
	if replace {
		st, e := j.root.Lstat(target)
		if e != nil || !st.Mode().IsRegular() || !postOperationOwner(st, true) || st.Size() > PostRecordLimit {
			return ErrPostOperation
		}
		budget += int(st.Size()) // old record and new temporary coexist until rename
	}
	if err = j.capacity(old, budget); err != nil {
		return err
	}
	if err = j.step("create"); err != nil {
		return err
	}
	tmp := r.ID + ".tmp"
	f, err := openPostOperationFile(j.root, tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return ErrPostOperation
	}
	temporaryPresent := true
	defer func() {
		f.Close()
		if temporaryPresent {
			_ = j.root.Remove(tmp)
		}
	}()
	if j.step("write") != nil {
		return ErrPostOperation
	}
	n, err := f.Write(data)
	if err != nil || n != len(data) {
		return ErrPostOperation
	}
	if j.step("file_sync") != nil || f.Sync() != nil {
		return ErrPostOperation
	}
	if f.Close() != nil || j.step("publish") != nil {
		return ErrPostOperation
	}
	if replace {
		err = j.root.Rename(tmp, target)
		if err != nil {
			return ErrPostOperation
		}
		temporaryPresent = false // tmp no longer exists; never require its removal.
		if j.step("renamed") != nil {
			return ErrPostOperation
		}
	} else {
		// Exclusive initial publication cannot overwrite another retained ID.
		if j.root.Link(tmp, target) != nil {
			return ErrPostOperation
		}
		if j.root.Remove(tmp) != nil {
			return ErrPostOperation
		}
		temporaryPresent = false
	}
	if j.step("directory_sync") != nil || syncPostRoot(j.root) != nil {
		return ErrPostOperation
	}
	if j.step("directory_synced") != nil {
		return ErrPostOperation
	}
	return nil
}

// Prepare creates only a not_submitted record. Existing operations are returned
// unchanged even if not_submitted: creation ownership is never a replay grant.
func (j *PostJournal) Prepare(candidate PostOperation) (r PostOperation, created bool, err error) {
	err = j.locked(func() error {
		var exists bool
		var e error
		r, exists, e = j.read(candidate.ID)
		if e != nil {
			return e
		}
		if exists {
			if !samePostIntent(r, candidate) {
				return ErrPostOperation
			}
			return nil
		}
		if candidate.Outcome != "not_submitted" {
			return ErrPostOperation
		}
		candidate.RecordDigest = candidate.digest()
		if e = j.save(candidate, false); e != nil {
			return e
		}
		r, created = candidate, true
		return nil
	})
	return
}

// ReserveAttempt must complete before entering mutation I/O. Its caller must
// own the freshly created operation; existing IDs never call this method.
func (j *PostJournal) ReserveAttempt(expected PostOperation) (r PostOperation, err error) {
	err = j.locked(func() error {
		var ok bool
		var e error
		r, ok, e = j.read(expected.ID)
		if e != nil || !ok || !samePostRecord(r, expected) || r.Outcome != "not_submitted" {
			return ErrPostOperation
		}
		r.Outcome = "unknown"
		r.RecordDigest = r.digest()
		return j.save(r, true)
	})
	return
}

// RecordACK is monotonic. Request removal and ACK metadata share one atomic,
// synced replacement. On failure callers cannot claim a durable ACK.
func (j *PostJournal) RecordACK(expected PostOperation, event string, status int, length int64) (r PostOperation, err error) {
	err = j.locked(func() error {
		var ok bool
		var e error
		r, ok, e = j.read(expected.ID)
		if e != nil || !ok || !samePostRecord(r, expected) {
			return ErrPostOperation
		}
		if r.Outcome == "acknowledged" {
			if r.EventID != event {
				return ErrPostOperation
			}
			return nil
		}
		if r.Outcome != "unknown" {
			return ErrPostOperation
		}
		r.Outcome, r.EventID, r.HTTPStatus, r.ResponseLength = "acknowledged", event, status, length
		r.Request = nil
		r.RecordDigest = r.digest()
		return j.save(r, true)
	})
	return
}

// MatchesPostOperation compares semantic input without regenerating its frozen
// header, request, transaction or default deadline after wall-clock advance.
func MatchesPostOperation(r PostOperation, binding PostBinding, room, root, intent string) bool {
	return r.Binding == binding && r.RoomID == room && r.RootID == root && r.IntentDigest == intent
}
