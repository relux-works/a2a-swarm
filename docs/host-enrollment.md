# Host enrollment boundary

The opt-in Go configuration API binds a manager-supplied attachment to an
independently observed caller scope. `HostScope` retains the exact agent,
member, connection and configuration references, source session, actual native
thread, consumer generation and consumer reference. Source sessions allow up
to 200 UTF-8 bytes; native threads use the Host ASCII identifier subset up to
128 bytes. Generation and connection references are bounded to 128 bytes.
References are compared byte for byte, with no trimming or truncation.

The manager must supply `HostAttachment` only from its admitted launch receipt.
The source caller supplies its actual provider/tool observation separately.
Reading an environment variable, decoding the structure, knowing an attachment
reference or acquiring a file lock does not establish that authority. The API
cannot authenticate arbitrary same-UID callers: it assumes cooperating binaries
and admitted launch custody. OS-user isolation and exclusion of older binaries
that bypass the supported writer are outside this boundary.

`State.EnrollHost` requires the existing source lease to equal the admitted
attachment lease and remain unexpired. Existing active native or terminal
bindings refuse enrollment. Enrollment neither acquires nor force-releases a
lease. A repeated identical enrollment is idempotent; changing the attachment,
consumer, generation or lease does not implicitly renew or replace custody,
even after expiry. Lease timestamps compare by instant; newly captured and
decoded attachment timestamps use UTC. Location and monotonic-clock metadata
do not change lease identity. Explicit handover/retirement needs a separate
reconciliation contract. A refusal preserves pending mail, receipts and reservations.

`WithHostEnrollment` holds the existing consumer lock throughout the adapter
callback and serializes enrollment under control and state locks. It persists
the record before the callback starts. The lock deadline is explicit and finite;
a duplicate consumer cannot run the callback. Its supplied clock is sampled
under the state lock and again after persistence, so elapsed lock or write time
cannot reuse an earlier admission timestamp. Expiry after persistence retains
the fence without starting the adapter. The caller owns the actual
process identity observation and descriptor lifecycle. This entry point does
not launch a process, install hooks or activate a notification backend.

Supported state readers/writers reject unknown enrollment versions, unknown
members and an explicit null enrollment. An unrelated state update preserves
enrollment; dropping or replacing existing custody through `SaveState` refuses.
These guards apply only to binaries containing this API. They cannot prevent an
older executable or direct same-user file edit from dropping the field. State
updates use the existing state lock; direct `SaveState` callers must already
own the appropriate serialization.

Persisted enrollment selects Host custody. Hook entry points suppress session
export, auto-binding, healing and legacy fallback before reading hook input.
Wait/watch and terminal/native bind refuse persisted enrollment before lease
acquisition or provider/process probes, including after expiry. Bind rechecks
under the control lock before creating a consumer. A same-session member read
such as inbox may borrow its exact, still-valid Host lease without changing
its holder, host, PID or expiry. A foreign session, changed lease or expired
lease refuses without taking over or extending custody. Ordinary non-Host
lease refresh remains available.
Ordinary `identity release` also refuses persisted Host enrollment, including
the attached session, a foreign session, `--force` and an expired lease.
It preserves the lease and enrollment; release is not a Host retirement path.
The shared notification reservation boundary refuses hook, native and terminal
output while enrolled, even after attachment expiry. Expiry does not select a
different backend. A malformed enrollment fails closed. Without enrollment,
ordinary SessionStart export and pending-mail notifications remain available.

No new CLI flag or live delivery route is exposed. Version2 of the strict
enrollment envelope adds an opaque Source receipt reference and a bounded
notification-attempt ledger. `WithHostNotificationEnrollment` upgrades the
identical existing custody under the original locks and passes the receipt to
its callback only after persistence and a fresh expiry check. The manager's
attachment reference remains distinct; neither reference supplies authority.
An already shipped version1 reader refuses version2 rather than discarding it.

`PrepareHostNotification` reserves the shared doorbell and records its exact
token/session/generation/notice digest and complete admitted Host binding in
the same State. `BeginHostNotification` records UNKNOWN and the original
trigger request ID. Each resulting State must be saved successfully before
the next step or transport I/O. Prepared/UNKNOWN blocks replacement even
after cooldown; a stale writer, legacy cancellation or legacy reservation
cannot discard that fence through the supported writer.

Only an exact retained Host observation can accept the attempt and complete
its shared reservation. Native stages advance monotonically for the same
Host sequence and prove only that stage. Pre-I/O abandonment or a conclusive
refusal of the original trigger can cancel; lookup absence, timeout, EOF and
a different call's refusal cannot. Expiry closes new admission while exact
retained observations may reconcile the old obligation. No implicit binding
replacement, eviction or time-based expiry exists. At most32 records and65536
compact encoded ledger bytes are retained; byte capacity may refuse earlier.
Version2 State uses compact JSON so indentation cannot make a valid ledger
unreadable. Transport recovery and integrated counterpart validation remain
required before any Host effect can be enabled.

