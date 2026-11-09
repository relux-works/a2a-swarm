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
one LF. `station` uses the same registry grammar and is persisted as one
LF-terminated line in the same atomic publication.
The secret uses canonical padded standard base64 and decodes to exactly 32 bytes.
The socket pointer starts with `/`, is valid UTF-8, and is 1 through 103 bytes
including the leading slash, excluding the file LF. It contains no NUL, CR, LF
or BOM. This is a byte bound, not a character count or an ASCII restriction.
Spaces, repeated slashes and dot components are permitted in the pointer;
enrollment neither normalizes nor opens it nor checks that it exists. The
separate caller-owned output directory still has the path safety checks below.

The final directory is caller-owned at 0700, with exactly four caller-owned,
single-link regular files at 0600: `courier.sock` contains the socket path bytes
(a pointer file, followed by exactly one LF), `identity` contains the selector and one LF, `station.key` contains the 32 raw bytes, and `station` contains the station name and one LF. Existing incomplete, foreign-owned,
wrong-mode or extra-entry directories fail even with `--rotate`. No permissions
are repaired. Without `--rotate`, any existing `.a2a` directory fails.
`--rotate` can also install an absent directory.

The target must be an absolute clean path ending in `.a2a`. Its parent is
resolved once with `EvalSymlinks`; all subsequent operations use that fixed
canonical path. A home beneath a symlinked ancestor (including macOS `/var`)
is accepted. Each canonical ancestor must be owned by root or the caller and
must not be group- or world-writable, except for root-owned sticky directories.
The canonical home itself must be owned by the station user. Symlinks at `.a2a`
or any of its four entries are refused, as are `..` and other target names.
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
enroll commands. A random 0700 sibling `.a2a-enroll-*` stages four complete,
fsynced, verified files outside the final namespace. Linux uses
`renameat2(RENAME_NOREPLACE)` for first installation and `RENAME_EXCHANGE` for
rotation; macOS uses `renameatx_np(RENAME_EXCL/RENAME_SWAP)`. Unsupported filesystems
fail closed. There is no sequential rename fallback. No temporary entry is
admitted inside `.a2a`.

A rotation atomically exchanges complete directories and removes the previous
generation and its old `station.key` staging copy after successful verification
and parent sync. **Readers must open `.a2a` once and
open all four files relative to that directory descriptor.** Opening individual
absolute file paths across a rotation cannot provide a consistent snapshot.
Files already open remain readable after unlink. A reader still opening files
or checking the namespace can refuse during rotation and retry with the current
generation. The station reader uses the pinned-descriptor protocol and checks
that the directory remains the current namespace before returning its snapshot. A legacy three-file
generation is incomplete and requires re-running enroll.

An ordinary failure before exchange leaves the previous enrollment unchanged;
an ordinary verification/durability failure after exchange exchanges it back.
A failed rollback reports an explicitly uncertain outcome and retains staging
for inspection. A crash before exchange leaves the previous enrollment, plus
possibly partial sibling staging; a crash after exchange leaves a complete new
enrollment and may leave a complete or partly removed old sibling. A failure
during old-generation removal or its final sync reports an uncertain outcome
without rolling back the verified new enrollment. Retrying never auto-promotes or deletes an
orphan. Inspect the four files using a pinned descriptor before deciding which
complete generation to retain. The operator may remove confirmed obsolete
siblings only after readers have stopped. Ordinary successful rotation retains
no old key copy. Filesystem snapshots, backups, previously copied keys and
power-loss recovery are outside this cleanup guarantee. Enrollment is a local
file operation; registration/key rotation in the daemon and custody of the input
pipe remain separate operator steps. No automatic cleanup service or new
authority is implied. Whole-machine power-loss durability and native macOS
filesystem behavior require separate qualification.

This component covers enrollment only; migration and the Courier server are
separate work. It does not claim a complete Courier migration implementation.
