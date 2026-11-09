# Admission interface

`internal/admission` is a build-time seam for downstream policy implementations.
Upstream imports no chain, signature, name registry or grant implementation and
links only the stateless `none` checker. This is independent of Courier station
enrollment and monitor admission.

The optional credential metadata field `admission_policy` selects a registered
name for that identity; absence or `"none"` selects `none`. It is host-written
metadata in the existing agent/human credential file, including named connection
metadata. There is no policy selector or state directory in requests, flags or
environment variables. Existing host selection resolves the identity base first.
Unknown names refuse session startup with `admission: policy_unknown`.

A checker is constructed when an authenticated or metadata session opens, and
closed at invocation completion, including error exits. A long-lived consumer
keeps its checker for its session. Courier constructs these sessions inside its
daemon's per-invocation engine, beneath the authenticated identity home; stations
neither construct checkers nor access their state. Each session receives:

```go
type Config struct {
    Identity string
    StateDir string
    Nostr    bool
}
```

`StateDir` is `<identity-base>/admission/<identity>/`, created with mode 0700.
The base is the selected local credential namespace, or its counterpart inside
Courier's identity home. It is derived by the engine, never supplied separately
by a caller. Files are mode 0600; links and permissive files/directories are
refused. Every current Matrix identity has `Nostr: false`.

## Checker contract

```go
type Verdict uint8
const (
    Live Verdict = iota
    Exchange
    Refused
)
type Decision struct {
    Verdict Verdict
    Retryable bool
    Reason string
}
type Inbound struct {
    RoomID, EventID, Sender, Kind string
    Timestamp int64
    Body []byte
}
type Outbound struct {
    Identity, RoomID, TransactionID, Kind string
    Request []byte
}
type Checker interface {
    Inbound(context.Context, Inbound) (Decision, error)
    Outbound(context.Context, Outbound) (Decision, error)
    Close() error
}
type Factory func(Config) (Checker, error)
```

`Inbound` describes an authenticated Matrix sync delivery candidate. `Kind` is
the Matrix event type and `Body` is the event's content JSON bytes, not rendered
text. Local invite/gap delivery records use their mailbox kind and empty body.
Timestamps are Matrix milliseconds since the epoch (zero if unavailable).
`Outbound.Request` is the complete prepared Matrix message content JSON;
`Kind` is `m.room.message`. Policies receive owned byte copies: mutating them
cannot change the mailbox, spool or transmitted request. These byte fields are
private inputs and never diagnostic/status fields.

| Verdict | Inbound | Outbound |
| --- | --- | --- |
| `Live` | Append to the ordinary mailbox | Send this attempt |
| `Exchange` | Append to the exchange spool; no mailbox entry | Refuse with `exchange_outbound` |
| `Refused`, definite | Drop and increment its reason count | No send; fail with its closed reason |
| `Refused`, retryable | Hold outside the mailbox | No send; fail with its closed reason |

`Retryable` is valid only on `Refused`. Unknown verdicts, unknown reasons or
retryability on any other verdict normalize to definite `policy_invalid`.
A policy error normalizes to definite `policy_error`; its error text is never
wrapped, logged or returned. Startup errors normalize to `policy_startup`.

The fixed reason table is:

| Code | Meaning |
| --- | --- |
| `allowed` | Policy permits ordinary delivery |
| `exchange` | Policy routes to exchange |
| `denied` | Definite policy refusal |
| `dependency_unavailable` | Policy dependency unavailable (may be retryable) |
| `policy_invalid` | Invalid decision |
| `policy_error` | Checker returned an error |
| `exchange_outbound` | Outbound exchange unsupported |
| `held_age` | Held entry reached 24 hours |
| `held_count` | New held entry exceeds 256 entries |
| `exchange_capacity` | New spool line exceeds the 16 MiB file cap |
| `storage_error` | Outbound refusal count could not be persisted; send remains forbidden |

The table is shared by validation, persistence and diagnostics. Adding reasons
requires an explicit contract change; free text is never accepted as a reason.

## Held queue and counts

`held.json` is an atomic private snapshot containing held delivery payloads,
their first-hold local times and per-reason counts. It is separate from State
and mailbox files. The queue holds at most 256 entries, for less than 24 hours
from first hold, independently of peer event timestamps. An entry at the age
limit is dropped and counted before another checker call. If the count limit is
reached, the incoming entry is dropped; older retained entries are preserved.
The serialized snapshot has a 32 MiB defensive read/write limit. Storage or
serialization failures stop cursor advancement with a closed diagnostic.

