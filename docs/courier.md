# Courier authentication endpoint

The Courier endpoint provides local authentication-only Unix serving and
operator registration. It does not execute commands, contact Matrix, load an
identity's credentials, or provide a client, execution engine, migration,
streaming, or a service unit. The same `a2a courier` namespace also includes
`enroll`; see the [local station enrollment contract](courier-enroll.md).

## Local operator interface

- `a2a courier register-station --home ABSOLUTE_PRIVATE_HOME --station S --identity I --uid U --emit - [--rotate] [--socket ABSOLUTE_PATH]`
- `a2a courier unregister-station --home ABSOLUTE_PRIVATE_HOME --station S`
- `a2a courier serve --home ABSOLUTE_PRIVATE_HOME [--socket ABSOLUTE_PATH]`

`--home` is required and must name a dedicated Courier-owned directory at exactly
0700. Its parent must already exist. Registration may create the last directory;
serve and unregister require an existing registry. Neither A2A_CONFIG_DIR,
A2A_AGENT nor a connection profile chooses this home or a credential provider.
The socket defaults to `/private/var/run/a2a/courier.sock` on macOS and
`/run/a2a/courier.sock` on Linux. Paths must be absolute and clean, with no symlink
components; socket paths must contain 1–103 bytes including the leading slash. They must be
valid UTF-8 and contain no NUL, CR, LF or BOM; paths are not restricted to ASCII.
The shared `ValidateSocketPath` checks these v1 bytes without a pointer file's LF.
`StationKeyLen = 32` and `ProtocolVersion = 1` are exported by package courier. Pass the same socket
path to registration and serving.

Registration generates 32 random bytes and atomically replaces the station's
entry in `stations.json`. The JSON object maps each station name to exactly
`{identity, uid, secret}`: `secret` is the ORIGINAL 32 bytes encoded as canonical
padded standard base64, not a digest. `uid` is an unsigned 32-bit decimal integer.
Station and identity names both match `^[a-z0-9._=-]{1,64}$`, case sensitively.
The registry and `.stations.lock` must be owned by the effective Courier UID,
regular, singly linked, and exactly 0600. Wrong modes/owners, symlinks, malformed
registries and a busy nonblocking registry lock are refused without repair.
An existing binding is refused unless registration explicitly includes
`--rotate`. Rotation is for environment recreation or an explicit operator action
on both registration and enrollment; it is never automatic per launch. Unregister
removes that entry and its secret. Other entries survive both operations.

The only deliberate secret output is one newline-terminated, closed JSON object
on registration stdout, containing exactly `socket`, `identity`, `station`, and
`secret_b64`. Send this directly through the operator's enrollment pipe; do not
capture, log, echo, or place it in argv or environment variables. Courier does
not write station files and has no `--secret-out`. The separate station enrollment component supplies
`a2a courier enroll --dir HOME/.a2a --from - [--rotate]`. If output fails after registry replacement, the binding
may have changed even though the command failed: recover by a fresh deliberate
rotation and enrollment, without treating the old secret as current.

## Socket access policy

The socket directory must have Courier's effective UID and GID and be exactly
0711: other users can
traverse it without listing it. The socket is Courier-owned at 0666. The final
directory may be created when its parent exists. Existing directories with other
modes are refused without repair. Ancestors must also allow the intended station
to traverse the full socket path; a private ancestor keeps an override private.
No shared station group is needed. Connecting alone grants nothing: admission
requires both the HMAC and the kernel peer UID to match the registry.

The Courier home remains 0700; its registry and lock remain 0600. A Courier home
cannot double as the public socket directory. Deployment requires the intended
OS users and parent paths to exist; this command does not create OS users or
change ownership. Stations sharing one UID can read one another's station
secrets; their isolation is not a security boundary.

Serve takes a nonblocking `.courier.lock` in the socket directory. Existing socket
paths are refused rather than unlinked or adopted. On shutdown it closes active
connections, releases locks and removes only its own unchanged socket inode;
lock files remain for subsequent starts. A stale socket requires explicit operator
inspection and removal while the server is stopped.

## Wire contract

Each frame is a 4-byte unsigned big-endian JSON payload byte length, followed by
exactly that many bytes. The inclusive payload limit is **65536 bytes**, with a
minimum of one byte. Fragmented reads and short writes are completed. Oversize
lengths are refused before allocating a payload. JSON is UTF-8; objects use exact
case-sensitive fields. Unknown, missing, duplicate or trailing data are refused.
This is protocol version 1, without wire version negotiation or an alternative
schema in this leaf. The version constant does not add a new wire field.

1. Courier sends `{"type":"challenge","nonce":"<64 lowercase hex digits>"}`,
   using a fresh cryptographically random 32-byte nonce for every connection.
2. The station sends exactly
   `{"type":"hello","station":"S","identity":"I","mac":"<64 lowercase hex digits>"}`.
   The MAC is HMAC-SHA256 keyed by the ORIGINAL station secret over the raw
   32 nonce bytes followed immediately by the station's ASCII bytes and the
   identity's ASCII bytes: `raw_nonce || station || identity`. No separators,
   lengths, hex nonce text, NUL or newline enter that transcript.
3. Courier independently reads kernel peer credentials (Linux SO_PEERCRED;
   macOS LOCAL_PEERCRED), looks up the exact station, checks the registered UID
   and exact registered identity, and compares the MAC in constant time. A client
   identity cannot replace the registry binding. An unknown station still performs
   a dummy-key MAC comparison and receives the same refusal as a known station.
4. Success is exactly `{"type":"authenticated"}`. This grants no command or
   data operation. A clean disconnect ends the session; every additional frame,
   including a second hello, is refused. Idle authenticated sessions expire.

The raw concatenation is not a globally self-delimiting identifier encoding.
Authorization always uses the exact station lookup, its independently generated
secret, its stored UID and its stored identity. A MAC for another binding cannot
select or override that binding, even when two name pairs concatenate identically.
Registry bytes belong exclusively to the trusted Courier operator; sharing keys
between station entries is unsupported. Rotation is read on each new hello and
makes old keys fail; existing authenticated sockets carry no executable authority.

Every connection refusal attempts the same frame `{"type":"refused"}`, writes
exactly one fixed line `courier: refused` to stderr, then closes. No client bytes,
names, UID, MAC, nonce, secret or reason appear in refusals or logs. This hides
station existence in the response schema and log; it is not a constant-time
network service or protection against an endpoint owner inspecting its registry.
Startup/operator failures are closed local errors rather than wire responses.

The application admits at most 64 active connections, including authenticated
ones. A handshake has a total 5-second read/write deadline; an authenticated
session has a 5-second idle deadline. Refusal writes have a 1-second deadline,
including capacity refusals. The registry is at most 512 KiB and 1024 stations.
Busy registry locks fail closed instead of waiting. The caller must drain the
server's fixed stderr log output. No request allocates from a client-declared
unbounded length or spawns an execution engine.

## Reference alignment and validation limits

The framing layout matches the stationlink reference's four-byte big-endian
length prefix. That reference permits **1 MiB** and uses a different message and
authentication schema. Matching this layout does not imply wire interoperability.

Tests use synthetic registrations, same-UID kernel Unix sockets and injected UID
mismatches. Socket mode tests check the public traversal/connect permission bits.
A rootless host with only one mapped UID cannot exercise a genuinely different
kernel UID or chown a private fixture to another host UID; injected UID checks and
synthetic fstat owner metadata cover those refusals without claiming a multi-user
runtime qualification. Linux checks do not qualify macOS runtime behavior; macOS
CI and independent review remain necessary. No test creates host users or performs
real enrollment.
