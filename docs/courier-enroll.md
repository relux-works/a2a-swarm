# Local Courier enrollment

`a2a courier enroll --dir /absolute/station/home/.a2a --from - [--rotate]`
consumes the station registration producer's pipe. It performs no registration,
network request, credential migration, service start or identity selection.
Successful stdout is the fixed line `Enrollment installed.`; errors contain no
input data. A downstream output error can follow a successful local installation;
inspect the enrollment before rerunning with `--rotate`.

Input is exactly one LF-terminated JSON object, followed by EOF, at most 4096
bytes including LF. Its four required, case-sensitive string fields are
`socket`, `identity`, `station`, and `secret_b64`. Unknown or duplicate fields,
extra JSON, invalid types, physical multiline input and invalid UTF-8 fail.
`identity` is a bare selector matching `[a-z0-9._=-]{1,64}`; the file adds exactly
one LF. `station` is a nonempty string of at most 255 printable ASCII bytes
without spaces. Stationlink reference names such as `id/swarma-station-1` remain
valid; station is not a path or a replacement identity. It is validated but not
persisted, because this enrollment format defines only three files. The future
client protocol must resolve how to obtain the station value for authentication.
The secret uses canonical padded standard base64 and decodes to exactly 32 bytes.
The socket pointer starts with `/`, is valid UTF-8, and is 1 through 103 bytes
including the leading slash, excluding the file LF. It contains no NUL, CR, LF
or BOM. This is a byte bound, not a character count or an ASCII restriction.
Spaces, repeated slashes and dot components are permitted in the pointer;
enrollment neither normalizes nor opens it nor checks that it exists. The
separate caller-owned output directory still has the path safety checks below.

The final directory is caller-owned at 0700, with exactly three caller-owned,
single-link regular files at 0600: `courier.sock` contains the socket path bytes
(a pointer file, followed by exactly one LF), `identity` contains the selector and one LF, and
`station.key` contains the 32 raw bytes. Existing incomplete, foreign-owned,
wrong-mode or extra-entry directories fail even with `--rotate`. No permissions
are repaired. Without `--rotate`, any existing `.a2a` directory fails.
`--rotate` can also install an absent directory.

The target must be an absolute clean path ending in `.a2a`. Its parent is
resolved once with `EvalSymlinks`; all subsequent operations use that fixed
canonical path. A home beneath a symlinked ancestor (including macOS `/var`)
is accepted. Each canonical ancestor must be owned by root or the caller and
must not be group- or world-writable, except for root-owned sticky directories.
The canonical home itself must be owned by the station user. Symlinks at `.a2a`
or any of its three entries are refused, as are `..` and other target names.
Directory-fd traversal uses `O_NOFOLLOW` on the canonical components and rechecks
linked inodes and modes before publication. The original alias is never resolved
again during installation. Every reader of `.a2a`, including enrollment rotation, must receive an absolute
path equal to `EvalSymlinks(dir)`. A noncanonical reader input is refused, even
when its resolved form would be safe. The caller supplies the canonical form;
no station-home path record is stored. A previously canonical path that becomes
a symlink therefore refuses instead of silently selecting another namespace.
This is a POSIX mode/UID
contract: ACLs granting another user write access are outside its qualification.
A hostile process running under the same UID can already read and change these
files and is outside this isolation boundary.

## Transaction, readers and interruption

The parent directory inode is locked exclusively and nonblocking for concurrent
enroll commands. A random 0700 sibling `.a2a-enroll-*` stages three complete,
fsynced, verified files outside the final namespace. Linux uses
`renameat2(RENAME_NOREPLACE)` for first installation and `RENAME_EXCHANGE` for
rotation; macOS uses `renameatx_np(RENAME_EXCL/RENAME_SWAP)`. Unsupported filesystems
fail closed. There is no sequential rename fallback. No temporary entry is
admitted inside `.a2a`.

A rotation atomically exchanges complete directories and retains the previous
generation under the staging sibling name. **Readers must open `.a2a` once and
open all three files relative to that directory descriptor.** Opening individual
absolute file paths across a rotation cannot provide a consistent snapshot.
Old generations remain intact so a pinned reader can finish opening its files.
This leaf adds no consumer and makes no claim that existing consumers implement
that protocol. Integration must preserve this requirement.

An ordinary failure before exchange leaves the previous enrollment unchanged;
an ordinary verification/durability failure after exchange exchanges it back.
A failed rollback reports an explicitly uncertain outcome and retains staging
for inspection. A crash before exchange leaves the previous enrollment, plus
possibly partial sibling staging; a crash after exchange leaves a complete new
enrollment and complete old sibling. Retrying never auto-promotes or deletes an
orphan. Inspect the three files using a pinned descriptor before deciding which
complete generation to retain. The operator may remove confirmed obsolete
siblings only after readers have stopped. No automatic cleanup service or new
authority is implied. Whole-machine power-loss durability and native macOS
filesystem behavior require separate qualification.

This component covers enrollment only; migration and the Courier server are
separate work. It does not claim a complete Courier migration implementation.
