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
unconfigured identities now use the existing mail-alarm owner route and optional lead target. Missing alert destinations remain locally
unavailable; no live routes are provisioned by this producer slice.
The operator supplies the known own project/lane route; runtime checks do not
classify arbitrary projection, broadcast or status rooms. The explicit global
`#system` refusal does not establish a new room-type policy. Conservative power
checks may refuse valid room-version-12 creator/default-omission cases. Live
membership, permission, subscriber receipt, provider behavior and transaction
retention remain unknown. Binding-log rotation is not established here.

## Stranded-notice fallback: retry lifetime

The producer slice uses one durable transaction per audience/episode in the
mail-alarm outbox, retrying failed incident sends with its bounded exponential
backoff. Ordinary stale-mail uncertainty holds are unchanged. Matrix transaction
retention is finite and has not been measured here: arbitrarily late retries
cannot promise universal server-side deduplication. Owner question: should
incidents have an explicit maximum retry age/manual hold, and what should it be?
The current implementation keeps retrying while the episode/audience remains
eligible, with the same transaction and destination.

No tests, live panes, live delivery or process-replacement experiments were run
on this host. Added tests use pure logic and a fake homeserver and await CI.
The older recovery descriptions above are historical and conflict with current
bind documentation/source; reconciling those unrelated recovery decisions is
outside this producer slice.