Host-enrolled state writes use one opened configuration/state-directory pair,
a private exclusive temporary, file sync, atomic directory-relative replacement
and state-directory sync. The configuration directory is synced before the
file write to retain its state-directory entry. The admitted configuration
namespace must already exist. The state directory must be private (0700), and
an existing state file must be regular and 0600; symlink entries refuse.
The maximum encoded source State is 16 MiB, separate from the Host journal and
wire limits. Legacy writes without enrollment keep their existing behavior.

An error after replacement is explicitly uncertain: new bytes may exist, but
the caller must not start transport or infer nonapplication. Returned failures
remove only the writer's own uncommitted temporary. An abrupt process exit may
leave that temporary; it is not a receipt or retry permission. These guarantees
assume the admitted namespace and existing serialization, with a filesystem that
honors successful file/directory sync and atomic replacement. Isolated process
crash tests do not establish universal power-loss durability or protection from
hostile same-UID namespace replacement.

The low-level `internal/hostnotification` client implements the closed
`curator-host-notification/1` exchange on one manager-supplied Unix STREAM
endpoint. `New` checks the actual socket type, admitted binding shape and an
explicit1..60000ms full-call deadline. This does not authenticate the manager
or enroll a caller. Invalid framing, correlation, timeout, cancellation or
connection loss closes the endpoint as unresolved; there is no retry, path
dial, alternate backend or payload diagnostic. Accepted records prove only
their stated Host custody/native stage. A refusal concerns only that call.

`NewAdapter` combines this client with the original Source enrollment. Construct
and use it only inside the same `WithHostNotificationEnrollment` callback, with
the manager-supplied endpoint/binding and actual scope. The outer callback owns
the sole-consumer lock throughout; constructor checks do not authenticate an
arbitrary caller, acquire custody or admit reattachment. A new constructor needs
live admission. Each retained entry constrains the entire binding thereafter.

`Notify` accepts the trusted fixed Source notice (1..65536 valid UTF-8 bytes),
context, finite lock deadline and existing migration timestamp. It hashes the
exact notice locally. Under control/state serialization it first durably saves
the shared reservation and prepared correlation, then durably saves UNKNOWN
with a distinct original trigger request ID. Only after both writes succeed
and admission remains live does it make one trigger call. State callbacks never
perform transport I/O. Only the original exact conclusive trigger refusal may
cancel that reservation. Accepted observations durably complete its shared
spacing at the evidenced Host/native stage, without receipting pending mail.

`Reconcile` checks the exact retained own key before making only `lookup_attempt`
with a fresh transport ID. It may resolve the original obligation after expiry;
missing/refused/unresolved observations do not establish nonapplication, cancel
UNKNOWN or resend. A later observation for an older accepted key cannot finish
a different current reservation. `AbandonPrepared` explicitly cancels only a
prepared pre-I/O record. UNKNOWN cannot be abandoned or expire automatically.
No operation evicts terminal correlations or replaces the original enrollment.

Public errors expose fixed categories, including persistence uncertainty, not
filesystem paths or payload diagnostics. A returned key on error is correlation
only; a returned Host response with a state error is not a persisted Source
receipt. Preserve the obligation for explicit observation/recovery. Lost or
malformed replies close the original stream; no automatic reattachment exists.
The low-level client still does not perform this orchestration by itself. No
CLI backend or live activation is provided; integrated counterpart checks and
the admitted enrollment/handover lifecycle remain required.

Run the isolated configuration and neighboring notification checks with:

```sh
go test ./internal/config -count=1
go test -race ./internal/config -count=1
go vet ./internal/config
go test -race ./internal/hostnotification -count=1
go test ./internal/cli -run 'TestHostEnrollment|TestNotification|TestHookSessionStart|TestHookDoorbell' -count=1 -timeout=3m
```

Tests cover valid/idempotent admission, mismatched scope, foreign/missing/expired
lease, unchanged member-read custody, non-UTC timestamp round trips, alternate
consumer refusal after expiry, implicit successor refusal, strict bounds,
compatible and lossy state
writes, unknown versions/fields/null, duplicate-consumer exclusion, legacy-output
suppression and a neighboring valid legacy notification/session export. They use
isolated fixture directories and do not qualify an installed native runtime.

The optional cross-module test consumes an explicitly supplied inert Host test
executable and its expected SHA-256. Verify its exact
source/test/artifact handoff before running; there is no discovery, product
attachment or installed runtime behind these test-only inputs:

```sh
A2A_TEST_HOST_HELPER=/absolute/path/to/host-fixture.test \
A2A_TEST_HOST_HELPER_SHA256=<expected-lowercase-sha256> \
go test ./internal/hostnotification -run '^TestHostCounterpartDock$' -count=1 -v -timeout=1m
```

With neither input this check is skipped and docking remains unqualified. A
partial input pair, invalid executable or mismatched digest fails the check.
The test owns finite subprocess/socket cleanup and uses the actual Source
enrollment and transaction with the counterpart's actual private engine. The
initial cases cover accepted/same-endpoint lookup and original input-not-ready
refusal. They do not qualify cross-module lost-reply recovery, reattachment,
native scheduling, live activation or idle delivery. Parser/artifact guard tests
run normally; their success alone does not establish counterpart conformance.
