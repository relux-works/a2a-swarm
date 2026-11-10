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

type daemonFile struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}
type daemonJournal struct {
	Version          int          `json:"version"`
	Identity         string       `json:"identity"`
	Source           string       `json:"source"`
	Metadata         string       `json:"metadata"`
	At               time.Time    `json:"at"`
	Keychain         bool         `json:"keychain"`
	Files            []daemonFile `json:"files"`
	CredentialDigest string       `json:"credential_digest"`
	CredentialReady  bool         `json:"credential_ready"`
	RollbackReady    bool         `json:"rollback_ready"`
}

const daemonJournalName = "migration.json"
const daemonMaxFile = 64 << 20
const daemonMaxTotal = 256 << 20

func daemonDigest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// Reject symlinks in every component, including ancestors. The selected roots
// and all per-identity directories must be private and owned by this UID. No
// permission repair, chown or inferred alternate root is attempted.
func daemonPath(path string, directory bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrDaemonMigration
	}
	cur := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(path, cur), string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return ErrDaemonMigration
		}
		if cur != path && !st.IsDir() {
			return ErrDaemonMigration
		}
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return daemonOwned(st, directory)
}

func daemonOwned(st fs.FileInfo, directory bool) error {
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return ErrDaemonMigration
	}
	if directory {
		if !st.IsDir() || st.Mode().Perm() != 0700 {
			return ErrDaemonMigration
		}
	} else {
		if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || owner.Nlink != 1 {
			return ErrDaemonMigration
		}
	}
	return nil
}

func daemonRead(path string, limit int64) ([]byte, error) {
	if err := daemonPath(path, false); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil || before.Size() > limit {
		return nil, ErrDaemonMigration
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || daemonOwned(after, false) != nil {
		return nil, ErrDaemonMigration
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, ErrDaemonMigration
	}
	return b, nil
}

func daemonMkdir(path string) error {
	if err := daemonPath(path, true); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := daemonMkdir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	if err := syncStateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return daemonPath(path, true)
}

func daemonWrite(path string, data []byte) error {
	if err := daemonMkdir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := daemonPath(path, false); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := writeSecretAtomic(path, "daemon", data); err != nil {
		return err
	}
	return syncStateDirectory(filepath.Dir(path))
}

func daemonInventory(p Paths, name string) ([]daemonFile, error) {
	var files []daemonFile
	var total int64
	for _, dir := range []string{"agents", "state", "owner-dm"} {
		root := p.Base
		if dir == "agents" {
			root = p.metadataRoot()
		}
		base := filepath.Join(root, dir)
		if err := daemonPath(base, true); errors.Is(err, fs.ErrNotExist) && dir != "agents" {
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
				return nil, ErrDaemonMigration
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
					return daemonPath(path, true)
				}
				if strings.HasSuffix(d.Name(), ".lock") {
					return daemonPath(path, false)
				}
				b, e := daemonRead(path, daemonMaxFile)
				if e != nil {
					return e
				}
				total += int64(len(b))
				if total > daemonMaxTotal || len(files) >= 4096 {
					return ErrDaemonMigration
				}
				rel, e := filepath.Rel(base, path)
				if e != nil {
					return e
				}
				files = append(files, daemonFile{Path: filepath.Join(dir, rel), Digest: daemonDigest(b), Size: int64(len(b))})
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
			return nil, ErrDaemonPostPending
		}
		for _, entry := range entries {
			if entry.Name() == ".lock" {
				continue
			}
			if !strings.HasSuffix(entry.Name(), ".json") {
				return nil, ErrDaemonPostPending
			}
			id := strings.TrimSuffix(entry.Name(), ".json")
			r, ok, err := journal.read(id) // caller holds the journal flock
			if err != nil || !ok || r.Outcome == "not_submitted" {
				return nil, ErrDaemonPostPending
			}
			path := filepath.Join(journalRoot, entry.Name())
			b, err := daemonRead(path, PostRecordLimit)
			if err != nil {
				return nil, err
			}
			total += int64(len(b))
			if total > daemonMaxTotal || len(files) >= 4096 {
				return nil, ErrDaemonMigration
			}
			files = append(files, daemonFile{Path: filepath.Join("post-operations", "agents", name, entry.Name()), Digest: daemonDigest(b), Size: int64(len(b))})
		}
	}
	return files, nil
}

