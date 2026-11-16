//go:build darwin || linux

package devicelist

import (
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// snapshotProfile is the filesystem profile of the snapshot file: the trusted
// root that the descriptor walk starts at, and the UID that owns the root,
// every directory below it on the path and the file. On a host it is
// hostSnapshotProfile and nothing else; it is not configurable. Controls use
// a profile rooted in a directory of their own, because an unprivileged test
// cannot make root-owned files.
type snapshotProfile struct {
	root string
	uid  uint32
}

// hostSnapshotProfile: the walk starts at "/", and root owns everything.
var hostSnapshotProfile = snapshotProfile{root: "/", uid: 0}

// snapshotOwnerCheck decides whether a stat'd object is owned by the UID of
// the profile. A variable only so controls can plant an ownership bypass and
// prove that the unsafe-file negatives detect it; production never reassigns
// it.
var snapshotOwnerCheck = func(st *unix.Stat_t, uid uint32) bool { return st.Uid == uid }

var effectiveUID = os.Geteuid
var snapshotOpenat = unix.Openat // Test seam for a rename between open and fstat.
var errSnapshotUnlinked = errors.New("devicelist: snapshot replaced during open")

// snapshotStatSafe applies the profile of the chain gateway contract,
// section 5: ancestors have no group/other write bits, the output parent is
// exactly 0755, and the regular file is exactly 0644 with one link. All are
// profile-owned. Linux rejects POSIX ACL xattrs unless the filesystem returns
// ENOTSUP (that filesystem's ACLs are not checked). Darwin does not check ACLs
// and therefore has a weaker-than-contract profile.
func snapshotStatSafe(f *os.File, profile snapshotProfile, directory, parent bool) error {
	fd := int(f.Fd())
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		return ErrSnapshotUnsafe
	}
	mode := uint32(st.Mode)
	if directory {
		if mode&unix.S_IFMT != unix.S_IFDIR || mode&0022 != 0 || (parent && mode&07777 != 0755) {
			return ErrSnapshotUnsafe
		}
	} else if uint64(st.Nlink) == 0 {
		return errSnapshotUnlinked
	} else if mode&unix.S_IFMT != unix.S_IFREG || mode&07777 != 0644 || uint64(st.Nlink) != 1 {
		return ErrSnapshotUnsafe
	}
	if !snapshotOwnerCheck(&st, profile.uid) || !snapshotNoACL(fd, directory) {
		return ErrSnapshotUnsafe
	}
	return nil
}

// readSnapshotFile opens the file without following any symlink, by a walk
// relative to descriptors from the trusted root, applies snapshotStatSafe to
// every opened component, the root included, and reads at most the size
// bound plus one byte from the descriptor it validated. It never writes,
// creates or repairs. The reader is not root: a root agent could write any
// snapshot, so the file would protect nothing.
func readSnapshotFile(profile snapshotProfile, path string) ([]byte, error) {
	if effectiveUID() == 0 {
		return nil, ErrSnapshotUnsafe
	}
	prefix := strings.TrimSuffix(profile.root, "/") + "/"
	if !strings.HasPrefix(path, prefix) || len(path) == len(prefix) {
		return nil, ErrSnapshotUnsafe
	}
	parts := strings.Split(path[len(prefix):], "/")
	fd, err := unix.Open(profile.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrSnapshotUnsafe
	}
	current := os.NewFile(uintptr(fd), "chain-snapshot-dir")
	defer func() { current.Close() }()
	for i, part := range parts {
		// current is the directory that contains part.
		if err := snapshotStatSafe(current, profile, true, i == len(parts)-1); err != nil {
			return nil, err
		}
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		} else {
			// A FIFO must not block the open before its type is checked.
			flags |= unix.O_NONBLOCK
		}
		for attempt := 0; attempt < 2; attempt++ {
			next, err := snapshotOpenat(int(current.Fd()), part, flags, 0)
			if errors.Is(err, unix.ENOENT) {
				return nil, ErrSnapshotUnavailable
			}
			if err != nil {
				return nil, ErrSnapshotUnsafe
			}
			opened := os.NewFile(uintptr(next), "chain-snapshot")
			if i < len(parts)-1 {
				current.Close()
				current = opened
				break
			}
			err = snapshotStatSafe(opened, profile, false, false)
			if err != nil {
				opened.Close()
				if err == errSnapshotUnlinked && attempt == 0 {
					continue
				}
				return nil, ErrSnapshotUnsafe
			}
			raw, readErr := io.ReadAll(io.LimitReader(opened, MaxSnapshotBytes+1))
			opened.Close()
			if readErr != nil {
				return nil, ErrSnapshotUnsafe
			}
			return raw, nil
		}
	}
	return nil, ErrSnapshotUnsafe
}

// ReadSnapshot reads the snapshot file of the host configuration under the
// host filesystem profile, parses it, checks its bindings and labels it for
// the check. It returns:
//
//   - a view, its label (fresh, stale or unknown) and no error;
//   - no view, the label invalid and the reason: ErrSnapshotUnsafe for the
//     filesystem profile, ErrSnapshotInvalid for the schema, the network, the
//     watch set or a write time in the future, ErrCapacity for the size,
//     ErrInvalid for a malformed configuration;
//   - no view, unknown and ErrSnapshotUnavailable when the file is absent.
//
// On any read failure, pass UnavailableView{Absent: errors.Is(err,
// ErrSnapshotUnavailable)} to Admit to obtain a per-event unavailable verdict.
//
// The label is the one of this check only. Admit labels the view again at
// every use, so a view that is kept does not stay young.
//
// Darwin does not check ACLs: its filesystem profile is weaker than the
// contract and requires separate native qualification before installation.
// Linux rejects POSIX ACL xattrs, but ENOTSUP is accepted: a filesystem without
// the POSIX ACL xattr namespace is not checked for ACLs.
func ReadSnapshot(config SnapshotConfig, at Check) (*Snapshot, Freshness, error) {
	return readSnapshot(hostSnapshotProfile, config, at)
}

func readSnapshot(profile snapshotProfile, config SnapshotConfig, at Check) (*Snapshot, Freshness, error) {
	if !config.valid() {
		return nil, LabelInvalid, ErrInvalid
	}
	raw, err := readSnapshotFile(profile, config.Path)
	if errors.Is(err, ErrSnapshotUnavailable) {
		return nil, LabelUnknown, err
	}
	if err != nil {
		return nil, LabelInvalid, err
	}
	view, err := newSnapshot(raw, config)
	if err != nil {
		return nil, LabelInvalid, err
	}
	label := view.Freshness(at)
	if label == LabelInvalid {
		return nil, LabelInvalid, ErrSnapshotInvalid
	}
	return view, label, nil
}
