# Courier station commands

Courier runs existing a2a commands in process for an authenticated station's
registered identity. Identity credentials and delivery state stay in the Courier
home. No shell or second a2a process executes the request.

## Operator setup

- `a2a courier register-station --home ABSOLUTE_PRIVATE_HOME --station S --identity I --uid U --emit - [--rotate] [--socket ABSOLUTE_PATH]`
- `a2a courier unregister-station --home ABSOLUTE_PRIVATE_HOME --station S`
- `a2a courier serve --home ABSOLUTE_PRIVATE_HOME [--socket ABSOLUTE_PATH]`
- `a2a courier enroll --dir HOME/.a2a --from - [--rotate]`

The explicit Courier home is owned by the Courier UID at exactly 0700. Registry
and lock files are single-link regular 0600 files. Registration stores exactly
`{identity, uid, secret}` for each station in `stations.json`; the secret is the
original 32 random bytes in canonical padded standard base64. Station and
identity names match `^[a-z0-9._=-]{1,64}$`. Rotation is deliberate, with matching
registration and enrollment, never automatic. Registration's only deliberate
secret output is the closed enrollment JSON on stdout. Keep it in the enrollment
pipe. A failed output can follow committed registration; explicitly rotate to
recover. Unregister removes only the named entry.

Before serving commands, provision each identity's own a2a config base at
`<courier-home>/identities/<identity>`, owned by Courier at 0700. The directory
must already exist and be canonical. Its existing a2a configuration resolves its
one connection: its own `connections.json` current profile and bound credential
metadata, or its own legacy configuration and credential file. Named connection
state uses the existing connection/identity paths beneath this base. No request
selects a profile, credential backend or alternate config base. Missing or
invalid identity configuration returns `courier: identity unavailable`, with no
Matrix request. This component does not provision or migrate those identities.

The socket defaults to `/private/var/run/a2a/courier.sock` on macOS and
`/run/a2a/courier.sock` on Linux. The clean, absolute socket path has no symlink
components and is at most 103 UTF-8 bytes, with no NUL, CR, LF or BOM. Its final
directory is Courier-owned at 0711 with the effective Courier GID; the socket
is 0666. Ancestors must permit intended users to traverse the path. A private
ancestor keeps an override private. Existing wrong modes, ownership, socket
paths and symlinks are refused without repair or unlinking. No OS users or
shared groups are created. Same-UID stations are not a security boundary.

Serve takes a nonblocking `.courier.lock`. Shutdown closes connections and
removes only its own unchanged socket inode. A stale socket needs explicit
operator inspection while serving is stopped. Registry operations likewise
refuse a busy nonblocking lock; simultaneous authentication attempts can refuse.

## Station behavior and authorization

The station CLI discovers `<A2A_CONFIG_DIR>/.a2a` using normal config-base
resolution. Absence preserves local behavior. Any present partial, invalid or
noncanonical enrollment refuses without falling back to local credentials.
See [enrollment](courier-enroll.md) for the exact four-file format and pinned
reader. Local `courier` operator commands retain their explicit local interface.

The closed execution allowlist is `post`, `inbox`, `inbox show`, `read`, `topics`,
`topic new`, `attach`, `files`, `wait`, `watch`, `hook stop|session-start|prompt`,
`bind status`, `who`, `whoami`, and `post-operation status`. New options need
explicit admission. Identity, connection, credential, configuration, keychain,
administrative and Courier operations are refused by the execution engine.

`--as` must match the enrolled identity on the client and the authenticated
registration on the daemon. `--profile` and `--connection-fingerprint` are
assertions against the daemon's independently resolved identity binding. Wrong
assertions return the same fixed refusal as a wrong identity, without reselection
or fallback. Daemon identity, config and session environment values are ignored.
Every invocation constructs its own identity, connection, secret-store selection
and state paths; previous and concurrent requests supply none of these values.
`whoami` uses the registered identity and its own resolved connection.