Every persisted sync batch rechecks held entries first, including an empty
batch. `Live` entries are durably appended before removal from held state.
A crash can replay an append, but mailbox event-ID dedup suppresses it even if
the original entry has since been receipted (within the mailbox's existing
receipt retention window). Retryable entries retain their first-hold time.
Definite refusals, expiry, count overflow and exchange overflow increment
persisted counters. Counters count refusal/drop actions, not unread messages.
Repeated cursor attempts can count another definite refusal.

Queue/spool/counter mutations share a private cross-process `queue.lock`.
Sync also holds the existing identity state/mailbox lock. Outbound counters
need only the admission lock, so sends already holding the state lock do not
reenter it. Checker methods are serialized within a session. Locks and state
support the engine's existing macOS/Linux targets; other targets fail closed.

`a2a status admission --json` returns only `{"counts":{"denied":3}}` (an empty
map if nothing was refused), scoped to the selected identity. No bodies,
senders, room names, held records or spool contents are projected. Courier
allows this read-only query through its normal command boundary.

Inbox, wait, watch, hook notices and unread counts read only the mailbox;
neither held nor exchange can ring a doorbell. No new consumer reads either
private payload store. The `none` hot path constructs no new `Inbound` values
and calls the original mailbox append directly, preserving mailbox and State
bytes. It creates no queue/spool/counter files during sync or post for a fresh `none`
identity. If a host switches an identity to `none`, existing held inputs are
rechecked first; new batch entries still bypass Inbound construction.

## Exchange spool v1

`exchange.jsonl` is append-only, mode 0600, capped at **16 MiB including newlines**.
Each UTF-8 JSON line contains exactly these writer fields:

```json
{"version":1,"event_id":"$event","room":"!room:server","sender":"@sender:server","timestamp":42,"kind":"m.room.message","body":"e30=","reason":"exchange"}
```

`body` is standard padded base64 of the content bytes (`e30=` is `{}`). Empty
bodies are encoded as an empty base64 string. `reason` is the normalized closed
code returned with `Exchange`. The writer only appends and stats this file; it never reads or consumes payloads.
It fsyncs accepted lines. Sync replay after a crash can append a repeated
room/event pair; an exchange consumer must use that pair as its dedup key.
If a new line would
exceed the cap it is refused and `exchange_capacity` increases. Nothing is
truncated, rotated or pruned automatically. Oversized or non-private retained
files cause a closed storage error, rather than discarding existing data.
The separate exchange service and its consumption protocol are out of scope.

## Outbound and journal

The identity session installs a guard at `matrix.SendMessageTxnReceipt`, shared
by all message send helpers and preserved by scoped client copies. Each call,
including a retry with the same transaction ID, checks the newly prepared bytes;
`Live` is never cached. The default `none` installs no guard.

Durable post first prepares its journal and reserves UNKNOWN before reaching
this guard. A refusal records `outcome: "refused"` and `refusal_reason`, removes
the retained request, then fails the command without HTTP send. A failure to
persist this closure is reported as `refusal_persist_failed`, never success.
A retained refused or UNKNOWN operation does not gain permission to resend;
existing journal replay rules are preserved. Ordinary posts and service sends
have no durable post-operation record but share the same pre-send gate.

This seam gates message sends, including the existing admin identity's token
request. Matrix state/account-data changes, membership, media upload and reads
are outside the prepared message-send contract; attachment/voice event sends
are gated after media preparation. This is not a media-upload authorization seam.

## Fork registration

A downstream build imports its checker package; that package calls from `init`:

```go
func init() {
    admission.Register("private-policy", newChecker)
    // Or, for policies that require Nostr identity capabilities:
    // admission.RegisterNostrOnly("private-policy", newChecker)
}
```

`Lookup(name) (Factory, bool)` returns the factory. A Nostr-only registration
wraps the factory with a capability check: Matrix startup refuses before the
implementation is constructed. Names do not imply capabilities. Registration
panics for empty/duplicate names, nil factories or calls outside an init stack.
The pre-registered `none` cannot be replaced. Test-only registration lives under
`internal/admission/testdata/policy` and is imported only by test sources.
