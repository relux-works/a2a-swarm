# a2a daemon (a2ad) station commands

The a2a daemon (a2ad) runs existing a2a commands in process for an authenticated station's
registered identity. Identity credentials and delivery state stay in the a2a daemon
home. No shell or second a2a process executes the request.

## Operator setup

- `a2a daemon register-station --home ABSOLUTE_PRIVATE_HOME --station S --identity I --uid U --emit - [--rotate] [--socket ABSOLUTE_PATH]`
- `a2a daemon unregister-station --home ABSOLUTE_PRIVATE_HOME --station S`
- `a2a daemon serve --home ABSOLUTE_PRIVATE_HOME [--socket ABSOLUTE_PATH]`
- `a2a daemon enroll --dir HOME/.a2a --from - [--rotate]`

The explicit a2a daemon home is owned by the a2a daemon UID at exactly 0700. Registry
and lock files are single-link regular 0600 files. Registration stores exactly
`{identity, uid, secret}` for each station in `stations.json`; the secret is the
original 32 random bytes in canonical padded standard base64. Station and
identity names match `^[a-z0-9._=-]{1,64}$`. Rotation is deliberate, with matching
registration and enrollment, never automatic. Registration's only deliberate
secret output is the closed enrollment JSON on stdout. Keep it in the enrollment
pipe. A failed output can follow committed registration; explicitly rotate to
recover. Unregister removes only the named entry.

Before serving commands, provision each identity's own a2a config base at
`<daemon-home>/identities/<identity>`, owned by a2a daemon at 0700. The directory
must already exist and be canonical. Its existing a2a configuration resolves its
one connection: its own `connections.json` current profile and bound credential
metadata, or its own legacy configuration and credential file. Named connection
state uses the existing connection/identity paths beneath this base. No request
selects a profile, credential backend or alternate config base. Missing or
invalid identity configuration returns `daemon: daemon_unavailable`, with no
Matrix request. This component does not provision or migrate those identities.

The socket defaults to `/private/var/run/a2a/daemon.sock` on macOS and
`/run/a2a/daemon.sock` on Linux. The clean, absolute socket path has no symlink
components and is at most 103 UTF-8 bytes, with no NUL, CR, LF or BOM. Its final
directory is a2a daemon-owned at 0711 with the effective a2a daemon GID; the socket
is 0666. Ancestors must permit intended users to traverse the path. A private
ancestor keeps an override private. Existing wrong modes, ownership, socket
paths and symlinks are refused without repair or unlinking. No OS users or
shared groups are created. Same-UID stations are not a security boundary.

Serve takes a nonblocking `.daemon.lock`. Shutdown closes connections and
removes only its own unchanged socket inode. A stale socket needs explicit
operator inspection while serving is stopped. Registry operations likewise
refuse a busy nonblocking lock. Authentication lookups hold shared registry
locks, so simultaneous valid hellos do not exclude each other.

## Station behavior and authorization

The station CLI discovers `<A2A_CONFIG_DIR>/.a2a` using normal config-base
resolution. Absence preserves local behavior. Any present partial, invalid or
noncanonical enrollment refuses without falling back to local credentials.
See [enrollment](daemon-enroll.md) for the exact four-file format and pinned
reader. Local `daemon` operator commands retain their explicit local interface.

The closed execution allowlist is `post`, `inbox`, `inbox show`, `read`, `topics`,
`topic new`, `attach`, `files`, `wait`, `watch`, `hook stop|session-start|prompt`,
`bind status`, `who`, `whoami`, and `post-operation status`. New options need
explicit admission. Identity, connection, credential, configuration, keychain,
administrative and a2a daemon operations are refused by the execution engine.

`--as` must match the enrolled identity on the client and the authenticated
registration on the daemon. `--profile` and `--connection-fingerprint` are
assertions against the daemon's independently resolved identity binding. Wrong
profile/fingerprint assertions return `not_allowed`; a mismatching `--as`
returns `identity_mismatch`, without reselection or fallback. Daemon identity, config and session environment values are ignored.
Every invocation constructs its own identity, connection, secret-store selection
and state paths; previous and concurrent requests supply none of these values.
`whoami` uses the registered identity and its own resolved connection.

Body `@file`, topic `--body @file`, post `--envelope @file`, attachment files and
captions are sent as contents. The daemon opens only invocation-owned memory for
these inputs. `inbox show` accepts an explicit streamed `--from @file`; it does
not reopen a retained station path. Output paths (`--out`, `--id-file`), implicit
file-saving modes (`--brief`), voice side effects and other path options are
refused. Save streamed stdout on the station instead. The `--envelope=@file`, `--body=@file` and `--from=@file` spellings are admitted
alongside separate option and value arguments; `topic new --json` is admitted.
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

