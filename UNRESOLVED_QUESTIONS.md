# Unresolved questions

## Issue #308: Q4 and foundation runner gates

Q1/Q2/Q3/Q5/Q6 are recorded in `docs/e2ee.md` section 11. Q4 still needs an
owner decision on automatic versus explicit future-device admission and the
authority for approval changes. It gates U2b and later; it does not gate U1
foundations or U2a cross-signing bootstrap / public identity evidence.

The offline U1 worker cannot run tests, benchmarks or network advisory queries.
Source must obtain runner results, 1/8MiB operational p95 <=500ms and 64MiB
capacity p95 <=10s evidence for the optimized Save/Load path,
and external security-history review before treating the foundation as admitted.
These two-tier bounds are Source's engineering decision, not owner ratification.
A full-cap save can exceed the existing 5-second hook budget. U2a/U3 must keep
full-cap saves off the hook path with a pre-mutation size/latency guard or an
equivalent separately admitted design; this wiring remains unresolved. Hosted CI
does not run the capacity benchmark, and this follow-up supplies no new latency
measurements. Native and full-hook qualification remain separate gates.
The repository declares Go 1.25.12; this bench supplies 1.25.14. Exact 1.25.12
execution remains unverified. mautrix v0.31.0/v0.32.0 require Go 1.26 and are
excluded without a separate toolchain decision. No E2EE activation is delivered.

Source's U1 r2 resolution closes the server-only USK visibility assumption:
`/keys/query` exposes USKs only for the requesting user's own keys. The host pin
provides owner MSK/USK, both fingerprinted, and the MSK signature on the USK.
U2a verifies that evidence and the USK signature on the agent's own queried MSK.
A live Element ceremony witness still gates U2a shipment; it does not block U1.
U1 validates retained public shape only. No private owner credentials or trust
boolean substitute is authorized. Under A1, U2a must also enforce a2ad-only
custody under the dedicated service-user daemon, excluding agent access; local
identities outside a2ad get no custody and no home/local-base/env fallback.

U2a must additionally enforce the 0700-parent creation and ACL exclusion
provisioning contract, add a monotonic anchor outside snapshot rollback, and
derive signing keys with concrete goolm/pk. An executable no-OlmMachine/no-libolm
import-boundary test belongs to U2a or a dedicated follow-up. U4 owns composite
lock-order integration and full hook latency, beyond U1's Save/Load benchmark.
The full 64MiB cap is capacity evidence, not an interactive latency promise or a
larger hook deadline. Hooks must skip before crypto mutation when measured
prepare/commit time cannot be reserved, preserve the cursor and leave at least
one second for cancellation/output; interactive consumers recover.

Typed secret-inventory equivalence covers canonical snapshot-writer encodings
only. An altered AEAD-valid snapshot requires the per-generation wrapping key,
but accepted noncanonical JSON can lose overwritten values or stored spellings
through case-insensitive struct-member aliases, numeric map-key spelling aliases
and base64 normalization. Rejecting or otherwise inventorying those forms remains
future hardening; the legacy compatibility oracle shares alias gaps and does not
prove them absent. Issue #447 documents this limit without changing the decoder.

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
## Principal coverage: explicit subscription removal
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

## Hook executable custody: macOS ACL permissions

The executable-selection review identified that image and ancestry permission
checks use POSIX mode bits. They do not inspect macOS extended ACLs, which can
permit another account to modify an otherwise mode-safe image or directory.
This remains unresolved in hook installation and the shared terminal-refresh
image validator; no permission gate or Git-marker refusal has been relaxed.

A native ACL implementation and dedicated positive/negative platform fixtures
are needed before claiming ACL-level exclusion. This producer was restricted to
formatting, vetting and building, with no tests, installations or live process
changes. Introducing an unvalidated security parser or shell-output heuristic
would not establish the requested guarantee, so this finding is explicitly
retained rather than presented as fixed.

Codex owner-DM companion rules deny known selected, resolved and launch spellings.
Arbitrary undiscovered aliases and later image versions remain outside these
spelling-based rules; independent host-terminal checks still apply. No live
Codex rule-engine or existing-hook fleet verification was performed.

## C6: host and native qualification evidence

C6's repository implementation and inert unit fixtures are documented in
[daemon-service.md](docs/daemon-service.md). This disposable producer bench is
not authorized to execute tests, start/install a unit, reboot, provision root
files or inspect an installed native runtime. Source/Meta and host keepers must
supply these remaining evidence artifacts before native sign-off:

- Mac precheck and CI test execution, including planted negative controls.
- Provisioned `_a2a`/Linux account, final Infra owner-policy path/hash and reviewed
  script alignment; service-UID policy/image/directory ACL write-denial check.
- Recorded native `launchctl print`/`systemctl show` shapes for the conservative
  matching parser, then explicit install/status/uninstall checks.
- A removed-run-dir/reboot-equivalent boot followed by a real station connect,
  crash restart, bounded SIGTERM cancellation and next-cadence sweep detection.
- Controller integration that records actual recovered-delivery/traffic counts
  in the existing service context, followed by owner-selected real-traffic soak.
  C6-r3/P3-rescue-caller: the controller needs an authorized service-context
  reporting/accounting channel; C6 must not grant agents write access or add C7.
  C6-r3/P3-daily-soak: the owner must choose calendar days versus rolling 24-hour
  windows and minimum daily traffic; the current artifact uses interval totals
  and cannot certify daily traffic or authorize fallback removal.
- C6-r3/ACL-native: verify Darwin fgetattrlist ACL decoding on the installed
  target and Linux kernel faccessat2/no-extended-ACL assumptions. The private
  grant seam and SDK-shaped parser fixtures do not qualify native ACL denial.
- Exact installed Muse hook stdin/stdout and monitor websocket fixtures. This
  producer does not invent an installed version pin, upstream issue, local shim
  or native qualification verdict. Existing Claude/Codex fixtures remain.

Infra owns the operator `--check` exemption for an absent boot-volatile run
directory. Source owns acceptance of the documented preparation fallback;
there is no launchd inherited-listener path in this producer patch.
