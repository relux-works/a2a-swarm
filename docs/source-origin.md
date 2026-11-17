# P17-A Source run-origin seam (review candidate)

This implements the upstream part of Source proposal r2
(SHA-256 `3687f08991c035232b1348b4c40715f3e650ca1e62b97df6790cbb2525cc3a89`)
and Curator hooks r1.2
(`15e7e393b4254f9f6ef2ef211667d203204d095e8b83f71e696dc03673b4f021`).
This source library does not activate a service.

Generation spelling is a **breaking change of the public `runorigin` package**:
`Principal.Generation` is now `string`, and `GenerationAuthority.CurrentGeneration`
returns `(string, error)`, replacing `uint64`. Callers must migrate both their
principal values and authority implementations to the user manager's generation
journal spelling: exactly 32 lowercase hexadecimal characters, not all zero.
The value is opaque and compared bytewise, never converted, truncated, padded
or case-folded. UID and generation come only from the user manager; no local
counter or station/runtime generation can substitute for journal authority.

## Build boundary

`runorigin.Producer` is an optional, explicit typed dependency. It is unrelated
to the generic `admission.Checker`/`Decision` registry. No producer is linked or
registered in stock upstream. `daemon.NewOriginService` and `Serve` are Linux
library entry points; no existing daemon/CLI path calls them. A stock library
endpoint constructed without a producer advertises no operations and returns
`origin_not_supported` on a well-formed authenticated prepare.

A future reviewed downstream service must construct ONE `OriginService` at
process start from protected `OriginConfig`: SourceControlDir, pinned launcher
UID/generation, current `GenerationAuthority`, and its qualified Producer. It
must also establish the Source A account/image/generation/service binding for
the launcher. This candidate does not infer those from a self-reported hash,
station secret, checker name, runtime token or Matrix author.

`Producer.Open` receives only authenticated launcher, retained-event reference
and input principal. The downstream implementation must independently resolve
identity, connection, Matrix user, policy and Keymaker registration from its
protected mapping, read the retained event itself, and construct an OWNED
checker with its own private scope. It must verify the exact signed inner
request/profile, grant and current tip. A missing mapping/fact refuses. It
returns a new `Session` for that preparation; it never borrows another
invocation's session token or checker. The upstream server calls `Prepare`, then
`Validate` for fresh admission/pin checks, then `Close`. Close also runs on
replacement, disconnect, malformed traffic, refusal, lost reply or timeout.
The downstream must honor context cancellation and implement prompt local Close.

A `Result` contains the protected registration plus a typed six-member Origin.
Upstream checks shape and exact immutable equality on revalidation. It cannot
prove cryptographic truth supplied by a malicious linked producer. In
particular, a syntactically valid forged signed-inner-request claim is a
**downstream producer qualification obligation**. Upstream tests reject a
caller-injected origin, malformed producer origin, Matrix `$` ID and changed
recheck facts; they do not implement or simulate a rich cryptographic checker.

## Pinned wire contract

The envelope, hello, errors, scalar encodings and validation correlation below
are pinned for the launcher (`curator run`) and downstream service clients.

Framing is the existing 4-byte big-endian length, `1..65536` UTF-8 JSON bytes.
Only framing is shared with station traffic; this protocol has its own closed
JSON decoder. No station hello, HMAC, argv, identity, secret or session parser
is used. The socket is exclusively `<SourceControlDir>/origin.sock`.

After kernel/current-generation authentication, Source sends exactly:

```json
{"capabilities":["source.origin.prepare/1","source.origin.validate/1"],"source_incarnation":"0123456789abcdef0123456789abcdef"}
```

With no producer, `capabilities` is `[]`. These are announcements, not peer
credentials. Unauthorized peers instead receive a fixed code/incarnation frame
and the connection closes, before any event read or checker/prepared work.

Each request envelope contains exactly `op` and `params`:

```json
{"op":"source.origin.prepare/1","params":{"request_id":"11111111111111111111111111111111","launch_nonce":"22222222222222222222222222222222","event_ref":{"room_id":"!room:example","event_id":"$matrix-event"},"input_principal":{"uid":4242,"generation":"fedcba9876543210fedcba9876543210"}}}
```