1. a2a daemon sends `{"type":"challenge","nonce":"<64 lowercase hex>"}` using a
   fresh 32-byte nonce.
2. The station sends `{"type":"hello","station":"S","identity":"I","mac":"<64 lowercase hex>"}`.
   HMAC-SHA256 uses the original station key over exactly
   `raw_nonce || station || identity`, without separators, lengths or LF.
3. a2a daemon checks the registry binding, kernel peer UID (Linux SO_PEERCRED or
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

`stdout` and `stderr` frames carry the invocation `id`, authenticated `identity`,
a fresh chunk `batch` and base64 `data`. The client decodes opaque output bytes,
writes and flushes them, then sends `output_ack` bound to that invocation, batch
and identity. Only one chunk is outstanding; flow ACKs never complete receipts.
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

Wait and watch stream over the connection without a silent-command idle cutoff.
Invisible keepalive frames arrive every 20 seconds; the station read deadline is
three intervals (60 seconds). Disconnect and daemon shutdown cancel the request.
Station SIGINT returns 130; daemon SIGTERM reports `daemon_unavailable` and does
not complete unacknowledged receipts. At most four invocations per station and
32 globally execute, within the 64-connection cap. Excess execution requests get
`daemon_busy`; there is no invocation queue. Output chunks are at most 1024
bytes and block until their flow ACK, with a 60-second deadline and a framed
`daemon_idle_timeout` on a stalled reader. Hello and first-request deadlines
are five seconds; fixed refusal writes have a one-second deadline. Registry size
remains bounded to 512 KiB and 1024 stations.

Station hooks fail open with empty stdout and exit 0 on a refusal, unreachable
endpoint, saturation or exhausted five-second budget. Their only station-local
write is the bounded, redacted failure log; the next successful hook reports
counts on stderr once. See [streaming, hooks and named bounds](cli.md#daemon-streaming-cancellation-and-station-hooks)
for accounting, all closed diagnostics, resource constants and logging behavior.
No native idle-wake reliability follows from this hook path. Service management,
an admin socket, migration and external reliability qualification remain separate
work.

Tests use synthetic registrations, kernel Unix sockets and fake Matrix. They
qualify same-UID serving, explicit UID mismatch checks, delivery barriers,
request isolation and offline command execution. They do not qualify a second
real OS user, live Matrix credentials, ACL isolation, service installation,
whole-machine crash durability or every long-running streaming failure mode.


## Station notice listener

An enrolled station can run `a2a watch --notices --listen` with an explicit
`A2A_SESSION_ID` for its existing Muse owner lease. a2a daemon retains the mailbox,
State, owner and lease; the station publishes only
`state/daemon-notice-listener.endpoint` alongside the existing hook failure log.
The endpoint is atomically created, mode 0600 under a 0700 directory, and removed
on normal stop. It contains an ephemeral loopback WebSocket address and a secret
capability; neither is logged or sent to a2a daemon. A verified stopped listener's
stale endpoint after SIGKILL requires explicit operator cleanup, never takeover.

Exactly one client may upgrade `/` with `Authorization: Bearer CAPABILITY`.
Wrong credentials or request forms refuse `not_allowed`; a second client refuses
`daemon_busy`. The stream accepts no commands. Each text frame is one closed
L01 JSON projection, never a raw event. See the [CLI notice monitor contract](cli.md#daemon-notice-monitor)
for the file format, HTTP refusals and controller responsibilities.

The engine coalesces each burst for a fixed 30 seconds. Only the exact Matrix
sender matching the identity's pinned owner closes it early; sender-declared
urgency and owner-looking display names do not. Watch never receipts mail;
`inbox` retains the print/ACK receipt barrier. Successful notice transport to the
authenticated listener updates the fenced generation's `last_notice_at`.

C5 liveness and cancellation are reused unchanged: 20-second keepalives,
60-second read deadline, closed `daemon_unavailable` / `daemon_idle_timeout`
reasons, and consumer-lock release within five seconds after disconnect.
Fake Matrix, owned test processes and synthetic WebSocket clients exercise this
path. C6 sweep auditing is independent. **TODO:** pin real installed Muse
version/events/stdin/stdout/handshake/close fixtures before native qualification;
Muse is unavailable here and no native Muse E2E claim is made.
