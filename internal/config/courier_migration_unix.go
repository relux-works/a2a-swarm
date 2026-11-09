//go:build unix

package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type courierFile struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}
type courierJournal struct {
	Version          int           `json:"version"`
	Identity         string        `json:"identity"`
	Source           string        `json:"source"`
	Metadata         string        `json:"metadata"`
	At               time.Time     `json:"at"`
	Keychain         bool          `json:"keychain"`
	Files            []courierFile `json:"files"`
	CredentialDigest string        `json:"credential_digest"`
	CredentialReady  bool          `json:"credential_ready"`
	RollbackReady    bool          `json:"rollback_ready"`
}

const courierJournalName = "migration.json"
const courierMaxFile = 64 << 20
const courierMaxTotal = 256 << 20

func courierDigest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// Reject symlinks in every component, including ancestors. The selected roots
// and all per-identity directories must be private and owned by this UID. No
// permission repair, chown or inferred alternate root is attempted.
func courierPath(path string, directory bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrCourierMigration
	}
	cur := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(path, cur), string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return ErrCourierMigration
		}
		if cur != path && !st.IsDir() {
			return ErrCourierMigration
		}
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return courierOwned(st, directory)
}

func courierOwned(st fs.FileInfo, directory bool) error {
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return ErrCourierMigration
	}
	if directory {
		if !st.IsDir() || st.Mode().Perm() != 0700 {
			return ErrCourierMigration
		}
	} else {
		if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || owner.Nlink != 1 {
			return ErrCourierMigration
		}
	}
	return nil
}