```json
{"op":"source.origin.validate/1","params":{"request_id":"11111111111111111111111111111111","launch_nonce":"22222222222222222222222222222222","prepared_id":"0123456789abcdef0123456789abcdef","source_incarnation":"fedcba9876543210fedcba9876543210"}}
```

Both successful operations return exactly `runorigin.PreparedOrigin`'s seven
fields. Validate repeats the original request ID and every preparation binding:

```json
{"request_id":"11111111111111111111111111111111","launch_nonce":"22222222222222222222222222222222","input_principal":{"uid":4242,"generation":"fedcba9876543210fedcba9876543210"},"identity_registration_number":"protected-registration","source_incarnation":"fedcba9876543210fedcba9876543210","prepared_id":"0123456789abcdef0123456789abcdef","origin":{"kind":"admission","event_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","grant_id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","tip_hash":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","mode":"enforcing","command_id":""}}
```

Scalar rules: UID is a JSON uint32; generation is only a JSON string satisfying
`runorigin.ValidGeneration`: exactly 32 characters of `0123456789abcdef`, except
32 zeros. JSON numbers of any value, null, empty, uppercase/mixed-case, non-hex,
and 31/33-character strings refuse with `run_origin_refused` before any
per-request authority read or producer call (hello authentication precedes the
request). The configured launcher generation and every current authority value
must pass the same validator. A one-character difference refuses; no
normalization or numeric compatibility path exists.
Input UID zero is refused. The configured launcher UID must be nonzero and
must differ from the service's effective UID. Request ID and launch nonce are
exactly 32 lowercase hex characters; the launcher draws each launch nonce from
a CSPRNG. Event references are nonempty strings of at most 4096 UTF-8 bytes
with no NUL/CR/LF. Registration is an opaque producer string of 1..4096 bytes
without NUL/CR/LF; its canonical domain belongs to the registration authority.
Incarnation and prepared ID are independently random nonzero 128-bit values
encoded as 32 lowercase hex characters. The three Origin IDs are exactly 64
lowercase hex characters. The event reference may contain a Matrix ID;
Origin.event_id must describe the verified inner request and cannot contain one.

Every object is closed, including the envelope and both nested prepare objects.
Missing, unknown, duplicate (including escaped aliases), case-variant and
wrong-type fields refuse. There are no additional operations. Failures contain
exactly `code` and current `source_incarnation`; they close the connection:

```json
{"code":"run_origin_refused","source_incarnation":"fedcba9876543210fedcba9876543210"}
```

Only names from the accepted texts are emitted: `origin_not_supported` is a
definite refusal; malformed/unauthorized/path/origin/correlation failures use
`run_origin_refused`; explicitly classified dependency failures and timeouts use
`run_origin_unavailable`. Arbitrary producer error text is suppressed and
unclassified errors refuse. There are no invented path/parse/nonce/timeout
subcodes. Bind/end UNKNOWN belongs to the launcher/Keymaker, not this endpoint.

## Authentication, records and budgets

The server checks real Linux SO_PEERCRED against the protected launcher UID,
then consults injected CURRENT generation authority, validates its journal
spelling and compares it bytewise with the protected launcher generation. It
checks again for every prepare/validate before calling the producer. A denied UID does not even reach the generation dependency.
Input/caller fields cannot authenticate a connection.

SourceControlDir must already exist, be A-owned exactly 0711, and have no
symlink components. Its ancestors must be operator-qualified protected paths;
this library checks no-follow traversal, not host ACL/group membership policy.
The newly created socket is A-owned exactly 0666, a single-link socket. Existing
paths, including stale sockets, are refused without repair/removal. Only a new
socket is chmodded; cleanup compares the held directory and created inode.
The complete ancestor chain is reopened without following links and compared
by device/inode on every endpoint check. Directory/socket identity, owner,
type, links and modes are rechecked after current-generation lookup and
immediately before protected operations. Path changes refuse. No account/group/ACL is created.

