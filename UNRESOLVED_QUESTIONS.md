# Unresolved questions

## Issue #258: cross-topic citations after #219

The current #219 reply contract in `docs/cli.md` and
`TestPostReplyTopicRoomAndConflict` requires explicit topic/reply root agreement.
Exact case: `a2a post ROOM --topic mine --kind answer --re '$child-of-other' TEXT`
refuses when `mine` resolves to a different root from the child's validated
`m.thread.event_id`, even if both roots are readable. Earlier binaries allowed
this form and real lanes use it for citations. This slice retains that refusal
and explains how to cite instead: omit `--re`, include the event ID or link in
the body. Owner question: should cross-topic citation gain a separate explicit
mechanism, or should the #219 agreement contract change? No protocol decision
is made here. The original issue pages were unavailable to this producer;
the finding is based on the checked-in contract and executable conflict cases.

## Issue #89: exact trigger in the reported live lanes

Private deterministic fixtures reproduce the post-type refusal when a Claude
registry status or statusUpdatedAt changes after typing, while the normalized
composer start stays unchanged. Stable named Claude 2.1.309/2.1.310 and Codex
fixtures submit successfully. The field report did not preserve the before/after
keys, so the precise live trigger, including the Codex lane's installed-image
behavior, remains unverified.

Any future field investigation belongs to a separately authorized host-keeper
window. This fix uses the issue's loud-surfacing alternative: pending typed
attempts become SUSPECTED-DEAF and report once to an explicitly configured own project/lane room after
ten minutes. Recovery remains manual; existing nonempty-input fences are kept.

## Issue #89: literal exact-editor-content recovery

Automatic recovery Enter remains held. A private real-tmux countermodel renders
an exact notice and the same notice with one/two appended ASCII spaces as
identical styled viewport cells, with identical cursor and geometry. The
candidate continuation also admitted identical PR83 comparable readiness keys;
it submitted the non-exact drafts on Codex, legacy Claude and named Claude.
Visible suffix and true-end cursor controls remained distinguishable. This is
source/interface evidence, not a recording of an installed provider's renderer.

The current terminal interface exposes rendered cells and cursor position, not
authenticated editor contents or an edit revision. More captures, strict key
rechecks and a durable Enter fence do not recover information absent from those
observations. Rejecting ambiguous frames also rejects their exact controls.
No parser/readiness relaxation or invented provenance can satisfy the literal
requirement. Existing fresh Send and its documented non-atomic limit are unchanged.

The product decision retains loud/manual recovery and limits this change to
incident-report continuity and session/episode boundaries. A trusted editor or
native operation that verifies exact content and its revision before submitting
is a separate, non-blocking follow-up. A rendered-only heuristic would weaken
the safety contract and is not implemented.

## Issue #89: stale carry snapshot and route availability

The replacement now carries the current on-disk incident report inside the same
locked update that installs the new binding; the later consumer-startup update
changes only PID/birth metadata. This protects pin/ACK writes made after bind's
initial state load. Executed owned-consumer replacement coverage remains absent.
The required deterministic fixture pauses after that initial load, lets only its
owned consumer persist the pin and a server-applied report, stops and reaps that
consumer, then releases replacement. It must retain the exact pin and ACK with
one applied event; restoring snapshot carry must fail those assertions.
This fixture requires a separately admitted guard/PID/isolation/cleanup plan.
Finite and unknown server transaction retention also limits retries that retain
the pin; neither the source fix nor synthetic controls prove universal duplicate
prevention or live provider behavior.

Own-room configuration is opt-in. No CLI setter or live provisioning is delivered;
unconfigured identities emit a local unavailable status and zero incident sends.
The operator supplies the known own project/lane route; runtime checks do not
classify arbitrary projection, broadcast or status rooms. The explicit global
`#system` refusal does not establish a new room-type policy. Conservative power
checks may refuse valid room-version-12 creator/default-omission cases. Live
membership, permission, subscriber receipt, provider behavior and transaction
retention remain unknown. Binding-log rotation is not established here.

## Principal coverage: account-data concurrency and history limits

Explicit removal is resolved: a single protected `host-principal` row refuses
with `principal_subscription_required` and a host-list remedy; `--all` removes
unprotected rows and reports each retained row with that remedy. Manual rows
remain removable. No force flag exists. Host removal of an ID or departure
from the room ends protection. Owner-DM cleanup maintains independent rows.

Cross-host account-data writers still have no compare-and-swap in the existing
client. Dedicated audit rows can be lost by a competing get/modify/put. Should
future server support provide conditional writes, or should a single writer
own subscription storage? Local delivery does not wait on this decision: each
cycle snapshots the atomically replaced host list without a policy lock or
network repair. Lists and cursors are not synchronized across hosts.

The accepted coverage-start window includes all messages behind the current
committed cursor when the cycle starts with coverage. This includes first
positioning, configuration/upgrade, changes during an in-flight cycle, and
cycles with unreadable metadata. These messages are not replayed. Initial
history crawling and retroactive principal backfill are removed. Existing
limited-sync recovery from the current cursor remains unchanged. Malformed
metadata warns once per delivery session and is visible in `subs check` and
`bind status`, but cannot freeze the cursor or pending-mail output.

Case-sensitive historical MXIDs remain supported intentionally; a case typo
cannot be corrected without potentially selecting a different identity. Exact
membership is checked by `subs check`; no normalization or alias inference is
safe. Manual projection selectors retain their explicit origin semantics;
only dedicated principal rows and mandatory host delivery require real sender
matching. Older binaries sharing account data/cursors retain their old behavior.

The new isolated fixtures are not executed in this producer round, by explicit
instruction. They use memory HTTP responses and private temporary stores, no
server, tmux or a2a processes. Live wake responsiveness, crash/fsync recovery
and multiprocess behavior remain unverified.