func daemonSource(p Paths, rel string) string {
	root := p.Base
	if strings.HasPrefix(rel, "agents/") {
		root = p.metadataRoot()
	}
	return filepath.Join(root, rel)
}
func daemonBackup(target string) string {
	return filepath.Join(target, "rollback", "source-credential.json")
}

func daemonRollbackCredential(target string) string {
	return filepath.Join(target, "rollback", "credential.json")
}

// A closed nonsecret schema, derived from the sealed transaction, also makes
// a completed atomic replacement distinguishable after a crash before done.
func daemonTombstone(name string, at time.Time) []byte {
	b, _ := json.Marshal(struct {
		Migrated bool      `json:"migrated_to_daemon"`
		At       time.Time `json:"at"`
		Identity string    `json:"identity"`
	}{true, at.UTC(), name})
	return append(b, '\n')
}

// Resume may fill a missing private copy but never overwrite a damaged one.
func daemonPrivateCopy(path string, data []byte) error {
	b, e := daemonRead(path, daemonMaxFile)
	if errors.Is(e, fs.ErrNotExist) {
		if e = daemonWrite(path, data); e != nil {
			return e
		}
		b, e = daemonRead(path, daemonMaxFile)
	}
	if e != nil || !bytes.Equal(b, data) {
		return ErrDaemonMigration
	}
	return nil
}
func daemonStep(o DaemonMigrationOptions, label string) error {
	if o.step != nil {
		return o.step(label)
	}
	return nil
}