func courierRead(path string, limit int64) ([]byte, error) {
	if err := courierPath(path, false); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil || before.Size() > limit {
		return nil, ErrCourierMigration
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || courierOwned(after, false) != nil {
		return nil, ErrCourierMigration
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, ErrCourierMigration
	}
	return b, nil
}

func courierMkdir(path string) error {
	if err := courierPath(path, true); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := courierMkdir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	if err := syncStateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return courierPath(path, true)
}

func courierWrite(path string, data []byte) error {
	if err := courierMkdir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := courierPath(path, false); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := writeSecretAtomic(path, "courier", data); err != nil {
		return err
	}
	return syncStateDirectory(filepath.Dir(path))
}

func courierInventory(p Paths, name string) ([]courierFile, error) {
	var files []courierFile
	var total int64
	for _, dir := range []string{"agents", "state", "owner-dm"} {
		root := p.Base
		if dir == "agents" {
			root = p.metadataRoot()
		}
		base := filepath.Join(root, dir)
		if err := courierPath(base, true); errors.Is(err, fs.ErrNotExist) && dir != "agents" {
			continue
		} else if err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(base)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			n := entry.Name()
			if !strings.HasPrefix(n, name+".") && n != name {
				continue
			}
			// Refuse historical secret copies rather than carrying them through a
			// cutover. Their disposition belongs to explicit operator recovery.
			if strings.Contains(n, ".migrated") {
				return nil, ErrCourierMigration
			}
			// Stable source lock inodes are never copied, renamed or deleted.
			if strings.HasSuffix(n, ".lock") {
				continue
			}
			err := filepath.WalkDir(filepath.Join(base, n), func(path string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if d.IsDir() {
					return courierPath(path, true)
				}
				if strings.HasSuffix(d.Name(), ".lock") {
					return courierPath(path, false)
				}
				b, e := courierRead(path, courierMaxFile)
				if e != nil {
					return e
				}
				total += int64(len(b))
				if total > courierMaxTotal || len(files) >= 4096 {
					return ErrCourierMigration
				}
				rel, e := filepath.Rel(base, path)
				if e != nil {
					return e
				}
				files = append(files, courierFile{Path: filepath.Join(dir, rel), Digest: courierDigest(b), Size: int64(len(b))})
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	journalRoot := filepath.Join(p.Base, "post-operations", "agents", name)
	if _, err := os.Lstat(journalRoot); !errors.Is(err, fs.ErrNotExist) {
		if err != nil {
			return nil, err
		}
		journal, err := OpenPostJournal(p, name, false, false)
		if err != nil {
			return nil, err
		}
		defer journal.Close()
		entries, err := os.ReadDir(journalRoot)
		if err != nil {
			return nil, err
		}
		if len(entries) > PostRecordCountLimit+1 {
			return nil, ErrCourierPostPending
		}
		for _, entry := range entries {
			if entry.Name() == ".lock" {
				continue
			}
			if !strings.HasSuffix(entry.Name(), ".json") {
				return nil, ErrCourierPostPending
			}
			id := strings.TrimSuffix(entry.Name(), ".json")
			r, ok, err := journal.read(id) // caller holds the journal flock
			if err != nil || !ok || r.Outcome == "not_submitted" {
				return nil, ErrCourierPostPending
			}
			path := filepath.Join(journalRoot, entry.Name())
			b, err := courierRead(path, PostRecordLimit)
			if err != nil {
				return nil, err
			}
			total += int64(len(b))
			if total > courierMaxTotal || len(files) >= 4096 {
				return nil, ErrCourierMigration
			}
			files = append(files, courierFile{Path: filepath.Join("post-operations", "agents", name, entry.Name()), Digest: courierDigest(b), Size: int64(len(b))})
		}
	}
	return files, nil
}

func courierSource(p Paths, rel string) string {
	root := p.Base
	if strings.HasPrefix(rel, "agents/") {
		root = p.metadataRoot()
	}
	return filepath.Join(root, rel)
}
func courierBackup(target string) string {
	return filepath.Join(target, "rollback", "source-credential.json")
}

func courierRollbackCredential(target string) string {
	return filepath.Join(target, "rollback", "credential.json")
}

// A closed nonsecret schema, derived from the sealed transaction, also makes
// a completed atomic replacement distinguishable after a crash before done.
func courierTombstone(name string, at time.Time) []byte {
	b, _ := json.Marshal(struct {
		Migrated bool      `json:"migrated_to_courier"`
		At       time.Time `json:"at"`
		Identity string    `json:"identity"`
	}{true, at.UTC(), name})
	return append(b, '\n')
}

// Resume may fill a missing private copy but never overwrite a damaged one.
func courierPrivateCopy(path string, data []byte) error {
	b, e := courierRead(path, courierMaxFile)
	if errors.Is(e, fs.ErrNotExist) {
		if e = courierWrite(path, data); e != nil {
			return e
		}
		b, e = courierRead(path, courierMaxFile)
	}
	if e != nil || !bytes.Equal(b, data) {
		return ErrCourierMigration
	}
	return nil
}
func courierStep(o CourierMigrationOptions, label string) error {
	if o.step != nil {
		return o.step(label)
	}
	return nil
}

// MigrateCourier takes consumer -> credential -> post attempt -> journal ->
// state locks, retaining every
// inode forever. It uses L02's locked writer/seal, never an alternate State
// encoding. Explicit resume/rollback is required after any interruption.
func MigrateCourier(ctx context.Context, p Paths, name string, o CourierMigrationOptions) (err error) {
	// All errors are closed: provider errors and paths may contain private data.
	defer func() {
		if err != nil && !errors.Is(err, ErrCourierSourceSecrets) && !errors.Is(err, ErrCourierPostPending) {
			err = ErrCourierMigration
		}
	}()
	if !courierPublishSupported || ValidateName(name) != nil || o.Now.IsZero() || !filepath.IsAbs(o.To) || filepath.Clean(o.To) != o.To || o.Resume && o.Rollback || o.Keychain && o.Secrets == nil {
		return ErrCourierMigration
	}
	if err = courierPath(p.Base, true); err != nil {
		return err
	}
	if err = courierPath(p.metadataRoot(), true); err != nil {
		return err
	}
	if p.metadataRoot() != p.Base {
		if _, e := os.Lstat(filepath.Join(p.Base, "agents", name+".json")); !errors.Is(e, fs.ErrNotExist) {
			return ErrCourierMigration
		}
	}
	if err = courierCheckOverlap(o.To, []string{p.Base, p.metadataRoot()}, os.Stat); err != nil {
		return err
	}
	if _, e := courierRead(p.StateFile(name), 1<<20); e != nil && !errors.Is(e, fs.ErrNotExist) {
		return e
	}
	// Refuse occupied identities before provisioning even lock files; recheck
	// under both lifetime/state locks to close the admission race.
	st, err := LoadState(p, name)
	if err != nil {
		return err
	}
	if courierOccupied(st) {
		return ErrCourierMigration
	}
	if err = courierMkdir(filepath.Join(p.Base, "state")); err != nil {
		return err
	}
	if err = courierPath(filepath.Join(p.metadataRoot(), "agents"), true); err != nil {
		return err
	}
	for _, path := range []string{filepath.Join(p.Base, "state", name+".runtime-consumer.lock"), filepath.Join(p.Base, "state", name+".lock"), filepath.Join(p.metadataRoot(), "agents", "."+name+".lock")} {
		if e := courierPath(path, false); e != nil && !errors.Is(e, fs.ErrNotExist) {
			return e
		}
	}
	_, release, err := acquireMigrationConsumerLock(p, name, time.Now(), 0)
	if err != nil {
		return err
	}
	defer release()
	releaseCreds, err := lockFileBy(filepath.Join(p.metadataRoot(), "agents", "."+name+".lock"), time.Now())
	if err != nil {
		return err
	}
	defer releaseCreds()
	// Lock order: consumer -> credential -> post attempt -> journal -> State.
	// A live reserved attempt refuses immediately; no lock is renamed or deleted.
	attempt := postAttemptPath(p, name, false)
	if e := courierPath(attempt, false); e != nil && !errors.Is(e, fs.ErrNotExist) {
		return e
	}
	releaseAttempt, e := lockFileBy(attempt, time.Now())
	if e != nil {
		return ErrCourierPostPending
	}
	defer releaseAttempt()
	journal, e := OpenPostJournal(p, name, false, false)
	if e == nil {
		defer journal.Close()
		f, e := openPostOperationLock(journal.root, false, openPostOperationFile)
		if e != nil {
			return ErrCourierPostPending
		}
		defer f.Close()
		// Nonblocking admission avoids waiting behind an unfinished journal writer.
		if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
			return ErrCourierPostPending
		}
		defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	} else if _, statErr := os.Lstat(filepath.Join(p.Base, "post-operations", "agents", name)); !errors.Is(statErr, fs.ErrNotExist) {
		return ErrCourierPostPending
	}
	_, err = withStateLock(p, name, func(path string) (func(), error) { return lockFileBy(path, time.Now()) }, func(st State) (State, error) {
		if courierOccupied(st) {
			return st, ErrCourierMigration
		}
		return migrateCourierLocked(ctx, p, name, st, o)
	})
	return err
}

func courierOccupied(s State) bool {
	return s.Lease != nil || s.HostEnrollment != nil || s.RefuseMonitorConsumer() != nil || s.Terminal != nil && !s.Terminal.Stopped || s.Runtime != nil && !s.Runtime.Retired
}

func migrateCourierLocked(ctx context.Context, p Paths, name string, st State, o CourierMigrationOptions) (State, error) {
	target := filepath.Join(o.To, "identities", name)
	fresh := st.Migration == nil
	if fresh && (o.Resume || o.Rollback) {
		return st, ErrCourierMigration
	}
	if !fresh && (!o.Resume && !o.Rollback || st.Migration.CourierBase != o.To || st.Migration.At.IsZero() || st.Migration.Phase != "in_progress" && st.Migration.Phase != "done") {
		return st, ErrCourierMigration
	}
	var j courierJournal
	if fresh {
		if _, e := os.Lstat(target); !errors.Is(e, fs.ErrNotExist) {
			return st, ErrCourierMigration
		}
		// Verify all inputs before the first fence write. No credentials/provider
		// values are emitted, and no source namespace is inferred from a name.
		files, e := courierInventory(p, name)
		if e != nil {
			return st, e
		}
		original, e := courierRead(p.AgentFile(name), 1<<20)
		if e != nil {
			return st, e
		}
		if e = courierSourceCredentialCheck(original, o.Keychain); e != nil {
			return st, e
		}
		if _, e = courierCredentials(original, name, o.Binding); e != nil {
			return st, e
		}
		hasCredential := false
		for _, f := range files {
			if f.Path == filepath.Join("agents", name+".json") {
				hasCredential = true
			}
		}
		if !hasCredential || o.Keychain && o.Secrets == nil {
			return st, ErrCourierMigration
		}
		// Validate target ancestors before changing the source. Parent directories
		// may be created after the fence, but an unsafe existing parent is refused.
		ancestor := filepath.Dir(target)
		for {
			if _, e = os.Lstat(ancestor); e == nil {
				if e = courierPath(ancestor, true); e != nil {
					return st, e
				}
				break
			}
			if !errors.Is(e, fs.ErrNotExist) {
				return st, e
			}
			ancestor = filepath.Dir(ancestor)
		}
		st.Migration = &IdentityMigration{Phase: "in_progress", CourierBase: o.To, At: o.Now.UTC()}
		st, e = saveStateLocked(p, name, st, st, ordinaryWriterFence(st, o.Now), false, true)
		if e != nil {
			return st, e
		}
		if e = courierStep(o, "fenced"); e != nil {
			return st, e
		}
	}
	// A fence may have persisted just before creation of the target journal. In
	// that case source inputs are intact and an explicit resume can initialize it.
	at := st.Migration.At
	journalPath := filepath.Join(target, courierJournalName)
	raw, e := courierRead(journalPath, 1<<20)
	if errors.Is(e, fs.ErrNotExist) {
		if _, e = os.Lstat(target); !errors.Is(e, fs.ErrNotExist) {
			return st, ErrCourierMigration
		}
		if o.Rollback {
			// A missing target is not proof that cutover never happened. Require
			// intact source credentials, no transaction backup, an in-progress sealed
			// State and a validated source inventory before aborting a pre-cutover run.
			if st.Migration.Phase != "in_progress" || st.Migration.CutoverStarted || !validWriterSeal(st) || ctx.Err() != nil {
				return st, ErrCourierMigration
			}
			original, e := courierRead(p.AgentFile(name), 1<<20)
			if e != nil {
				return st, e
			}
			creds, e := courierCredentials(original, name, o.Binding)
			if e != nil {
				return st, e
			}
			// Scrubbed metadata survives a Keychain cutover. It cannot alone
			// prove that this is a pre-cutover abort: require the original
			// backend still present before an explicit fence clear. A missing
			// item requires its private rollback copy, never metadata alone.
			if o.Keychain {
				creds, e = o.Secrets.Read(ctx, creds)
				if e != nil || creds.AccessToken == "" {
					return st, ErrCourierMigration
				}
			}
			if _, e = courierInventory(p, name); e != nil {
				return st, e
			}
			return courierClearFence(p, name, st, o.Now)
		}
		if e = courierMkdir(filepath.Dir(target)); e != nil {
			return st, e
		}
		original, e := courierRead(p.AgentFile(name), 1<<20)
		if e != nil {
			return st, e
		}
		if e = courierSourceCredentialCheck(original, o.Keychain); e != nil {
			return st, e
		}
		files, e := courierInventory(p, name)
		if e != nil {
			return st, e
		}
		j = courierJournal{Version: 1, Identity: name, Source: p.Base, Metadata: p.metadataRoot(), At: at, Keychain: o.Keychain, Files: files}
		// Publish a fully initialized directory. A crash before the rename leaves
		// only an unpublished staging directory, never an unowned final target.
		staging, e := os.MkdirTemp(filepath.Dir(target), "."+name+".migration-")
		if e != nil {
			return st, e
		}
		if e = courierSaveJournal(filepath.Join(staging, courierJournalName), j); e != nil {
			return st, e
		}
		if _, e = os.Lstat(target); !errors.Is(e, fs.ErrNotExist) {
			return st, ErrCourierMigration
		}
		if e = courierStep(o, "staged"); e != nil {
			return st, e
		}
		if e = courierPublishDirectory(staging, target); e != nil {
			return st, e
		}
		if e = syncStateDirectory(filepath.Dir(target)); e != nil {
			return st, e
		}
	} else if e != nil || DecodeConnectionJSON(raw, &j) != nil {
		return st, ErrCourierMigration
	}
	if e = courierStep(o, "journaled"); e != nil {
		return st, e
	}
	if j.Version != 1 || j.Identity != name || j.Source != p.Base || j.Metadata != p.metadataRoot() || !j.At.Equal(at) || j.Keychain != o.Keychain || len(j.Files) > 4096 || j.Keychain && o.Secrets == nil {
		return st, ErrCourierMigration
	}
	if e = courierValidateJournal(j, name); e != nil {
		return st, e
	}
	if o.Rollback {
		return courierRollback(ctx, p, name, st, o, target, j)
	}
	if st.Migration.Phase == "done" {
		if e = courierVerify(target, j); e != nil {
			return st, e
		}
		if e = courierVerifyRollback(target, j, name); e != nil {
			return st, e
		}
		if j.Keychain {
			b, e := courierRead(p.AgentFile(name), 1<<20)
			if e != nil {
				return st, e
			}
			if e = courierSourceCredentialCheck(b, true); e != nil {
				return st, e
			}
		}
		if !j.Keychain {
			b, e := courierRead(p.AgentFile(name), 1<<20)
			if e != nil || !bytes.Equal(b, courierTombstone(name, at)) {
				return st, ErrCourierMigration
			}
		}
		return st, nil
	}
	// Inventory names are fixed by the journal. All partial files are replaced
	// only in this proven source-bound transaction; finished files must match.
	for _, f := range j.Files {
		if e = ctx.Err(); e != nil {
			return st, e
		}
		dest := filepath.Join(target, f.Path)
		b, readErr := courierRead(dest, courierMaxFile)
		if readErr == nil {
			if courierDigest(b) != f.Digest || int64(len(b)) != f.Size {
				return st, ErrCourierMigration
			}
			continue
		}
		if !errors.Is(readErr, fs.ErrNotExist) {
			return st, readErr
		}
		source := courierSource(p, f.Path)
		b, e = courierRead(source, courierMaxFile)
		if e != nil || courierDigest(b) != f.Digest || int64(len(b)) != f.Size {
			return st, ErrCourierMigration
		}
		if e = courierWrite(dest, b); e != nil {
			return st, e
		}
		if e = courierStep(o, "copied"); e != nil {
			return st, e
		}
	}
	for _, f := range j.Files {
		if strings.HasPrefix(f.Path, "post-operations/") {
			if e = courierPrivateCopy(filepath.Join(target, "post-operations", "agents", name, ".lock"), nil); e != nil {
				return st, e
			}
			break
		}
	}
	if e = courierVerify(target, j); e != nil {
		return st, e
	}
	if e = courierStep(o, "copies_verified"); e != nil {
		return st, e
	}
	original, e := courierRead(filepath.Join(target, "agents", name+".json"), 1<<20)
	if e != nil {
		return st, e
	}
	creds, e := courierCredentials(original, name, o.Binding)
	if e != nil {
		return st, ErrCourierMigration
	}
	credentialPath := filepath.Join(target, "credential.json")
	if !j.CredentialReady {
		if j.Keychain {
			creds, e = o.Secrets.Read(ctx, creds)
			if e != nil {
				return st, e
			}
		}
		if creds.AccessToken == "" {
			return st, ErrCourierMigration
		}
		data, e := marshalCredentials(creds)
		if e != nil {
			return st, e
		}
		data = append(data, '\n')
		if e = courierWrite(credentialPath, data); e != nil {
			return st, e
		}
		read, e := courierRead(credentialPath, 1<<20)
		if e != nil || !bytes.Equal(read, data) {
			return st, ErrCourierMigration
		}
		j.CredentialDigest = courierDigest(data)
		j.CredentialReady = true
		if e = courierSaveJournal(journalPath, j); e != nil {
			return st, e
		}
	}
	credential, e := courierRead(credentialPath, 1<<20)
	if e != nil || courierDigest(credential) != j.CredentialDigest {
		return st, ErrCourierMigration
	}
	if creds, e = courierMaterializedCredentials(credential, name, o.Binding); e != nil || creds.AccessToken == "" {
		return st, ErrCourierMigration
	}
	if e = courierStep(o, "verified"); e != nil {
		return st, e
	}
	// Rollback credentials live only beneath the private rollback directory.
	// This precedes every source cutover effect and is independently verified.
	if e = courierPrivateCopy(courierBackup(target), original); e != nil {
		return st, e
	}
	if e = courierStep(o, "rollback_source_copied"); e != nil {
		return st, e
	}
	if e = courierPrivateCopy(courierRollbackCredential(target), credential); e != nil {
		return st, e
	}
	if e = courierStep(o, "rollback_credential_copied"); e != nil {
		return st, e
	}
	j.RollbackReady = true
	if e = courierSaveJournal(journalPath, j); e != nil {
		return st, e
	}
	if e = courierVerifyRollback(target, j, name); e != nil {
		return st, e
	}
	if e = courierStep(o, "rollback_verified"); e != nil {
		return st, e
	}
	if e = ctx.Err(); e != nil {
		return st, e
	}
	// Persist nonsecret cutover intent using the same sealed writer before any
	// source effect. Losing the private journal afterward cannot authorize an
	// abort merely because source metadata or a recreated backend still exists.
	if !st.Migration.CutoverStarted {
		migration := *st.Migration
		migration.CutoverStarted = true
		st.Migration = &migration
		st, e = saveStateLocked(p, name, st, st, ordinaryWriterFence(st, o.Now), false, true)
		if e != nil {
			return st, e
		}
	}
	if e = courierStep(o, "cutover_ready"); e != nil {
		return st, e
	}
	if j.Keychain {
		if e = o.Secrets.Delete(ctx, creds); e != nil {
			return st, e
		}
		source, e := courierRead(p.AgentFile(name), 1<<20)
		if e != nil {
			return st, e
		}
		if e = courierSourceCredentialCheck(source, true); e != nil {
			return st, e
		}
	} else {
		source := p.AgentFile(name)
		b, e := courierRead(source, 1<<20)
		tombstone := courierTombstone(name, at)
		if e != nil {
			return st, e
		}
		if !bytes.Equal(b, tombstone) {
			if courierDigest(b) != courierCredentialDigest(j, name) {
				return st, ErrCourierMigration
			}
			// Only nonsecret bytes enter the temporary file. No secret-bearing
			// source rename or backup is created by the atomic replacement.
			if e = courierWrite(source, tombstone); e != nil {
				return st, e
			}
		}
		if b, e = courierRead(source, 1<<20); e != nil || !bytes.Equal(b, tombstone) {
			return st, ErrCourierMigration
		}
	}
	if e = courierStep(o, "cutover"); e != nil {
		return st, e
	}
	if e = courierStep(o, "secret_deleted"); e != nil {
		return st, e
	}
	st.Migration = &IdentityMigration{Phase: "done", CourierBase: o.To, At: at, CutoverStarted: true}
	st, e = saveStateLocked(p, name, st, st, ordinaryWriterFence(st, o.Now), false, true)
	if e != nil {
		return st, e
	}
	return st, courierStep(o, "done")
}

func courierSaveJournal(path string, j courierJournal) error {
	b, e := json.Marshal(j)
	if e != nil {
		return e
	}
	if len(b) > 1<<20 {
		return ErrCourierMigration
	}
	return courierWrite(path, append(b, '\n'))
}
func courierValidateJournal(j courierJournal, name string) error {
	seen := map[string]bool{}
	var total int64
	credential := false
	for _, f := range j.Files {
		clean := filepath.Clean(f.Path)
		parts := strings.Split(clean, string(filepath.Separator))
		identityPath := len(parts) >= 2 && (parts[0] == "agents" || parts[0] == "state" || parts[0] == "owner-dm") && (parts[1] == name || strings.HasPrefix(parts[1], name+"."))
		postPath := len(parts) == 4 && parts[0] == "post-operations" && parts[1] == "agents" && parts[2] == name && strings.HasSuffix(parts[3], ".json") && ValidPostOperationID(strings.TrimSuffix(parts[3], ".json"))
		if clean != f.Path || !identityPath && !postPath || strings.HasSuffix(clean, ".lock") || strings.Contains(clean, ".migrated-") || seen[clean] || !lowerHex(f.Digest, 64) || f.Size < 0 || f.Size > courierMaxFile {
			return ErrCourierMigration
		}
		seen[clean] = true
		total += f.Size
		if clean == filepath.Join("agents", name+".json") {
			credential = true
		}
	}
	if !credential || total > courierMaxTotal || j.CredentialReady && !lowerHex(j.CredentialDigest, 64) || j.RollbackReady && !j.CredentialReady {
		return ErrCourierMigration
	}
	return nil
}
func courierCredentialDigest(j courierJournal, name string) string {
	for _, f := range j.Files {
		if f.Path == filepath.Join("agents", name+".json") {
			return f.Digest
		}
	}
	return ""
}
func courierVerify(target string, j courierJournal) error {
	for _, f := range j.Files {
		if strings.HasPrefix(f.Path, "post-operations/") {
			b, e := courierRead(filepath.Join(target, "post-operations", "agents", j.Identity, ".lock"), 1)
			if e != nil || len(b) != 0 {
				return ErrCourierMigration
			}
			break
		}
	}
	for _, f := range j.Files {
		b, e := courierRead(filepath.Join(target, f.Path), courierMaxFile)
		if e != nil || int64(len(b)) != f.Size || courierDigest(b) != f.Digest {
			return ErrCourierMigration
		}
	}
	if j.CredentialReady {
		b, e := courierRead(filepath.Join(target, "credential.json"), 1<<20)
		if e != nil || courierDigest(b) != j.CredentialDigest {
			return ErrCourierMigration
		}
	}
	return nil
}

func courierVerifyRollback(target string, j courierJournal, name string) error {
	if !j.RollbackReady || !j.CredentialReady {
		return ErrCourierMigration
	}
	b, e := courierRead(courierBackup(target), 1<<20)
	if e != nil || courierDigest(b) != courierCredentialDigest(j, name) {
		return ErrCourierMigration
	}
	b, e = courierRead(courierRollbackCredential(target), 1<<20)
	if e != nil || courierDigest(b) != j.CredentialDigest {
		return ErrCourierMigration
	}
	return nil
}

// A partial pre-cutover transaction may still use its unchanged source. Once
// the rollback journal is committed, credentials must come from private custody
// even if a redundant identity/runtime copy is intact.
func courierRollbackSource(p Paths, name, target string, j courierJournal) ([]byte, error) {
	b, e := courierRead(courierBackup(target), 1<<20)
	if errors.Is(e, fs.ErrNotExist) && !j.RollbackReady {
		b, e = courierRead(p.AgentFile(name), 1<<20)
	}
	if e != nil || courierDigest(b) != courierCredentialDigest(j, name) {
		return nil, ErrCourierMigration
	}
	return b, nil
}
func courierClearFence(p Paths, name string, st State, now time.Time) (State, error) {
	st.Migration = nil
	return saveStateLocked(p, name, st, st, ordinaryWriterFence(st, now), false, true)
}
func courierRollback(ctx context.Context, p Paths, name string, st State, o CourierMigrationOptions, target string, j courierJournal) (State, error) {
	original, e := courierRollbackSource(p, name, target, j)
	if e != nil || st.Migration.Phase == "done" && !j.RollbackReady {
		return st, ErrCourierMigration
	}
	if j.RollbackReady {
		if e = courierVerifyRollback(target, j, name); e != nil {
			return st, e
		}
	}
	// Incomplete copies may be restored from unchanged source data. A missing or
	// corrupt target is never trusted after cutover, and the fence stays present.
	for _, f := range j.Files {
		if f.Path == filepath.Join("agents", name+".json") {
			continue // credential bytes are read only from private rollback custody
		}
		b, e := courierRead(filepath.Join(target, f.Path), courierMaxFile)
		if errors.Is(e, fs.ErrNotExist) {
			b, e = courierRead(courierSource(p, f.Path), courierMaxFile)
		}
		if e != nil || courierDigest(b) != f.Digest {
			return st, ErrCourierMigration
		}
	}
	if j.RollbackReady {
		data, e := courierRead(courierRollbackCredential(target), 1<<20)
		if e != nil || courierDigest(data) != j.CredentialDigest {
			return st, ErrCourierMigration
		}
		creds, e := courierMaterializedCredentials(data, name, o.Binding)
		if e != nil || creds.AccessToken == "" {
			return st, ErrCourierMigration
		}
		if j.Keychain {
			if e = o.Secrets.Restore(ctx, creds); e != nil {
				return st, e
			}
		}
	}
	for _, f := range j.Files {
		if f.Path == filepath.Join("state", name+".json") {
			continue
		} // source fence is authoritative, cleared last with L02 sealing
		dest := courierSource(p, f.Path)
		var data []byte
		var e error
		if f.Path == filepath.Join("agents", name+".json") {
			data, e = original, nil
		} else {
			data, e = courierRead(filepath.Join(target, f.Path), courierMaxFile)
		}
		if errors.Is(e, fs.ErrNotExist) {
			data, e = courierRead(dest, courierMaxFile)
		}
		if e != nil || courierDigest(data) != f.Digest {
			return st, ErrCourierMigration
		}
		if e = courierWrite(dest, data); e != nil {
			return st, e
		}
		read, e := courierRead(dest, courierMaxFile)
		if e != nil || courierDigest(read) != f.Digest {
			return st, ErrCourierMigration
		}
	}
	if e := courierStep(o, "restored"); e != nil {
		return st, e
	}
	if e := ctx.Err(); e != nil {
		return st, e
	}
	restored := st
	for _, f := range j.Files {
		if f.Path == filepath.Join("state", name+".json") {
			data, e := courierRead(filepath.Join(target, f.Path), courierMaxFile)
			if errors.Is(e, fs.ErrNotExist) {
				data, e = courierRead(p.StateFile(name), courierMaxFile)
			}
			if e != nil || courierDigest(data) != f.Digest || unmarshalState(data, &restored) != nil {
				return st, ErrCourierMigration
			}
		}
	}
	restored.Migration = nil
	return saveStateLocked(p, name, st, restored, ordinaryWriterFence(st, o.Now), false, true)
}

func courierCredentials(data []byte, name string, binding *ConnectionBinding) (Credentials, error) {
	var c Credentials
	if len(data) > 1<<20 || json.Unmarshal(data, &c) != nil || !strings.HasPrefix(c.UserID, "@"+name+":") || len(c.UserID) <= len(name)+2 || strings.ContainsAny(c.UserID, "\x00\r\n\t \"\\") {
		return Credentials{}, ErrCourierMigration
	}
	if binding != nil && ValidateBoundCredentials(c, *binding) != nil {
		return Credentials{}, ErrCourierMigration
	}
	return c, nil
}

// The file backend carries a materialized value while its binding remains the
// source routing snapshot. Validate that snapshot without mistaking the file
// backend for named Keychain metadata; rollback restores the original bytes.
func courierMaterializedCredentials(data []byte, name string, binding *ConnectionBinding) (Credentials, error) {
	c, e := courierCredentials(data, name, nil)
	if e != nil {
		return c, e
	}
	if binding != nil {
		metadata := c
		if binding.Connection.Store.Backend == "keychain" {
			metadata.AccessToken = ""
			metadata.Password = ""
		}
		if ValidateBoundCredentials(metadata, *binding) != nil {
			return Credentials{}, ErrCourierMigration
		}
	}
	return c, nil
}

// Admission uses the credential schema as a positive metadata allowlist. Unknown
// fields (including refresh tokens) cannot hide secrets in legacy loose JSON.
// File credentials legitimately carry access_token/password until tombstoning;
// Keychain metadata must already be clean, including with no named binding.
func courierSourceCredentialCheck(data []byte, keychain bool) error {
	var metadata Credentials
	if metadata.decodeJSON(data, true) != nil || metadata.ValidateWaitKeywords() != nil {
		return ErrCourierSourceSecrets
	}
	// The legacy writer deliberately retains repeated optional values. Walk
	// every occurrence, so an empty successor cannot conceal a secret-bearing
	// predecessor, without refusing nonsecret wire forms the writer emits.
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ErrCourierSourceSecrets
	}
	for decoder.More() {
		token, err = decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return ErrCourierSourceSecrets
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return ErrCourierSourceSecrets
		}
		switch key {
		case "user_id", "device_id", "homeserver", "wait_keywords", "own_room", "connection_binding":
		case "access_token", "password":
			if keychain {
				var empty string
				if json.Unmarshal(value, &empty) != nil || empty != "" {
					return ErrCourierSourceSecrets
				}
			}
		default:
			return ErrCourierSourceSecrets
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return ErrCourierSourceSecrets
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return ErrCourierSourceSecrets
	}

	return nil
}

func courierLexicalOverlap(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	return err != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Walk both sets of existing ancestors: aliases can share a device/inode
// without sharing any path spelling. Compare both device and inode, not inode alone.
// The stat seam is solely for testing directory aliases forbidden to os.Link.
func courierCheckOverlap(to string, roots []string, stat func(string) (os.FileInfo, error)) error {
	for _, root := range roots {
		a, b := root, to
		if runtime.GOOS == "darwin" {
			a, b = strings.ToLower(a), strings.ToLower(b)
		}
		if courierLexicalOverlap(a, b) || courierLexicalOverlap(b, a) {
			return ErrCourierMigration
		}
		rootInfo, err := stat(root)
		if err != nil {
			return ErrCourierMigration
		}
		for ancestor := to; ; ancestor = filepath.Dir(ancestor) {
			info, err := stat(ancestor)
			if err == nil && courierSameIdentity(rootInfo, info) {
				return ErrCourierMigration
			}
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return ErrCourierMigration
			}
			if ancestor == filepath.Dir(ancestor) {
				break
			}
		}
		toInfo, err := stat(to)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return ErrCourierMigration
		}
		if err == nil {
			for ancestor := root; ; ancestor = filepath.Dir(ancestor) {
				info, err := stat(ancestor)
				if err != nil {
					return ErrCourierMigration
				}
				if courierSameIdentity(toInfo, info) {
					return ErrCourierMigration
				}
				if ancestor == filepath.Dir(ancestor) {
					break
				}
			}
		}
	}
	return nil
}

func courierSameIdentity(a, b os.FileInfo) bool {
	sa, oka := a.Sys().(*syscall.Stat_t)
	sb, okb := b.Sys().(*syscall.Stat_t)
	return oka && okb && sa.Dev == sb.Dev && sa.Ino == sb.Ino
}