Body `@file`, topic `--body @file`, post `--envelope @file`, attachment files and
captions are sent as contents. The daemon opens only invocation-owned memory for
these inputs. `inbox show` accepts an explicit streamed `--from @file`; it does
not reopen a retained station path. Output paths (`--out`, `--id-file`), implicit
file-saving modes (`--brief`), voice side effects and other path options are
refused. Save streamed stdout on the station instead. Input file arguments using
an option's `=...` spelling are refused; use separate option and value arguments.
A request, including argv, stdin and base64 file contents, must fit one frame;
large attachments are refused rather than truncated. Ordinary local commands
retain their existing filesystem behavior.

Installed `<exe> --as NAME hook EVENT --runtime R` lines retain their syntax.
Hook stdin is content, and station environment and filesystem paths are not
forwarded. Muse's counts-only diagnostic journal is scoped to the resolved
identity store rather than the daemon's working directory.

## Wire and delivery

Frames use a four-byte unsigned big-endian length followed by UTF-8 JSON, with
an inclusive 65536-byte payload cap. Unknown, duplicate, missing, case-changed
and trailing fields refuse. All refusals are bounded and never echo request
bytes. Daemon diagnostics contain no request content or message bodies.

The C1 hello and registry schema remain unchanged:

1. Courier sends `{"type":"challenge","nonce":"<64 lowercase hex>"}` using a
   fresh 32-byte nonce.
2. The station sends `{"type":"hello","station":"S","identity":"I","mac":"<64 lowercase hex>"}`.
   HMAC-SHA256 uses the original station key over exactly
   `raw_nonce || station || identity`, without separators, lengths or LF.
3. Courier checks the registry binding, kernel peer UID (Linux SO_PEERCRED or
   macOS LOCAL_PEERCRED) and MAC in constant time. Unknown stations use a dummy
   key and the same refusal. A client cannot replace the registered identity.
4. Success is `{"type":"authenticated"}`. The CLI endpoint then accepts one
   `request` with `id` (32 lowercase hex digits), `argv`, `stdin` (base64 bytes),
   and `files` (argv-index keys mapping to exact `{name,data}` objects, with
   base64 content). No request environment or selection state is transmitted.

The original authentication-only `Serve` API retains its no-request behavior;
CLI serving composes the execution handler through `ServeWithHandler`.
Registration rotation affects future hellos; an already authenticated invocation
keeps its identity binding until it ends. Raw concatenation is not globally
self-delimiting: authorization depends on the exact registry lookup and its
independently generated secret, UID and identity, not concatenation alone.

`stdout` and `stderr` frames carry the invocation `id` and base64 `data`; the
client decodes them as opaque bytes without peer-controlled framing additions.
An `exit` frame carries the same `id` and an integer `code`. Known identity
secrets are scrubbed before transport chunking. Command errors have fixed text;
backend errors containing peer bodies or private paths are not daemon logs.

At a delivery-completion point, the daemon sends `delivery` with the invocation
`id`, authenticated `identity` and a fresh unpredictable `batch` token. Only an
`ack` with those exact three values on that authenticated connection releases
the delivery barrier. The station sends it only after local stdout writes and
flushes succeed. Disconnects, timeouts, short writes, failed writes/flushes,
missing ACKs and ACKs for other invocations, batches or identities cannot receipt.
A crash after output but before receipt can print those entries again on retry.
Wait/watch mark shown and hooks ring their doorbell; none of them receipt mail.

Wait and watch stream over the connection. Disconnect cancels the request.
There are at most 64 connections; hello has a five-second total deadline, the
first request a five-second deadline, and execution a sixty-second idle budget.
Output activity renews the execution idle budget. Refusal writes have a
one-second deadline. Registry size remains bounded to 512 KiB and 1024 stations.
Longer streaming hardening, service management, an admin socket, migration and
unrelated delivery repairs remain separate work.

Tests use synthetic registrations, kernel Unix sockets and fake Matrix. They
qualify same-UID serving, explicit UID mismatch checks, delivery barriers,
request isolation and offline command execution. They do not qualify a second
real OS user, live Matrix credentials, ACL isolation, service installation,
whole-machine crash durability or every long-running streaming failure mode.
