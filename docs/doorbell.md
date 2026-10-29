# Body-free detector

`a2a doorbell --generation G [--once] [--timeout DUR]` is a deterministic,
non-receipting detector for an **already enrolled own session**. It emits a
fixed summary through stdout. It does not submit native input, create a
session, install hooks, acquire/replace a lease, or establish model receipt.
This source primitive is not a qualified Muse Monitor integration.

Example syntax, after the lifecycle owner has supplied the existing generation:

```sh
a2a doorbell --generation "$A2A_BINDING_GENERATION" --timeout 25m
a2a doorbell --generation "$A2A_BINDING_GENERATION" --once --timeout 25m
```

The example variable is a caller-owned value, not a variable installed by this
command. Duration defaults to 25 minutes and must be greater than zero and at
most 25 minutes. Both modes have a finite lifetime. A lifecycle owner must
supervise exit/rearming through its supported contract; this command installs
no service, timer or model-first Cron.

## Output and outcomes

Both modes call the existing, unchanged hook `doorbellOf` / `doorbellText`
encoder. A notice is its at-most-300-byte fixed summary plus a newline. Only
counts, fixed relevance/kind enums and verified local joined-member identifiers
can appear. Foreign, invalid and unverified senders do not become named sender
authority. Raw `From`, body, header, room label, topic, subject, display text,
event IDs and errors never become notification text. Membership lookup failure
reduces attribution; it does not interpolate an error. No structured peer
metadata is emitted by this version.

`--once` returns exit 0 only after one eligible notice was fully written,
flushed where supported, and its transport reservation completed. With no
eligible notice stdout remains empty. Timeout and cancellation return exit 1
with the fixed stderr classifications `doorbell: timeout` and
`doorbell: canceled`. Other failures also return exit 1 with a fixed command
classification; invalid command syntax returns exit 2. Callers distinguish
non-notice outcomes using these fixed classifications, not exit 0 for an
empty check. Streaming can emit notices before a later nonzero termination.
Partial write/flush failures can leave a partial safe line; consumers must
require a complete newline-terminated frame.

**Never merge stderr into a model notification.** The CLI's global preflight
and diagnostics are not the notice channel. A wrapper that wakes a model on
every background completion, including timeout/error, needs a source-owned
pre-model gate. Empty stdout and a shell exit cannot prove zero empty model
turns. No native harness was qualified by the CLI tests.

The duration bounds context-aware network/lock waits. Like the existing hook
path, a synchronous stdout writer must return: arbitrary blocked `Write` or
`Flush` implementations cannot be preempted by a Go context. A later supervised
adapter must enforce its actual transport/lifecycle bound.

## Existing custody and arbitration

The supplied session environment (`A2A_SESSION_ID`, otherwise
`CODEX_THREAD_ID`) must match the existing live own lease, host, identity and
binding generation. Conflicting native/session values refuse. There is no
PID fallback, expired-lease takeover or force release. The lifecycle owner
must keep the lease live; the detector refuses an expired one.

This base has only native Codex and legacy terminal binding records. Missing
generic/native enrollment for another harness remains a source-owned interface
gap. Reading existing terminal enrollment metadata does not authorize terminal
typing or qualify its native session. No pane capture or terminal input is
performed. Existing consumer and control locks serialize this detector with
other admitted consumers; a second live consumer refuses.

All notices reserve the existing shared `Notification` ledger as path
`doorbell`, without bypass. The existing 30-second completion-anchored spacing,
clock behavior and enrollment's hourly reservations apply; there is no second
throttle or rate ledger. Pause, manual human/blocked/stopping gates, native
binding gates and known active/failed native turns retain work. This detector
does not observe current native busy/approval state: the receiving adapter must
qualify and enforce its pre-model admission gate.

A new event inside the spacing window remains pending and is considered at the
fixed due time, even without another arrival. Each stream coalesces unchanged
pending IDs in memory. Restart rediscovers all pending obligations after spacing;
the in-memory set is not a durable receipt or native accepted-attempt record.

Reservation is durable before output. A proven zero-byte write can cancel only
its own token, retaining its hourly slot. Partial writes, flush/crash ambiguity,
generation replacement and pre-existing uncertain native/terminal attempts
retain their evidence and refuse blind retries, including after cooldown and
restart. Completing stdout transport never calls native `Accept` and records
no native turn or model acceptance. Authoritative same-attempt reconciliation
of an uncertain stdout transport is not exposed by this base; the existing
source controller/host contract must supply it. The detector cannot invent
that proof or clear a retained token to restore liveness.

## Mail and follow policy

Ingestion calls the existing `syncNow`, `ingest` and classifier. Mail is fsynced
before cursor advance through the existing durable mailbox. No raw wait/watch
renderer, `Receive`, ack, inbox, shown update, hook/native rung reset or intake
mutation occurs. Already-shown pending mail is still eligible on discovery.
Existing raw `wait`, `watch` and `inbox` behavior is unchanged.

This base still learns thread participation in its existing classifier.
**ISSUE-118 explicit-follow migration is required before broad adoption.** This
leaf adds no alternative eligibility policy. Current mute/direct/mention,
explicit follow clearing mute, and ordinary exact-principal subscriptions
retain their source behavior. Tests showing an own event alone is quiet do not
prove that a later participation-derived thread reply is excluded.

Unread=0 does not imply no unresolved intake. This detector only reads pending
mail; ISSUE-116's stronger intake journal/recovery interface is absent here.
After normal inbox receipts an event, the detector cannot reconstruct a lost
model context or assert that the obligation was handled.

## Validation

Run the bounded source tests with:

```sh
go test -race ./internal/cli ./internal/config -run '^(TestDoorbell|TestNotificationDoorbell)' -count=1 -v
go build ./cmd/a2a
go vet ./internal/cli ./internal/config
```

They use isolated Matrix/config/process/socket fixtures, not live identities or
provider sessions. Claims are stated beside the tests. Task evidence records
the actual commands, UTC, PID, environment overrides, complete stdout/stderr,
real exits, and an isolated forbidden-body output plant. The producer does not
run a full repository suite; independent review owns that separate gate.

Native model receipt, no-empty-turn behavior, actual busy/approval admission,
generic enrollment, lost-context recovery, watcher supervision and host adoption
remain separate qualification rows. No production install or migration follows
from passing these source tests.