// MigrateDaemon takes consumer -> credential -> post attempt -> journal ->
// state locks, retaining every
// inode forever. It uses L02's locked writer/seal, never an alternate State
// encoding. Explicit resume/rollback is required after any interruption.
func MigrateDaemon(ctx context.Context, p Paths, name string, o DaemonMigrationOptions) (err error) {
	// All errors are closed: provider errors and paths may contain private data.
	defer func() {
		if err != nil && !errors.Is(err, ErrDaemonSourceSecrets) && !errors.Is(err, ErrDaemonPostPending) {
			err = ErrDaemonMigration
		}
	}()
	if !daemonPublishSupported || ValidateName(name) != nil || o.Now.IsZero() || !filepath.IsAbs(o.To) || filepath.Clean(o.To) != o.To || o.Resume && o.Rollback || o.Keychain && o.Secrets == nil {
		return ErrDaemonMigration
	}
	if err = daemonPath(p.Base, true); err != nil {
		return err
	}
	if err = daemonPath(p.metadataRoot(), true); err != nil {
		return err
	}
	if p.metadataRoot() != p.Base {
		if _, e := os.Lstat(filepath.Join(p.Base, "agents", name+".json")); !errors.Is(e, fs.ErrNotExist) {
			return ErrDaemonMigration
		}
	}
	if err = daemonCheckOverlap(o.To, []string{p.Base, p.metadataRoot()}, os.Stat); err != nil {
		return err
	}
	if _, e := daemonRead(p.StateFile(name), 1<<20); e != nil && !errors.Is(e, fs.ErrNotExist) {
		return e
	}
	// Refuse occupied identities before provisioning even lock files; recheck
	// under both lifetime/state locks to close the admission race.
	st, err := LoadState(p, name)
	if err != nil {
		return err
	}
	if daemonOccupied(st) {
		return ErrDaemonMigration
	}
	if err = daemonMkdir(filepath.Join(p.Base, "state")); err != nil {
		return err
	}
	if err = daemonPath(filepath.Join(p.metadataRoot(), "agents"), true); err != nil {
		return err
	}
	for _, path := range []string{filepath.Join(p.Base, "state", name+".runtime-consumer.lock"), filepath.Join(p.Base, "state", name+".lock"), filepath.Join(p.metadataRoot(), "agents", "."+name+".lock")} {
		if e := daemonPath(path, false); e != nil && !errors.Is(e, fs.ErrNotExist) {
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
	if e := daemonPath(attempt, false); e != nil && !errors.Is(e, fs.ErrNotExist) {
		return e
	}
	releaseAttempt, e := lockFileBy(attempt, time.Now())
	if e != nil {
		return ErrDaemonPostPending
	}
	defer releaseAttempt()
	journal, e := OpenPostJournal(p, name, false, false)
	if e == nil {
		defer journal.Close()
		f, e := openPostOperationLock(journal.root, false, openPostOperationFile)
		if e != nil {
			return ErrDaemonPostPending
		}
		defer f.Close()
		// Nonblocking admission avoids waiting behind an unfinished journal writer.
		if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
			return ErrDaemonPostPending
		}
		defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	} else if _, statErr := os.Lstat(filepath.Join(p.Base, "post-operations", "agents", name)); !errors.Is(statErr, fs.ErrNotExist) {
		return ErrDaemonPostPending
	}
	_, err = withStateLock(p, name, func(path string) (func(), error) { return lockFileBy(path, time.Now()) }, func(st State) (State, error) {
		if daemonOccupied(st) {
			return st, ErrDaemonMigration
		}
		return migrateDaemonLocked(ctx, p, name, st, o)
	})
	return err
}

func daemonOccupied(s State) bool {
	return s.Lease != nil || s.HostEnrollment != nil || s.RefuseMonitorConsumer() != nil || s.Terminal != nil && !s.Terminal.Stopped || s.Runtime != nil && !s.Runtime.Retired
}

func migrateDaemonLocked(ctx context.Context, p Paths, name string, st State, o DaemonMigrationOptions) (State, error) {
	target := filepath.Join(o.To, "identities", name)
	fresh := st.Migration == nil
	if fresh && (o.Resume || o.Rollback) {
		return st, ErrDaemonMigration
	}
	if !fresh && (!o.Resume && !o.Rollback || st.Migration.DaemonBase != o.To || st.Migration.At.IsZero() || st.Migration.Phase != "in_progress" && st.Migration.Phase != "done") {
		return st, ErrDaemonMigration
	}
	var j daemonJournal
	if fresh {
		if _, e := os.Lstat(target); !errors.Is(e, fs.ErrNotExist) {
			return st, ErrDaemonMigration
		}
		// Verify all inputs before the first fence write. No credentials/provider
		// values are emitted, and no source namespace is inferred from a name.
		files, e := daemonInventory(p, name)
		if e != nil {
			return st, e
		}
		original, e := daemonRead(p.AgentFile(name), 1<<20)
		if e != nil {
			return st, e
		}
		if e = daemonSourceCredentialCheck(original, o.Keychain); e != nil {
			return st, e
		}
		if _, e = daemonCredentials(original, name, o.Binding); e != nil {
			return st, e
		}
		hasCredential := false
		for _, f := range files {
			if f.Path == filepath.Join("agents", name+".json") {
				hasCredential = true
			}
		}
		if !hasCredential || o.Keychain && o.Secrets == nil {
			return st, ErrDaemonMigration
		}
		// Validate target ancestors before changing the source. Parent directories
		// may be created after the fence, but an unsafe existing parent is refused.
		ancestor := filepath.Dir(target)
		for {
			if _, e = os.Lstat(ancestor); e == nil {
				if e = daemonPath(ancestor, true); e != nil {
					return st, e
				}
				break
			}
			if !errors.Is(e, fs.ErrNotExist) {
				return st, e
			}
			ancestor = filepath.Dir(ancestor)
		}
		st.Migration = &IdentityMigration{Phase: "in_progress", DaemonBase: o.To, At: o.Now.UTC()}
		st, e = saveStateLocked(p, name, st, st, ordinaryWriterFence(st, o.Now), false, true)
		if e != nil {
			return st, e
		}
		if e = daemonStep(o, "fenced"); e != nil {
			return st, e
		}
	}
	// A fence may have persisted just before creation of the target journal. In
	// that case source inputs are intact and an explicit resume can initialize it.
	at := st.Migration.At
	journalPath := filepath.Join(target, daemonJournalName)
	raw, e := daemonRead(journalPath, 1<<20)
	if errors.Is(e, fs.ErrNotExist) {
		if _, e = os.Lstat(target); !errors.Is(e, fs.ErrNotExist) {
			return st, ErrDaemonMigration
		}
		if o.Rollback {
			// A missing target is not proof that cutover never happened. Require
			// intact source credentials, no transaction backup, an in-progress sealed
			// State and a validated source inventory before aborting a pre-cutover run.
			if st.Migration.Phase != "in_progress" || st.Migration.CutoverStarted || !validWriterSeal(st) || ctx.Err() != nil {
				return st, ErrDaemonMigration
			}
			original, e := daemonRead(p.AgentFile(name), 1<<20)
			if e != nil {
				return st, e
			}
			creds, e := daemonCredentials(original, name, o.Binding)
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
					return st, ErrDaemonMigration
				}
			}
			if _, e = daemonInventory(p, name); e != nil {
				return st, e
			}
			return daemonClearFence(p, name, st, o.Now)
		}
		if e = daemonMkdir(filepath.Dir(target)); e != nil {
			return st, e
		}
		original, e := daemonRead(p.AgentFile(name), 1<<20)
		if e != nil {
			return st, e
		}
		if e = daemonSourceCredentialCheck(original, o.Keychain); e != nil {
			return st, e
		}
		files, e := daemonInventory(p, name)
		if e != nil {
			return st, e
		}
		j = daemonJournal{Version: 1, Identity: name, Source: p.Base, Metadata: p.metadataRoot(), At: at, Keychain: o.Keychain, Files: files}
		// Publish a fully initialized directory. A crash before the rename leaves
		// only an unpublished staging directory, never an unowned final target.
		staging, e := os.MkdirTemp(filepath.Dir(target), "."+name+".migration-")
		if e != nil {
			return st, e
		}
		if e = daemonSaveJournal(filepath.Join(staging, daemonJournalName), j); e != nil {
			return st, e
		}
		if _, e = os.Lstat(target); !errors.Is(e, fs.ErrNotExist) {
			return st, ErrDaemonMigration
		}
		if e = daemonStep(o, "staged"); e != nil {
			return st, e
		}
		if e = daemonPublishDirectory(staging, target); e != nil {
			return st, e
		}
		if e = syncStateDirectory(filepath.Dir(target)); e != nil {
			return st, e
		}
	} else if e != nil || DecodeConnectionJSON(raw, &j) != nil {
		return st, ErrDaemonMigration
	}
	if e = daemonStep(o, "journaled"); e != nil {
		return st, e
	}
	if j.Version != 1 || j.Identity != name || j.Source != p.Base || j.Metadata != p.metadataRoot() || !j.At.Equal(at) || j.Keychain != o.Keychain || len(j.Files) > 4096 || j.Keychain && o.Secrets == nil {
		return st, ErrDaemonMigration
	}
	if e = daemonValidateJournal(j, name); e != nil {
		return st, e
	}
	if o.Rollback {
		return daemonRollback(ctx, p, name, st, o, target, j)
	}
	if st.Migration.Phase == "done" {
		if e = daemonVerify(target, j); e != nil {
			return st, e
		}
		if e = daemonVerifyRollback(target, j, name); e != nil {
			return st, e
		}
		if j.Keychain {
			b, e := daemonRead(p.AgentFile(name), 1<<20)
			if e != nil {
				return st, e
			}
			if e = daemonSourceCredentialCheck(b, true); e != nil {
				return st, e
			}
		}
		if !j.Keychain {
			b, e := daemonRead(p.AgentFile(name), 1<<20)
			if e != nil || !bytes.Equal(b, daemonTombstone(name, at)) {
				return st, ErrDaemonMigration
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
		b, readErr := daemonRead(dest, daemonMaxFile)
		if readErr == nil {
			if daemonDigest(b) != f.Digest || int64(len(b)) != f.Size {
				return st, ErrDaemonMigration
			}
			continue
		}
		if !errors.Is(readErr, fs.ErrNotExist) {
			return st, readErr
		}
		source := daemonSource(p, f.Path)
		b, e = daemonRead(source, daemonMaxFile)
		if e != nil || daemonDigest(b) != f.Digest || int64(len(b)) != f.Size {
			return st, ErrDaemonMigration
		}
		if e = daemonWrite(dest, b); e != nil {
			return st, e
		}
		if e = daemonStep(o, "copied"); e != nil {
			return st, e
		}
	}
	for _, f := range j.Files {
		if strings.HasPrefix(f.Path, "post-operations/") {
			if e = daemonPrivateCopy(filepath.Join(target, "post-operations", "agents", name, ".lock"), nil); e != nil {
				return st, e
			}
			break
		}
	}
	if e = daemonVerify(target, j); e != nil {
		return st, e
	}
	if e = daemonStep(o, "copies_verified"); e != nil {
		return st, e
	}
	original, e := daemonRead(filepath.Join(target, "agents", name+".json"), 1<<20)
	if e != nil {
		return st, e
	}
	creds, e := daemonCredentials(original, name, o.Binding)
	if e != nil {
		return st, ErrDaemonMigration
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
			return st, ErrDaemonMigration
		}
		data, e := marshalCredentials(creds)
		if e != nil {
			return st, e
		}
		data = append(data, '\n')
		if e = daemonWrite(credentialPath, data); e != nil {
			return st, e
		}
		read, e := daemonRead(credentialPath, 1<<20)
		if e != nil || !bytes.Equal(read, data) {
			return st, ErrDaemonMigration
		}
		j.CredentialDigest = daemonDigest(data)
		j.CredentialReady = true
		if e = daemonSaveJournal(journalPath, j); e != nil {
			return st, e
		}
	}
	credential, e := daemonRead(credentialPath, 1<<20)
	if e != nil || daemonDigest(credential) != j.CredentialDigest {
		return st, ErrDaemonMigration
	}
	if creds, e = daemonMaterializedCredentials(credential, name, o.Binding); e != nil || creds.AccessToken == "" {
		return st, ErrDaemonMigration
	}
	if e = daemonStep(o, "verified"); e != nil {
		return st, e
	}
	// Rollback credentials live only beneath the private rollback directory.
	// This precedes every source cutover effect and is independently verified.
	if e = daemonPrivateCopy(daemonBackup(target), original); e != nil {
		return st, e
	}
	if e = daemonStep(o, "rollback_source_copied"); e != nil {
		return st, e
	}
	if e = daemonPrivateCopy(daemonRollbackCredential(target), credential); e != nil {
		return st, e
	}
	if e = daemonStep(o, "rollback_credential_copied"); e != nil {
		return st, e
	}
	j.RollbackReady = true
	if e = daemonSaveJournal(journalPath, j); e != nil {
		return st, e
	}
	if e = daemonVerifyRollback(target, j, name); e != nil {
		return st, e
	}
	if e = daemonStep(o, "rollback_verified"); e != nil {
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
	if e = daemonStep(o, "cutover_ready"); e != nil {
		return st, e
	}
	if j.Keychain {
		if e = o.Secrets.Delete(ctx, creds); e != nil {
			return st, e
		}
		source, e := daemonRead(p.AgentFile(name), 1<<20)
		if e != nil {
			return st, e
		}
		if e = daemonSourceCredentialCheck(source, true); e != nil {
			return st, e
		}
	} else {
		source := p.AgentFile(name)
		b, e := daemonRead(source, 1<<20)
		tombstone := daemonTombstone(name, at)
		if e != nil {
			return st, e
		}
		if !bytes.Equal(b, tombstone) {
			if daemonDigest(b) != daemonCredentialDigest(j, name) {
				return st, ErrDaemonMigration
			}
			// Only nonsecret bytes enter the temporary file. No secret-bearing
			// source rename or backup is created by the atomic replacement.
			if e = daemonWrite(source, tombstone); e != nil {
				return st, e
			}
		}
		if b, e = daemonRead(source, 1<<20); e != nil || !bytes.Equal(b, tombstone) {
			return st, ErrDaemonMigration
		}
	}
	if e = daemonStep(o, "cutover"); e != nil {
		return st, e
	}
	if e = daemonStep(o, "secret_deleted"); e != nil {
		return st, e
	}
	st.Migration = &IdentityMigration{Phase: "done", DaemonBase: o.To, At: at, CutoverStarted: true}
	st, e = saveStateLocked(p, name, st, st, ordinaryWriterFence(st, o.Now), false, true)
	if e != nil {
		return st, e
	}
	return st, daemonStep(o, "done")
}

func daemonSaveJournal(path string, j daemonJournal) error {
	b, e := json.Marshal(j)
	if e != nil {
		return e
	}
	if len(b) > 1<<20 {
		return ErrDaemonMigration
	}
	return daemonWrite(path, append(b, '\n'))
}
func daemonValidateJournal(j daemonJournal, name string) error {
	seen := map[string]bool{}
	var total int64
	credential := false
	for _, f := range j.Files {
		clean := filepath.Clean(f.Path)
		parts := strings.Split(clean, string(filepath.Separator))
		identityPath := len(parts) >= 2 && (parts[0] == "agents" || parts[0] == "state" || parts[0] == "owner-dm") && (parts[1] == name || strings.HasPrefix(parts[1], name+"."))
		postPath := len(parts) == 4 && parts[0] == "post-operations" && parts[1] == "agents" && parts[2] == name && strings.HasSuffix(parts[3], ".json") && ValidPostOperationID(strings.TrimSuffix(parts[3], ".json"))
		if clean != f.Path || !identityPath && !postPath || strings.HasSuffix(clean, ".lock") || strings.Contains(clean, ".migrated-") || seen[clean] || !lowerHex(f.Digest, 64) || f.Size < 0 || f.Size > daemonMaxFile {
			return ErrDaemonMigration
		}
		seen[clean] = true
		total += f.Size
		if clean == filepath.Join("agents", name+".json") {
			credential = true
		}
	}
	if !credential || total > daemonMaxTotal || j.CredentialReady && !lowerHex(j.CredentialDigest, 64) || j.RollbackReady && !j.CredentialReady {
		return ErrDaemonMigration
	}
	return nil
}
func daemonCredentialDigest(j daemonJournal, name string) string {
	for _, f := range j.Files {
		if f.Path == filepath.Join("agents", name+".json") {
			return f.Digest
		}
	}
	return ""
}
func daemonVerify(target string, j daemonJournal) error {
	for _, f := range j.Files {
		if strings.HasPrefix(f.Path, "post-operations/") {
			b, e := daemonRead(filepath.Join(target, "post-operations", "agents", j.Identity, ".lock"), 1)
			if e != nil || len(b) != 0 {
				return ErrDaemonMigration
			}
			break
		}
	}
	for _, f := range j.Files {
		b, e := daemonRead(filepath.Join(target, f.Path), daemonMaxFile)
		if e != nil || int64(len(b)) != f.Size || daemonDigest(b) != f.Digest {
			return ErrDaemonMigration
		}
	}
	if j.CredentialReady {
		b, e := daemonRead(filepath.Join(target, "credential.json"), 1<<20)
		if e != nil || daemonDigest(b) != j.CredentialDigest {
			return ErrDaemonMigration
		}
	}
	return nil
}

func daemonVerifyRollback(target string, j daemonJournal, name string) error {
	if !j.RollbackReady || !j.CredentialReady {
		return ErrDaemonMigration
	}
	b, e := daemonRead(daemonBackup(target), 1<<20)
	if e != nil || daemonDigest(b) != daemonCredentialDigest(j, name) {
		return ErrDaemonMigration
	}
	b, e = daemonRead(daemonRollbackCredential(target), 1<<20)
	if e != nil || daemonDigest(b) != j.CredentialDigest {
		return ErrDaemonMigration
	}
	return nil
}

// A partial pre-cutover transaction may still use its unchanged source. Once
// the rollback journal is committed, credentials must come from private custody
// even if a redundant identity/runtime copy is intact.
func daemonRollbackSource(p Paths, name, target string, j daemonJournal) ([]byte, error) {
	b, e := daemonRead(daemonBackup(target), 1<<20)
	if errors.Is(e, fs.ErrNotExist) && !j.RollbackReady {
		b, e = daemonRead(p.AgentFile(name), 1<<20)
	}
	if e != nil || daemonDigest(b) != daemonCredentialDigest(j, name) {
		return nil, ErrDaemonMigration
	}
	return b, nil
}
func daemonClearFence(p Paths, name string, st State, now time.Time) (State, error) {
	st.Migration = nil
	return saveStateLocked(p, name, st, st, ordinaryWriterFence(st, now), false, true)
}
func daemonRollback(ctx context.Context, p Paths, name string, st State, o DaemonMigrationOptions, target string, j daemonJournal) (State, error) {
	original, e := daemonRollbackSource(p, name, target, j)
	if e != nil || st.Migration.Phase == "done" && !j.RollbackReady {
		return st, ErrDaemonMigration
	}
	if j.RollbackReady {
		if e = daemonVerifyRollback(target, j, name); e != nil {
			return st, e
		}
	}
	// Incomplete copies may be restored from unchanged source data. A missing or
	// corrupt target is never trusted after cutover, and the fence stays present.
	for _, f := range j.Files {
		if f.Path == filepath.Join("agents", name+".json") {
			continue // credential bytes are read only from private rollback custody
		}
		b, e := daemonRead(filepath.Join(target, f.Path), daemonMaxFile)
		if errors.Is(e, fs.ErrNotExist) {
			b, e = daemonRead(daemonSource(p, f.Path), daemonMaxFile)
		}
		if e != nil || daemonDigest(b) != f.Digest {
			return st, ErrDaemonMigration
		}
	}
	if j.RollbackReady {
		data, e := daemonRead(daemonRollbackCredential(target), 1<<20)
		if e != nil || daemonDigest(data) != j.CredentialDigest {
			return st, ErrDaemonMigration
		}
		creds, e := daemonMaterializedCredentials(data, name, o.Binding)
		if e != nil || creds.AccessToken == "" {
			return st, ErrDaemonMigration
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
		dest := daemonSource(p, f.Path)
		var data []byte
		var e error
		if f.Path == filepath.Join("agents", name+".json") {
			data, e = original, nil
		} else {
			data, e = daemonRead(filepath.Join(target, f.Path), daemonMaxFile)
		}
		if errors.Is(e, fs.ErrNotExist) {
			data, e = daemonRead(dest, daemonMaxFile)
		}
		if e != nil || daemonDigest(data) != f.Digest {
			return st, ErrDaemonMigration
		}
		if e = daemonWrite(dest, data); e != nil {
			return st, e
		}
		read, e := daemonRead(dest, daemonMaxFile)
		if e != nil || daemonDigest(read) != f.Digest {
			return st, ErrDaemonMigration
		}
	}
	if e := daemonStep(o, "restored"); e != nil {
		return st, e
	}
	if e := ctx.Err(); e != nil {
		return st, e
	}
	restored := st
	for _, f := range j.Files {
		if f.Path == filepath.Join("state", name+".json") {
			data, e := daemonRead(filepath.Join(target, f.Path), daemonMaxFile)
			if errors.Is(e, fs.ErrNotExist) {
				data, e = daemonRead(p.StateFile(name), daemonMaxFile)
			}
			if e != nil || daemonDigest(data) != f.Digest || unmarshalState(data, &restored) != nil {
				return st, ErrDaemonMigration
			}
		}
	}
	restored.Migration = nil
	return saveStateLocked(p, name, st, restored, ordinaryWriterFence(st, o.Now), false, true)
}

func daemonCredentials(data []byte, name string, binding *ConnectionBinding) (Credentials, error) {
	var c Credentials
	if len(data) > 1<<20 || json.Unmarshal(data, &c) != nil || !strings.HasPrefix(c.UserID, "@"+name+":") || len(c.UserID) <= len(name)+2 || strings.ContainsAny(c.UserID, "\x00\r\n\t \"\\") {
		return Credentials{}, ErrDaemonMigration
	}
	if binding != nil && ValidateBoundCredentials(c, *binding) != nil {
		return Credentials{}, ErrDaemonMigration
	}
	return c, nil
}

// The file backend carries a materialized value while its binding remains the
// source routing snapshot. Validate that snapshot without mistaking the file
// backend for named Keychain metadata; rollback restores the original bytes.
func daemonMaterializedCredentials(data []byte, name string, binding *ConnectionBinding) (Credentials, error) {
	c, e := daemonCredentials(data, name, nil)
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
			return Credentials{}, ErrDaemonMigration
		}
	}
	return c, nil
}

