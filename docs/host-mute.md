# Owner-confirmed scope wake mute leases

`a2a host mute` suppresses **new wake admissions** for an explicit finite selection
of local configuration roots and named connections. It does not freeze processes.
Every result has `version: 1` and `quiesced: false`. Already admitted native/Host
work may complete after a mute ACK. There is no cancel, drain or receiver barrier.

This is a scope capability, not proof of whole-host coverage. Other roots, OS
users, old images, notification routes and consumers outside the admitted launch
configuration are not covered. A maintenance caller must keep this step
`unsupported` until it independently admits the installed image and exact scope.
A source merge, enrollment file or public operation ID alone proves neither
installed adoption nor permission to use another user's roots.

## Explicit interface

All stateful commands require `--scope FILE` and `--budget DUR` (1ms through 5m).
Mute, enrollment and cleanup also require an explicit `--run RUN`. No root,
connection, operation budget, TTL or automatic thaw is selected by default.
The commands use canonical argv without global identity/profile selectors and
ignore ambient identity credentials, station discovery and Matrix configuration.

```text
a2a host mute capability
a2a host mute enroll --scope FILE --run RUN --budget DUR
a2a host mute --scope FILE --run RUN --budget DUR
a2a host mute status --scope FILE --budget DUR
a2a host unmute --scope FILE --run RUN --budget DUR --token OPERATION_ID
```

The scope file contains public routing selections only:

```json
{
  "version": 1,
  "roots": [
    {"root": "/absolute/owned/config-root", "connections": ["", "work"]}
  ]
}
```

`""` explicitly selects the legacy connection; `work` is a profile name in that
root's public `connections.json`. Resolution binds both the profile name and its
immutable connection ID (`connections/ID`), never the current-profile default.
Identity state and metadata roots remain under that original configuration root.
Roots are normalized, sorted and disjoint; duplicate selections refuse.
Enrollment does not create roots, enumerate identities, read secret stores or
expand an existing enrollment. Changing enrollment is a separate deployment
operation, outside mute/undo/status. This feature provides no enrollment-repair
or history-pruning command.

`capability` is an image capability, not an adoption or scope certificate. All
other commands return JSON on stdout (enrollment/mute also display the consent
prompt there). A redirected/piped stdout cannot be used to authorize mute;
subsequent status is independently machine-readable without a TTY. Treat a
nonzero exit as refusal or uncertainty even if some output was captured.

## Authorization and restoration

Enrollment and each mute require the effective OS user to own every selected
root. Roots must be real directories without group/other write permission.
Safe filesystem operations pin their inode identities before showing consent;
the mutation rechecks them after acquiring its locks. Control files must be
single-link regular files owned by that user, mode 0600. Symlinks, unsafe modes,
wrong host/owner, replaced roots, malformed/oversized state and unsupported
versions fail closed.

Both stdin and stdout must be actual TTY descriptors. `TMUX` or `STY`, pipes,
redirection, EOF, decline and mismatched responses refuse. The owner personally
types the displayed challenge. It binds a fresh public operation ID, the actual
OS hostname, effective UID, run, action and exact displayed roots/connections.
The API retains that consent and pinned root identities only in memory before
mutation; constructing or round-tripping the public operation JSON cannot
manufacture consent. There is no approval flag, replacement terminal, supplied
owner string, credential grant or Touch ID implementation. TTY checks and an
unset environment do not cryptographically prove human presence. Every actual
run still needs the affected host owner's authorization.

The durable operation records its host, OS owner, run and full scope. Original
consent covers removal of that exact operation. Cleanup requires the same OS
owner, host, root identities, run and normalized selection. Its public ID alone
grants no authority. Unknown, wrong, stale, already closed and changed-scope
operations refuse before any ledger rewrite. Closed rows remain as replay
fences. An operation-closed result says only that operation was removed; the
per-root rows still report any overlapping mute.

## Enrollment, loss and durability

Explicit enrollment installs `wake-mute.operations.json` before
`wake-mute.enrolled.json`. The enrollment binds the root's device/inode, host,
owner, version and finite connections, and retains a recovery snapshot of the
ledger. Every state update syncs that snapshot before replacing the operational
ledger. Disagreement refuses admission and exposes the conservative union for
exact authorized undo; replacing one copy with valid empty JSON cannot thaw it. An interrupted enrollment is unsupported
or unsafe; status never repairs it. Root enrollment and the mute ledger are
separate from per-identity state, mail and subscriptions.

Admitted CLI consumers must receive `A2A_WAKE_MUTE_REQUIRED_VERSION=1` from their
owner-controlled launch configuration, independently of these mutable files.
Daemon requests inherit this immutable service-start requirement; station
forwarders need their own launch requirement. Embedded Host consumers set
`config.Paths.WakeMuteVersion = 1` (propagated through named `Connection.Paths`).
Unknown required versions refuse. Missing **both** control files then fails
closed, including on restart or a config-root override; it cannot masquerade as
an uninstalled root. A legacy consumer without that admitted requirement can use
an uninstalled namespace, which is explicitly outside this capability's coverage.
An enrolled ancestor cannot be hidden by selecting a nested configuration root.
The OS owner can alter its own files or launch environment; this is not a sandbox
against that owner. Installation/admission of this launch contract is required,
and no installation or live-host qualification is established by fixture tests.

All roots are pinned and exclusively locked in normalized order before a
mutation. A durable `installing` operation in the first root precedes activation
of any later root. Installing and closing rows are effective mutes. Each recovery-snapshot and ledger
replacement writes a private temporary inode, syncs it, renames it atomically
and syncs the directory. Temporary files are removed. Only after all roots have
active rows does mute return success. There is no cross-filesystem atomicity.

