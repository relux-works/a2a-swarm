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

No new CLI flag or live delivery route is exposed by this change. Before Host
effects can be enabled, durable
same-transaction reservation/attempt correlation, uncertain transport recovery,
and integrated counterpart validation are required.
The current file-sync/rename state writer alone does not claim directory-sync
or power-loss durability.

Run the isolated configuration and neighboring notification checks with:

```sh
go test ./internal/config -count=1
go test -race ./internal/config -count=1
go vet ./internal/config
go test ./internal/cli -run 'TestHostEnrollment|TestNotification|TestHookSessionStart|TestHookDoorbell' -count=1 -timeout=3m
```

Tests cover valid/idempotent admission, mismatched scope, foreign/missing/expired
lease, unchanged member-read custody, non-UTC timestamp round trips, alternate
consumer refusal after expiry, implicit successor refusal, strict bounds,
compatible and lossy state
writes, unknown versions/fields/null, duplicate-consumer exclusion, legacy-output
suppression and a neighboring valid legacy notification/session export. They use
isolated fixture directories and do not qualify an installed native runtime.