// Admission uses the credential schema as a positive metadata allowlist. Unknown
// fields (including refresh tokens) cannot hide secrets in legacy loose JSON.
// File credentials legitimately carry access_token/password until tombstoning;
// Keychain metadata must already be clean, including with no named binding.
func daemonSourceCredentialCheck(data []byte, keychain bool) error {
	var metadata Credentials
	if metadata.decodeJSON(data, true) != nil || metadata.ValidateWaitKeywords() != nil {
		return ErrDaemonSourceSecrets
	}
	// The legacy writer deliberately retains repeated optional values. Walk
	// every occurrence, so an empty successor cannot conceal a secret-bearing
	// predecessor, without refusing nonsecret wire forms the writer emits.
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ErrDaemonSourceSecrets
	}
	for decoder.More() {
		token, err = decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return ErrDaemonSourceSecrets
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return ErrDaemonSourceSecrets
		}
		switch key {
		case "user_id", "device_id", "homeserver", "wait_keywords", "own_room", "connection_binding":
		case "access_token", "password":
			if keychain {
				var empty string
				if json.Unmarshal(value, &empty) != nil || empty != "" {
					return ErrDaemonSourceSecrets
				}
			}
		default:
			return ErrDaemonSourceSecrets
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return ErrDaemonSourceSecrets
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return ErrDaemonSourceSecrets
	}

	return nil
}