A failure can leave zero committed mutes, a partial operation, or a complete
readable operation whose last sync/output outcome was uncertain. It is never
reported as proven rollback. Read-only status exposes exact operation scopes,
root states, generations, creation times, clamped nonnegative ages, partial
coverage and recovery IDs. `operation_coverage` separates covered/not-covered
roots; partial operations return `partial` and a nonzero exit. A readable active
record establishes visibility, not the previous caller's stdout capture or proof
that its final directory sync returned successfully. Status cannot reconstruct a
lost caller's historical ACK. Inspect every root recorded by the operation.
Authorized cleanup can finish an interrupted installation/cleanup and leaves
other operations effective. Corrupt or lost admitted state requires separately
authorized repair; status and wrong IDs cannot bypass it.

The ledger is bounded to 1 MiB and 32 selected roots, 64 connections per root.
Closed operations are retained; reaching capacity refuses new mutation rather
than silently discarding recovery/replay evidence. There is no default expiry.

## Admission and preservation

Lock order is consumer lifetime/generation mutex (where applicable), identity
control, root admission, then identity state/mailbox. Mute/cleanup take only the
sorted root locks and never an identity, transport or consumer lock. Monitor
owner validation releases identity control/state before acquiring admission;
monitor output may then take mailbox state. Host late trigger admission takes
only its root and bounded STREAM transport. No effect holds two root locks.
Even not-yet-enrolled compatible consumers take the directory fence, so first
enrollment/mute cannot race an unchecked local output.

Each emitter holds shared admission through its bounded local effect. Mute
needs exclusive admission and cannot ACK across that effect. Async receiver
work already admitted by the local transport can still finish afterwards.
Native and Host paths check before reservation and again at the actual trigger;
a late refusal retains their uncertainty fences rather than inventing a remote
nonapplication result. Ordinary muted polls do not manufacture new UNKNOWN work.

| Effect boundary | Gate and preservation |
| --- | --- |
| Claude/Codex/Muse hook output | `hookDeliver` gates reservation, stdout and completion after normal ingestion; liveness, Muse diagnostics and owner Stop mirroring keep their existing paths |
| Native `turn/start` | `dispatchNative` rechecks at the actual call, retaining raced attempts; reconciliation remains read-only with respect to remote wake effects |
| Terminal type, Enter, retry, owned-notice recovery | `dispatchTerminal` holds admission through observation, qualification and the actual Send/Resume callbacks; mute cannot split type and Enter |
| Daemon notice stdout and Shown | `emitScopedNotice` gates both; suppressed notices are neither printed nor marked Shown; lease renewal and ingestion continue |
| Station listener websocket forwarding | Every completed buffered frame rechecks the station root before websocket write; daemon and station are separate roots and both must be selected to cover the full chain |
| Host adapter STREAM trigger | `Adapter.Notify` checks before preparing and at the real trigger; UNKNOWN-before-trigger and lookup-only reconciliation remain intact |
| Incident/liveness/owner mirroring | Outbound reporting retains its existing contract; it is not a local wake sink and is not globally returned over |

A daemon frame admitted before the fence may already have a Shown marker at its
upstream boundary. A later station suppression does not erase that evidence or
receipt its mail. There is no remote drain or retrospective cancellation claim.

Mute never writes `HooksPaused`, credential files, principal/subscription/filter
policy, mailbox receipts or existing UNKNOWN records. Independent pauses remain
at their current values. Explicit inbox/read/status stay usable. Unmuting one
operation never restores an old identity snapshot or clears another mute.

Every mute/unmute advances the aggregate root generation and only the selected
connections' qualification generations. An unselected connection keeps its idle
qualification and draft policy. Terminal consumers lazily
invalidate pre-pulse idle/readiness samples even when they never observed the
muted interval. The next cycle obtains fresh observations; old owned drafts and
uncertain attempts are retained under a hold, never automatically submitted or
erased by thaw or restart. An existing genuine matching-turn hook can retire
that old hold under its original authority. Independent manual/lease/runtime
and hourly/spacing gates still apply.

Budgets bound lock polling (10ms backoff), confirmation, transport deadlines and
pollable stdout writes. Context is checked at persistence boundaries; possible
commits followed by a timeout are uncertain. Go cannot preempt a blocked regular
filesystem syscall or an arbitrary embedded writer: those require a local
filesystem and a bounded writer contract. This is not a hard real-time bound
against a stalled kernel/filesystem. No busy loop, privilege escalation,
peer signal, replacement consumer, install or deployment is part of this API.

## Fixture checks

Run focused checks without an installed runtime or a terminal multiplexer:

```text
go test -race -p 1 -count=1 -timeout=5m ./internal/wakemute ./internal/cli ./internal/hostnotification -run '^(TestScopeMute|TestHostScopeMute)'
go build ./cmd/a2a ./internal/wakemute ./internal/hostnotification
go vet ./internal/wakemute/... ./internal/cli ./internal/config ./internal/hostnotification
```

Tests use temporary roots, synthetic PTY descriptors, fake HTTP/native/STREAM
transports and an in-memory terminal driver. The only subprocess witnesses are
copies of the test binary using parent-created fixture roots; they exit on
their own and are waited. A synthetic PTY is not production owner consent.
See the [requirement/coverage map](host-mute-coverage.md). These checks establish
no live host run, installed-image adoption, macOS runtime qualification,
whole-host coverage or confirmed quiescence.