A fresh incarnation is generated with crypto/rand at service construction and
is immutable. Serve is single-use. Records are connection-local memory only,
with at most one outstanding preparation per connection. A fresh prepare
replaces/closes the old session. The record binds launcher (immutable
configuration plus current peer checks), Source incarnation, request ID, nonce,
input principal, mapping/session and immutable Origin. Validate on that same
live connection consumes it before recheck; a failure cannot reuse it. Another
connection or service instance has no authority to adopt the ID. Disconnect,
idle expiry or service shutdown closes an unconsumed session. Nothing restores
records after process restart.

The idle gap after prepare is at most one work/read budget (about 4.9 seconds).
The client must send validate promptly on the same connection. After idle
timeout, the connection closes and the preparation is destroyed: restart H1
with a fresh preparation, never reuse its old prepared ID or response.

Each prepare and validate has an independent five-second budget, including its
frame read, dependency work and response write. The final 100 ms is reserved
inside that budget for error delivery; no terminal write extends it. Late results are discarded and late sessions
closed. Worker slots remain occupied until the dependency actually returns,
bounding even a broken cancellation-ignoring dependency to 64 outstanding
workers. There are at most 64 live client connections, no unbounded prepared
map. Error writes are capped at one second and at the current call deadline,
whichever is sooner. A disconnected/timeout operation
never becomes an accepted preparation later. Network delivery can still be
lost: the client must restart from H1, compare every successful binding to its
current authenticated hello and never reuse an old prepared ID/response.

Source incarnation fences H1–H2 only. There is no Source-driven ending or
revalidation after Keymaker bind. H3–H6, durable launcher records, isolation,
FD/input-release gates and all Keymaker operations are outside P17-A. The launcher (`curator run`) never rebinds in this version, and hosted session
managers refuse enforcing. This library adds no alternative route.

## Tools and verification

Use the task's admitted Go environment before every Go/gofmt invocation. No
telemetry command or persistent Go environment change is needed. Ordinary
entry points (logs under `.temp/P17-A/`, fixtures in test temp dirs):

```sh
go test ./internal/daemon -run '^Test(SourceOrigin|Rev365)' -race -count=1 -v
go test ./runorigin ./internal/daemon -run '^Test(ValidGeneration|SourceOriginGeneration(Wire|Authority|Configuration))$' -count=1 -v
A2A_RUN_ORIGIN_PLANTS=1 go test ./internal/daemon -run '^TestSourceOrigin(Narrowing|Generation)Plants$' -count=1 -v
go test ./... -race
go vet ./...
go build ./...
```

The opt-in plant runner uses Go overlays, checks exact named test failures and
exit 1 (rejecting compile errors/panics as evidence), then reruns unchanged
source at exit 0. `A2A_ORIGIN_GO_ENV=/absolute/env.sh` optionally requires the
subprocesses to source the same admitted environment explicitly. Generation
plants cover case folding, all-zero admission, numeric JSON admission, an
off-by-one length check and omitted authority spelling validation, with passing
controls before and after each overlay. The authority-validation witness alone
bypasses the private fixture's constructor before serving, so an invalid
launcher pin cannot mask the current-authority validator through inequality;
production configuration remains immutable and validated at construction.
Generation refusal tests assert exact closed error frames, no producer/session
calls and unchanged control-directory contents/modes. They do not claim to
observe arbitrary writes outside the fixture directory.

For confined validation, use a write-confined sandbox with a private temporary
directory and PID namespace; keep compiler and test scratch in the authorized
writable root. Configure the toolchain environment outside public repository
content. Record exact commands, versions, timeouts and real exit codes in the
private task evidence. Compare any whole-suite failure with the unchanged base
under the same invocation before attributing it to code or the environment.

Wire fixtures use real kernel peer credentials with an injected construction-
time service identity. Public-constructor tests exercise the actual service
EUID rejection policy. These unit fixtures, synthetic wrong-owner metadata and
fake generation authority do not qualify real distinct-UID deployment, protected
pins, FD theft exclusion, signed inner-request verification, final response
release, client replay comparison or production startup.