func daemonLexicalOverlap(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	return err != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Walk both sets of existing ancestors: aliases can share a device/inode
// without sharing any path spelling. Compare both device and inode, not inode alone.
// The stat seam is solely for testing directory aliases forbidden to os.Link.
func daemonCheckOverlap(to string, roots []string, stat func(string) (os.FileInfo, error)) error {
	for _, root := range roots {
		a, b := root, to
		if runtime.GOOS == "darwin" {
			a, b = strings.ToLower(a), strings.ToLower(b)
		}
		if daemonLexicalOverlap(a, b) || daemonLexicalOverlap(b, a) {
			return ErrDaemonMigration
		}
		rootInfo, err := stat(root)
		if err != nil {
			return ErrDaemonMigration
		}
		for ancestor := to; ; ancestor = filepath.Dir(ancestor) {
			info, err := stat(ancestor)
			if err == nil && daemonSameIdentity(rootInfo, info) {
				return ErrDaemonMigration
			}
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return ErrDaemonMigration
			}
			if ancestor == filepath.Dir(ancestor) {
				break
			}
		}
		toInfo, err := stat(to)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return ErrDaemonMigration
		}
		if err == nil {
			for ancestor := root; ; ancestor = filepath.Dir(ancestor) {
				info, err := stat(ancestor)
				if err != nil {
					return ErrDaemonMigration
				}
				if daemonSameIdentity(toInfo, info) {
					return ErrDaemonMigration
				}
				if ancestor == filepath.Dir(ancestor) {
					break
				}
			}
		}
	}
	return nil
}

func daemonSameIdentity(a, b os.FileInfo) bool {
	sa, oka := a.Sys().(*syscall.Stat_t)
	sb, okb := b.Sys().(*syscall.Stat_t)
	return oka && okb && sa.Dev == sb.Dev && sa.Ino == sb.Ino
}
