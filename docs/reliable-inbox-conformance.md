# Reliable inbox v1 conformance and evidence

Status: **proposed acceptance specification**, companion to the
[reliable inbox contract](reliable-inbox.md), issue 115 / umbrella 114.
Every scenario in this document is a requirement, **NOT RUN** by this docs-only
producer. Scenario IDs are proposed test/evidence names, not assertions that an
executable test with that name exists. Existing source was inspected at
`78c5d615db871d811dc47bf6e34020424bdffe0a`; no native adapter or deployment is
qualified by publishing this matrix.

## 1. Evidence rules

An acceptance tuple is runtime family + immutable build/image + protocol/schema
fingerprint + OS/architecture + native launch/attachment mode + adapter/host
artifacts + deployed host. Include configuration, eligibility/environment policy
and tests/checker identity. A changed component invalidates affected evidence;
re-use unchanged evidence only with that full identity and coverage justification.
A release string alone, CLI help, a greeting or a fixture pass is insufficient.

Run the common black-box suite against **Claude Code, Codex CLI and Muse Code**.
All three are mandatory overall. Every deployed/enrolled tuple needs its own
coverage; do not invent uninstalled OS/runtime combinations as tested. Extend
inventory for each enrolled host and each counterpart enrolled by its own keeper.
Missing/unsupported native capabilities keep the applicable row and umbrella
open with owner/dependency, even while other tuples proceed.

Synthetic source fixtures establish local logic only. Native session runs must
establish exact live attachment, actual idle activation, inbox tool invocation,
original event exposure and local disposition. A display, hook output, accepted
RPC or printed receipt alone proves only that stage. UI/approval/draft checks
require actual native interactive sessions. Headless SDK/exec tests are separate
modes and cannot stand in for native CLI parity.

Every run MUST retain setup, exact commands and real exits, UTC/monotonic timings,
event/obligation/attempt/batch correlations, generation and native turn/tool
references, before/after durable state, and relevant log hashes. Store raw native
IDs, endpoint details and original payloads only in controlled private evidence;
public projections use opaque handles. Never publish secrets or native URLs.
Failed/truncated/missing logs are failed or unrun, never passing evidence. Do not
infer normal terminal exit from a last-message file.

Each executable test MUST state the specific claim beside it, with material
limits. Ground expected outcomes in the contract, not agreement between two
implementations. Pair rejection/invariant cases with nearby valid controls and
assert the intended reason plus post-failure state and no forbidden effects;
a timeout, unrelated setup failure or blanket rejection is not success.

## 2. Exact-version capability manifest template

This is a fill-in template; placeholders are not runtime values or qualification.
`unverified`, `qualified`, `unsupported` and `blocked` describe capability state.
Per-run evidence uses `not_run`, `pass`, `fail` or `inconclusive`; record the real
command exit independently, including nonzero exits in expected-rejection runs.
A required `unsupported` property is a gap, not a passing exemption.

```yaml
schema: a2a.reliable-inbox.capability/1
manifest_revision: REQUIRED
contract: reliable-inbox-v1
qualification: unverified
identity_connection_scope: PRIVATE_REFERENCE
runtime:
  family: claude_code_OR_codex_cli_OR_muse_code
  version: REQUIRED_EXACT_BUILD
  executable_sha256: REQUIRED
  protocol_schema_fingerprint: REQUIRED
platform:
  os_version: REQUIRED
  architecture: REQUIRED
  host_evidence_ref: REQUIRED
launch:
  mode: REQUIRED_NATIVE_CLI_OR_SEPARATE_HEADLESS_MODE
  launcher_source_head: REQUIRED
  launcher_artifact_sha256: REQUIRED
  host_specific_implementation: REQUIRED_BOOLEAN_WITH_SCOPE_JUSTIFICATION
  session_host_source_head: REQUIRED_IF_HOST_SPECIFIC_ELSE_NOT_APPLICABLE_WITH_REASON
  session_host_artifact_sha256: REQUIRED_IF_HOST_SPECIFIC_ELSE_NOT_APPLICABLE_WITH_REASON
  extracted_host_dependency: PUBLIC_EXTRACTION_SOURCE_CONTRACT_IF_HOST_SPECIFIC
  accepted_exported_api_source_receipt: REQUIRED_IF_HOST_SPECIFIC_ELSE_NOT_APPLICABLE_WITH_REASON
  extraction_completion_evidence: REQUIRED_BEFORE_HOST_IMPLEMENTATION_ELSE_NOT_APPLICABLE_WITH_REASON
  profile_lock_permission_model_policy_ref: REQUIRED
  native_attachment_proof_ref: REQUIRED_PRIVATE_REFERENCE
  sole_lifecycle_owner_ref: REQUIRED
  process_boot_birth_image_ref: REQUIRED_PRIVATE_REFERENCE
  endpoint_incarnation_owner_ref: REQUIRED_PRIVATE_REFERENCE
adapter:
  source_head: REQUIRED
  artifact_sha256: REQUIRED
  schemas:
    notify: a2a.reliable-inbox.notify/1
    observe: a2a.reliable-inbox.observe/1
    status: a2a.reliable-inbox.status/1
    retire: a2a.reliable-inbox.retire/1
  doorbell_encoder_source_version_digest: REQUIRED_SAME_STREAM_AND_ONE_SHOT
  notice_max_bytes: 300
  capability_digest: REQUIRED
  notify_authority_scope_ref: REQUIRED
  observation_authority_scope_ref: REQUIRED
  transport_credential_owner_scope_ref: REQUIRED_OR_EXPLICITLY_NONE
policy:
  eligibility_version_revision: REQUIRED
  explicit_follow_migration_ref: REQUIRED
  environment_policy_source_head: REQUIRED_ACCEPTED_PR111_POLICY
  environment_policy_evidence_ref: REQUIRED
  intake_enrollment_version: REQUIRED
  state_reader_migration_retention_ref: REQUIRED
  fixed_notification_window_seconds: 30
  hourly_budget: REQUIRED_ACTUAL_POLICY
  health_interval_seconds: REQUIRED_ACTUAL_POLICY
  reconciliation_interval_seconds: REQUIRED_ACTUAL_POLICY
  sweep_timeout_seconds: REQUIRED_ACTUAL_POLICY
  stream_stall_criterion_ref: REQUIRED
  detector_independence_ref: REQUIRED
  health_detection_bound_ref: REQUIRED
  sweep_discrepancy_detection_bound_ref: REQUIRED
  restart_eligible_set_and_rollback_ref: REQUIRED
properties:
  hook_consumer_inbox_same_session_generation_lease: {state: unverified, evidence: [], gap_owner: REQUIRED}
  detector_stream_one_shot_body_isolation: {state: unverified, evidence: [], gap_owner: REQUIRED}
  exact_session_attachment: {state: unverified, evidence: [], gap_owner: REQUIRED}
  idle_activation: {state: unverified, evidence: [], gap_owner: REQUIRED}
  safe_busy_boundary: {state: unverified, evidence: [], gap_owner: REQUIRED}
  human_approval_draft_preservation: {state: unverified, evidence: [], gap_owner: REQUIRED}
  detached_viewer: {state: unverified, evidence: [], gap_owner: REQUIRED}
  exact_session_restart: {state: unverified, evidence: [], gap_owner: REQUIRED}
  attempt_reconciliation: {state: unverified, evidence: [], gap_owner: REQUIRED}
  native_idempotency_lookup_window: {state: unverified, evidence: [], gap_owner: REQUIRED}
  tool_result_persistence_observation: {state: unverified, evidence: [], gap_owner: REQUIRED}
  intake_recovery: {state: unverified, evidence: [], gap_owner: REQUIRED}
  independent_watcher_supervision: {state: unverified, evidence: [], gap_owner: REQUIRED}
  pre_model_empty_gate: {state: unverified, evidence: [], gap_owner: REQUIRED}
  periodic_reconciliation: {state: unverified, evidence: [], gap_owner: REQUIRED}
  explicit_only_follow_all_paths: {state: unverified, evidence: [], gap_owner: REQUIRED}
  hook_wire_and_rearm: {state: unverified, evidence: [], gap_owner: REQUIRED}
hooks:
  exact_events_and_input_output_schema_ref: REQUIRED
  wire_compatibility_and_forbidden_outputs_ref: REQUIRED
  total_deadline_seconds: 5 # maximum; actual value may be stricter
  total_stdin_lock_child_output_deadline_ref: REQUIRED
  install_uninstall_pause_resume_evidence_ref: REQUIRED
acceptance:
  idle_20_ref: REQUIRED
  busy_20_ref: REQUIRED
  deterministic_faults_ref: REQUIRED
  active_soak_1h_ref: REQUIRED
  idle_12h_ref: REQUIRED
  representative_traffic_ref: REQUIRED
  enabled_observable_fallback_ref: REQUIRED
  ordinary_rescue_completed: UNMEASURED
  ordinary_rescue_unresolved: UNMEASURED
  controlled_rescue_evidence_ref: REQUIRED_SEPARATE_FROM_SOAK
  remaining_supervision_after_retirement_ref: REQUIRED
  independent_source_review_ref: PENDING
  meta_review_ref: PENDING
  legitimate_checks_ref: PENDING
  signed_landing_head_ref: PENDING
  installed_artifact_correspondence_ref: PENDING
  owning_keeper_adoption_ref: PENDING
```

The manifest MUST describe the actual native operation: what `accepted` proves,
what can be queued while busy, whether admission is atomic with lifecycle state,
what constitutes authoritative `not_applied`, history completeness limits,
idempotency key scope/retention, and behavior beyond that retention. If durable
context observation is unavailable, record that gap and provide qualified pinned
intake replay; do not invent a context ACK. A native uncertainty primitive needed
by a mandatory fault row cannot be waived by local journaling.

Host-independent source/parser/adapter tuples MUST set the conditional host
fields to explicit `not_applicable` with their host-independence justification;
they are not blocked by an absent extraction receipt. That scope does not qualify
host attachment, host implementation or deployment; required native rows remain
open until independently evidenced. Host-specific work requires the accepted
exported-API/source receipt defined in contract §14 **before** implementation,
including exact source repository/commit, API/schema, artifact and owner
acceptance. No invented extraction PR/commit or private tracker ID fills it.

### 2.1 Initial capability inventory

| Required family | Candidate surface | Evidence boundary at authoring |
| --- | --- | --- |
| Claude Code | Exact-session MCP channel or native background/async rewake, composed with independent supervision/rearm. | Unverified here. Preview/account availability, native CLI attachment and unattended wake need exact-build evidence. |
| Codex CLI | Sole host-owned app-server/thread with existing counts-only tool output adapter. | Source allowlist at the inspected base is 0.160.0 / 0.160.1 / 0.161.0. It does not qualify reported installed 0.163.0; no runtime tuple tested here. |
| Muse Code | Existing issue 113 Phase0, then exact-session MSP/native messaging/Monitor composition. | Offline hook/plugin or reported frozen CLI help is not native context/idle proof. No competing Phase0 or provider probe in this unit. |

## 3. Run record and independent review template

For each scenario/tuple create a record with the following fields. This document
does not create the executable harness or any passing records.

| Field | Required evidence |
| --- | --- |
| Identity | Scenario ID, tuple/manifest digest, contract clauses, source/test/configuration/checker identities. |
| Setup | Authorized isolated test session and lifecycle owner; exact attachment/boot/process/endpoint references; starting mail/attempt/intake/subscription state. |
| Trigger | Actual event IDs/revisions and independent expected eligibility, injected fault boundary and configured throttle/gates/budget. |
| Execution | Exact commands, argv/log/artifact paths, UTC/monotonic start/end, real exits/signals; distinguish expected rejection from failing harness. |
| Stages | Durable mail time, eligibility time, dispatch/reservation, native acceptance or uncertainty, inbox call, print/flush/receipt, native context, confirmation and application result separately. |
| Invariants | Before/after pending/pins/cursor/reservations/generation; absence of forbidden submission, receipt, model activation, approval/draft mutation, duplicated effect or leaked payload/authority. |
| Oracle | Requirement-grounded expected verdict/reason, nearby valid control, known-violation control reference and actual named test assertions. |
| Result | `not_run`, `pass`, `fail` or `inconclusive`, finite coverage limits, gap owner/dependency and evidence hashes. |
| Review | Independent exact-head verdict plus Meta/host review as applicable; required real checks, signed landing and installed-head correspondence remain separate. |

Application-effect fixtures use a bounded idempotent test operation with an
independent status oracle, never real user effects. Faults run only in isolated,
authorized sessions; do not kill keeper services, change live credentials, type
into live provider panes or revive retired identities for evidence.

## 4. Native lifecycle and scheduling matrix

Every row applies separately to Claude, Codex and Muse on each enrolled tuple.
All rows are **NOT RUN**. “Control” identifies a nearby valid case or contrasting
forbidden condition; it is not an executed control claim.

| ID / contract | Trigger and required positive outcome | Forbidden outcome and post-failure invariant / control |
| --- | --- | --- |
| N01 / RI-01, RI-08 | Untouched eligible idle session receives one addressed event: native notice, actual inbox invocation, original event ID, durable local disposition. | No owner prompt, manual Enter, terminal bytes, substituted conversation or second provider owner. Durable obligation survives failure; compare no-mail N09. |
| N02 / RI-06 | Busy reasoning, short tool and long tool as distinct variants: mail durable immediately, intake at permitted native boundary, current work preserved. | No replacement/interruption, tool-state loss or falsely excluded busy latency. Record arrival-to-boundary; paired idle N01. |
| N03 / RI-06 | Approval dialog, waiting-user question, plan gate and typed draft, each independently exercised; retain mail and expose exact hold. | No answered approval, submitted/erased draft, consumed unrelated input or bypassed plan. Resume same session explicitly and require N01 progress. |
| N04 / RI-01, RI-08 | Detach viewer without closing eligible session; later event activates same native conversation. | No attached-terminal dependence or hidden terminal input. Closed/parked negative stays closed; N01 attached control. |
| N05 / RI-05, RI-11 | Kill owned fixture controller; independent supervisor restarts and reconstructs pending/intake under same ownership. | No model polling/rearm prompt, second cursor or lost reservation. Controller-alive N01 is control. |
| N06 / RI-11 | Native process death, authorized exact-session restart and reboot fixture; recover only eligible active set. | No new conversation, duplicate owner, old prompt rerun, retired/parked revival, permission/profile/model drift. Include parked identity negative alongside active restore. |
| N07 / RI-01, RI-05 | Competing consumer, stale hook/confirm, PID reuse, boot change and endpoint replacement each refuse wrong ownership/generation. | No stale submit, successor mutation, receipt or signal to reused PID. Fresh valid-generation N01 must work. Verify hook, idle consumer and inbox enroll the same actual session, generation and lease; absent/mismatched subprocess propagation refuses enrollment without changing the foreign lease. Codex actual-thread `A2A_SESSION_ID` and Muse supported propagation need separate evidence. |
| N08 / RI-03, RI-09 | Hook, native path, Monitor and sweep race on same event; one stable obligation and shared bounded dispatch. | No independent queues/cursors, double effect or infinite Stop loop; repeated signals counted/coalesced. Single-path N01 is control. |
| N09 / RI-03, RI-07 | No mail/recovery obligation over prolonged idle and scheduled sweeps: zero mail-induced model turns/receipts; later real mail passes N01. | Empty one-line model reports fail; silence alone is insufficient without sweep execution/positive later delivery. |
| N10 / RI-07 | Throttle window already held; admit events at +0s and +15s inside it; one eventual coalesced notice after due and both events exposed. | Never zero notices, dropped second event, moving due time, false receipts or lost obligation. Inspect both IDs through intake; pair N11. |
| N11 / RI-07 | Eligible arrival just after fixed window opens proceeds; first-announced-then-second-arrival variant gets its later due delivery. | No hidden permanent suppression from rung/early hook or lost later event; no rate bypass. Pair N10. |
| N12 / RI-06, RI-07 | Hourly budget exhaustion, throttle, pause and release, separately and combined: pending age/hold visible; one coalesced activation when genuinely eligible. | No dropped mail, forced gate release, extra wake or budget reset on rebind/restart/clock change. Compare unheld N01. |
| N13 / RI-06 | Pause/retire while attempt prepared; generation/gate recheck fences new admission and reconciles already-applied attempt. | Do not claim a precheck is atomic or a late accepted call was rejected. Pending/journal preserved; ready-generation control progresses. |
| N14 / RI-06, RI-12 | Agent ignores notice or omits disposition; bounded reconciled recovery, then explicit stalled state and independent incident. | No infinite activation/ACK loop or accepted-as-handled label. Valid confirmation I06 must end intake recovery. |
| N15 / RI-08 | Unsupported build/schema/capability, including unqualified Codex build, reports exact gap/dependency. | No inferred allowlist widening, terminal fallback as qualified, development trust bypass or success-by-greeting. Qualified isolated tuple N01 is positive. |
| N16 / RI-08, RI-09 | Installed hooks on actual session after N07 enrollment, repeated Stop, clear, compaction and disconnect: prove rearm/lifecycle transfer behavior for each supported mode. | Install/schema success alone cannot pass idle. Preserve Claude/Codex initial wire and Muse nonblocking output; N01 after each boundary. |
| N17 / RI-05 | Validate versioned notify/observe/status/retire requests and capability digests; correct authorized scope succeeds. | Unknown version, missing/malformed fields, wrong endpoint/session/generation/caller, malformed count/reference, mismatched projection schema, or notice over the manifest's 300 UTF-8 byte ceiling refuse before mutation; no generic RPC/permission/profile capability. Assert closed reason and unchanged journals. |
| N18 / RI-05, RI-11 | Retire exact generation after both host/a2a uncertainty resolved; checkpoint obligations and fence dispatch. | Off/abandon alone cannot complete retirement/rebind; uncertain state remains visible. Same settled request idempotent; stale request cannot retire successor. |

## 5. Native uncertainty, transport and trust matrix

All rows are **NOT RUN** for all required tuples. Native inability to resolve a
required uncertain state keeps that recovery property open; an honestly retained
uncertain state is correct safety behavior, not evidence of full eventual recovery.

| ID / contract | Required valid/fault behavior | Forbidden outcome and retained state / control |
| --- | --- | --- |
| U01 / RI-04, RI-06 | Crash immediately before native I/O after durable attempt/reservation; recover exact attempt and immutable notice. | No new attempt before resolving possible application, no erased reservation. Normal accepted attempt is control. |
| U02 / RI-06 | Native applies notice, response lost; host and a2a journals both reconcile exact native marker/command and progress. | No blind second launch, fabricated rejection or receipt. Tick during uncertainty must respect fence; pair definitive no-application U04. |
| U03 / RI-06 | Missing/partial/failed/pagination-exhausted history remains unknown and exposes dependency; exact native match permits accepted observation. | Absence, assistant quote or peer body must not grant resend; preserve original attempt/payload/reservation. Include complete authoritative lookup positive. |
| U04 / RI-06 | Definitive native refusal with independently established non-application; retry only per qualified admission/key rules, preserving rate accounting. | Arbitrary internal error/timeout is not refusal. Exercise same-key retention expiry and foreign-key mismatch; no inferred dedup beyond supported window. |
| U05 / RI-06, RI-11 | Host lifecycle ledger uncertain while a2a appears settled, and reverse: replacement/retirement waits for both. | No one-ledger shortcut, socket-failure takeover or duplicate owner. Both-settled N18 is control. |
| U06 / RI-05, RI-06 | Human submission/lifecycle transition races status-read and notify; prove supported conditional/queued semantics. | No draft/approval effect; snapshot alone cannot assert compare-and-submit. Missing native primitive is unsupported, not patched by screens. Pair stable ready N01. |
| U07 / RI-04 | Matrix/network disconnect and reconnect, auth refusal, slow child, held lock and blocked output: deadlines and exact stage retained. | No cursor skip, new authentication/profile repair, unbounded child/input wait or false healthy status. Restore valid fixture connection and drain. Hook stdin/lock/network/child/output total stays within the pinned ceiling of 5 seconds; exercise a valid under-bound hook too. |
| U08 / RI-04 | Identical duplicate event deduped by stable key; conflicting digest is integrity fault without overwrite. | No new business effect, payload replacement or claim of unbounded historical dedup. Valid distinct event remains deliverable. |
| U09 / RI-02, RI-05 | Spoofed sender/localpart, malicious title/body, malformed event/native metadata, control characters and fake owner claims cannot escape bounded framing or validated notice fields. | No raw peer text in privileged notice/watch stream, chosen endpoint/operation, owner grant, tool execution or diagnostic secret. Legitimate pinned owner U10 passes. |
| U10 / RI-02, RI-12 | Ordinary untyped pinned owner message via existing from-owner subscription in local/federated room; verify principal and permitted answer-first behavior; correlated reply separately recorded. | No typed-kind/explicit-to prerequisite or same-localpart trust; mirror send/ACK cannot prove inbound wake. Spoof negative U09 and separate outbound receipt evidence. |
| U11 / RI-09 | Reviewed PR111 detached-consumer environment policy excludes ambient/session/registration secrets; separately provisioned adapter credential scoped to actual session/owner transport. | No second environment policy, copied HOME, inherited interactive environment or secret logs. Authorized narrow transport succeeds; foreign/retired credential cannot notify. |
| U12 / RI-12 | Broken recipient's failure reported by independent existing keeper/status route; during Matrix outage retain incident locally and publish when transport returns. | Do not depend only on waking the broken recipient or report PID/heartbeat as delivery proof. Healthy native intake N01 restores readiness. |
| U13 / RI-03, RI-05 | Monitor stream and transitional one-shot waiter each receive an admitted event planted with a unique body marker, malicious subject/topic/room label and error marker. Both use the same allowlisted encoder; bounded counts-only notice fires, one consumer lease exists, framed inbox/read alone exposes original content. | Marker, subject, topic, room label, raw JSON excerpt and stderr/error marker never appear in any model-facing detector/notice output. Raw ordinary `wait`/`watch` feed, even denylist-filtered, fails. Test both output modes, empty/error completion (zero model turns), same-encoder identity and valid later mail; do not claim a fixture proves native receipt. |

## 6. Mailbox and package F crash matrix

All rows are **NOT RUN**. Verify original payload reconstruction locally after
receipt **and compaction**, without relying on a network refetch. Inject only at
bounded fixture seams. Source-seam errors do not prove physical power-loss or
storage-hardware behavior; record that limit explicitly.

| ID / contract | Boundary and required behavior | Forbidden outcome and post-failure state / control |
| --- | --- | --- |
| I01 / RI-04 | Crash/error before append, after append before cursor, during cursor persistence; retained/replayed event or explicit gap, never silently skipped. | Cursor cannot advance past unpersisted relevant mail; duplicate stable ID is safe. Valid append/cursor sequence passes. |
| I02 / RI-10 | Opt-in select/pin/journal transaction interrupted before output; recover original batch or keep mail unread. | No printed unjournaled batch, dangling pin/reference, premature compaction or partial enrollment reported complete. Legacy unenrolled inbox remains compatible. |
| I03 / RI-10 | Partial write and flush failure each leave entries unreceipted and reconstructible; successful write+flush receipts exactly selected IDs. | No receipt on partial output, lost original or acknowledgment of unprinted next batch. Check actual selected and remaining IDs. |
| I04 / RI-10 | Crash after print before receipt permits duplicate exposure with stable IDs/replay attribution. | No duplicate application effect or receipt-as-handled inference. Fully receipted ordinary second inbox is empty for those events. |
| I05 / RI-10 | Crash after receipt before native durable tool-result persistence; unread zero, compact mailbox, recover pinned original batch. Obtain its reference from the enrolled structured inbox header or own-generation `delivery status`; with unread zero require a one-batch recovery notice whose count is unconfirmed batches and sole reference is the opaque batch ID. | No batch ID in legacy hook/public status, stale-generation reference leak, suppression by inbox-zero, network-only reconstruction, original revision loss or supervisor ordinary inbox/receipt. Nearby persisted context I06. |
| I06 / RI-10 | Tool result persisted before confirm: authoritative exact-result observation if qualified, otherwise explicit bound replay; valid local disposition ends intake recovery. | No invented context ACK/model understanding, application completion or foreign-batch confirm. Test unavailable observation with qualified replay. |
| I07 / RI-05, RI-10 | Stale generation, copied batch token, wrong member/event, foreign identity or malformed confirm refuses with intended reason. | No successor/foreign intake closure, receipt changes, payload leak or Matrix ACK. Same-generation valid recorded/deferred/no-action cases pass. |
| I08 / RI-10 | Confirm recorded/deferred/no-action; deferred retains task/blocker and resume condition/deadline; informational no-action needs no reply. | No ACK storm, repeated interruption of deliberate deferral or task marked delivered on receipt. Actual actionable result remains separately tracked. |
| I09 / RI-10 | Crash after confirm before effect retains linked application task; after effect before result uses independent application status/idempotency before repetition. | No exactly-once claim or blind replayed business effect. A no-effect status control permits one authorized fixture effect. |
| I10 / RI-04, RI-10 | Truncated preview still receives legacy printed receipt; dependent decision requires full original read with revision evidence. | Preview/receipt alone cannot mark full read/handled. Include short complete body and truncation boundary positives. |
| I11 / RI-07, RI-10 | 1, 50, 51 and 1,000+ admitted entries plus irrelevant flood: bounded frames, fair continuation, oldest age preserved. | No starvation, unprinted receipt, cursor loss or indefinite coalescing; verify complete admitted ID set and irrelevant exclusions. |
| I12 / RI-04, RI-10 | Capacity/storage error, unresolved-pin compaction, incompatible downgrade reader and retention checkpoint crashes retain reconstructible work. | No oldest-pending eviction, unresolved expiry, dangling original or format deletion to force downgrade. Compatible reader/checkpoint succeeds. |
| I13 / RI-04 | Restart `watch` after entries marked shown; arbiter reconciles durable pending/intake rather than assuming stream replay. | Shown/rung is not receipt or handled. New unread event and incomplete receipted batch both remain discoverable by proper metadata/recovery paths. |
| I14 / RI-02, RI-04 | Invite and history-gap fixtures preserve baseline mailbox-kind behavior and stable mailbox IDs across migration/restart; surface gap limits through framed inbox/read with retained range metadata. | No thread-filter drop, silent history gap, invented gap event ID, or raw invite/gap text in notice. Valid ordinary event control still delivers; counts notice alone does not establish full catch-up. |

## 7. Composition, supervision and rescue matrix

All rows are **NOT RUN** on every required runtime tuple. Record measured bounds
against each manifest's configured detection interval/timeout, then measure repair
and intake separately. Merely observing concurrent discovery is not a rescue.

| ID / contract | Trigger and required evidence | Forbidden outcome / valid control |
| --- | --- | --- |
| W01 / RI-03 | Healthy empty sweeps actually execute metadata-only checks, zero model starts/receipts; later N01 delivery works. | A model one-line empty report, scheduler calling inbox, or disabled/broken delivery cannot pass. |
| W02 / RI-03, RI-12 | Watcher exits, then separate fixture silently stalls with PID alive: independent supervision detects within bound, owned repair, correlated pending intake and rescue record. | PID alive or heartbeat alone cannot satisfy health; retained obligation and failed repair remain visible. Healthy N01 control. |
| W03 / RI-03 | Transport/cursor stalls while watcher heartbeat stays healthy; detect upstream stall/discrepancy, repair only owned component. | False ready, cursor reset, second consumer or fake progress prohibited. Resume transport and verify actual event intake. |
| W04 / RI-03, RI-06 | Hook/Monitor/native/sweep overlap, including fallback tick after lost ACK: one obligation, uncertainty preserved, one authorized arbitration path. | No blind resend/effect, new lease or independent gate policy. Single detector N01 and U02 are controls. |
| W05 / RI-03 | Overlapping scheduled ticks and crash mid-sweep: lock/lease/checkpoint permits one authorized action; restart resumes missing work. | No dropped durable obligation, stale-generation dispatch, overlapping effect or budget reset. Ordinary solitary tick passes. |
| W06 / RI-03, RI-11 | Sleep/reboot/wall-clock jump: current-generation bounded reconciliation, missed ticks reported, obligations/budgets retained. | No tick replay burst, old PID/session launch or forced budget replenishment. Explicitly parked identities remain parked. |
| W07 / RI-03, RI-06 | Paused/retired/human-waiting/wrong-generation sweeps record observation and hold; resumed authorized generation progresses. | No approval response, takeover, scheduler receipt or forced resume. Re-enqueue is not authority to dispatch. |
| W08 / RI-10 | Printed/receipted batch loses model context; sweep finds incomplete intake despite unread zero and requests bound recovery. | No handled label, cleared journal or lost payload pin. Completed confirmed batch must not be replayed needlessly. |
| W09 / RI-12 | Primary deliberately disabled in isolated fixture; preserve demonstrable missing/overdue state, causal fallback repair and later intake; increment completed rescue. | Concurrent discovery, attempted repair without progress, skipped plant or unrelated failure cannot count completed rescue. Healthy overlap increments observed/coalesced only. |
| W10 / RI-12 | Real need with failed repair increments unresolved rescue and blocks retirement; successful later progress correlates resolution without erasing history. | Zero counter from unavailable instrumentation or reset must not qualify. W09 proves healthy counting/recovery. |
| W11 / RI-12 | Disable fallback, break rescue counting or remove representative traffic in isolated retirement checker fixtures; retirement rejects apparent zero. | No-traffic/disabled/broken zero is not evidence; complete genuine enabled-soak positive must pass the checker. |
| W12 / RI-12 | Qualified primary with enabled observable fallback and representative traffic completes required counts, active/idle soaks with zero ordinary completed/unresolved rescues. | Planned faults are separate; genuine rescue/unmet need requires repair/new soak. No early umbrella completion. |
| W13 / RI-12 | Retire one redundant path after review; isolated silent-death control proves remaining independent supervision still repairs within bound. | No sole remaining stream supervising itself, automatic terminal path relabeled native, or lost rollback; a responsibility cannot disappear with its fallback. |

## 8. Explicit-subscription and migration matrix

All rows are **NOT RUN**. Execute the core S01–S04 sequence in **Claude, Codex and
Muse**, across hook, native/bind, Monitor and scheduled-reconciliation paths.
A classifier-only fixture is necessary source evidence but cannot prove deployed
path agreement. Use a foreign thread with no other covering subscription for
zero-new-entry controls; otherwise independent legitimate routing invalidates
the negative setup.

| ID / contract | Trigger and valid outcome | Forbidden outcome and post-state / control |
| --- | --- | --- |
| S01 / RI-02 | Unsubscribed identity posts one answer into active foreign thread; subsequent non-addressed replies yield zero new mailbox entries, wake obligations and model activations. | Participation/authorship must not create intent. Keep shared history readable; S02 proves valid delivery rather than blanket exclusion. |
| S02 / RI-02 | Explicit follow without active mute admits that same thread; repeat follow is idempotent; unrelated thread remains excluded. | No duplicate obligation or broadened selector scope. S01 before follow and unrelated-thread negative ground classifier behavior. |
| S03 / RI-02 | Mute stops thread-only traffic; explicit follow visibly clears mute and resumes it under declared policy. Posting alone preserves mute. | No silent post-unmute or follow outcome hidden from status. Direct/owner/mention positives S04 remain. |
| S04 / RI-02 | Direct-to, mentions and pinned ordinary untyped owner messages still deliver, with/without thread follow and mute. Independent explicit covering subscriptions retain exact scope. | No higher-priority routing suppression or owner/localpart spoof; expose actual eligibility reasons. Paired no-follow thread-only S01. |
| S05 / RI-02 | Topic creator, attachment sender, assignment responder, required reviewer and quoting/reference-only participant each remain unfollowed absent explicit intent. | No helper/post/review path auto-follow. An explicit post-follow option, if implemented, needs its own positive and omitted-option negative. |
| S06 / RI-02 | Restart, history resync, new binding generation and source recovery do not reconstruct implicit follows from own posts. | No durable intent inferred from thread history or shown/receipt state. Existing verified explicit follow survives. |
| S07 / RI-02 | Inventory separates local thread/mute state from Matrix account-data subscriptions and lists explicit/implicit/unknown/muted/pending. Indistinguishable legacy explicit-follow and participation rows are both unknown; operator sees re-follow requirement before cutover. Re-follow restores only genuinely requested future thread delivery. | No guessed provenance or silent unknown admission. Account-data selectors/source/expiry/start, rule-24 room-join baseline and recorded trims remain unchanged; no conversion between stores. Preserve mutes and cutover revision. |
| S08 / RI-02, RI-10 | Already-durable legacy mail and intake survive migration with exact receipt/pin/disposition states; drain through normal intake. | No deletion/receipt/handled mutation to make post-cutover counters zero. Report legacy tail separately; future thread-only S01 remains excluded. |
| S09 / RI-02 | Concurrent posts and follow/unfollow, repeated migration and crashes before/after cutover checkpoint: restartable/idempotent ordering preserves real intent. | No post-created follow, lost concurrent explicit change or false migration completion. Compare intact inventory/checkpoint and deterministic valid ordering. |
| S10 / RI-02, RI-03 | All notification paths share eligibility: rejected future traffic cannot reappear through hook/native/Monitor/sweep; earlier genuine durable obligations recover. | No recovery-based classifier bypass, competing policy or mislabeling old pending as new acceptance. Pair S01/S02 and W08. |
| S11 / RI-02 | Isolated planted implicit-follow regression is rejected by named checker test while genuine explicit-follow path works. | A skipped/not-applied plant, unrelated setup failure or all-delivery-broken checker is not a control pass. No mutant result claimed here. |
| S12 / RI-02 | Follow → unfollow → new thread-only traffic gives zero new entries/wakes, with mute state empty. Repeat unfollow is idempotent; posting afterward still does not follow. A later explicit follow restores delivery. | No unfollow-as-mute side effect, erased pre-existing mute or loss of independent subscription/direct/mention/owner delivery. Pair each preserved reason with S04. |
| S13 / RI-02 | At declared intent-store capacity, additional follow/mute refuses with visible capacity result and unchanged prior intent; a valid under-capacity mutation succeeds. | No silent oldest-entry eviction, partial intent mutation or falsely reported success; preserved mute/follow rows still enforce their original policies. |

## 9. Checker controls and mutation reporting

Before relying on a conformance instrument, establish that it detects a known
violation of its claimed property in an isolated fixture. At minimum include:
implicit-follow admission (S11), false-zero retirement (W11), and actual
primary-loss rescue correlation (W09), raw-detector feed and stderr leakage
(U13). Include native resend-with-uncertainty,
receipt-before-output and generation-bypass plants where those instruments are
used to accept the corresponding invariants. These are bounded control
requirements, not permission for third-party probes or destructive live tests.

A control passes only when the intended plant was applied and the named test
fails for the intended assertion, while the nearby valid control passes. Record
real exit codes. A checker crash or unrelated setup failure is a failed control.
Retest relevant controls when checker, configuration or contract identity changes;
do not run an arbitrary superficial mutation quota.

Use this reporting shape for actual executable evidence:

| Mutant/control | What it narrows the gate to | Named test that fails | Surviving bound / evidence |
| --- | --- | --- | --- |
| Planned implicit-follow plant | Participation alone cannot admit thread-only traffic. | NOT RUN; proposed S11 is not an executable test name. | No killed mutant claimed. |
| Planned false-zero retirement plant | Zero rescues requires enabled fallback, real traffic and working measurement. | NOT RUN; proposed W11 only. | No killed mutant claimed. |
| Planned lost-ACK resend plant | Shared tick cannot bypass uncertain native attempt. | NOT RUN; proposed U02/W04 only. | No killed mutant claimed. |

Replace planned rows with actual source mutation identity, assertion/test name,
command/exits and evidence. Any mutant without a named failing test is a
**survivor**; state the finite bound that survival leaves. Parser/synthetic kills
never establish native CLI idle or context behavior.

## 10. Quantitative and rollout gates

For each required deployed tuple, require all deterministic fault/negative rows
plus at least **20 idle and 20 busy deliveries**, **one-hour active soak** and
**at least 12-hour idle test**, including a valid later arrival without a human
poke. Record distribution, actual active/idle durations, model/load conditions,
event IDs and all held/stalled/failed outcomes. Counts are an initial acceptance
plan, not a mathematical reliability proof. An early working canary can proceed
through its actual owner before full soak; completion cannot.

Measure separately:

| Measurement | Required report |
| --- | --- |
| Durable arrival to eligibility | Throttle wait, native safe boundary, budget/human/other hold components and unconditional pending age. |
| Eligibility to native acceptance | Proposed healthy-local p95 <=5s / p99 <=10s; exact admitted-state definition and population. |
| Arrival to acceptance | Proposed no-hold throttle-aware p95 <=35s, not an arrival-to-acceptance five-second claim. |
| Acceptance to actual inbox invocation | Proposed healthy idle <=30s, actual model/load assumptions and failures. |
| Busy boundary to intake | Proposed <=5s once both permitted boundary and throttle allow; arrival-to-boundary remains visible. |
| Stages after inbox | Print/flush, receipt, native durable context, confirmation, application result and reply/mirror independently. |
| Holds and overdue work | Counts, ages, time fractions and success/held/stalled denominator; do not censor downtime or uncertain attempts. |
| Independent recovery | Configured/measured health and slow-sweep detection bounds, repair duration, subsequent intake and missed sweeps. |
| Fallback | Enabled/observable intervals, representative traffic, observed/attempted/completed/unresolved/coalesced/empty counters; controlled faults separated. |

Retirement requires exact deployed tuple qualification and representative
observation with fallback enabled throughout, zero ordinary completed rescues,
zero unresolved rescue needs and no missed eligible obligations. Genuine rescue
requires diagnosis/fix and a new qualifying soak. Planned fault controls remain
visible in separate totals, never silently excluded from ordinary incidents.
Verify remaining independent supervision and rollback before retiring one path.
No-traffic/disabled-fallback zero does not pass.

Each host adopts through its actual keeper/launcher lifecycle. The coordinating
meta role and configuration-management owners choose non-critical canaries; the
coordinating meta session and sysadmin keeper are excluded. The old consumer
is drained by its own session. Preserve exact conversation, gates, environment,
profile/model/permissions, pending state and parked predecessors. Do not remotely
operate another keeper's sessions or override registration/credential holds or
stopped work under its own owner.

## 11. Coverage, dependencies and delivery handoff

| Contract surface | Required scenario coverage | Accountable seam |
| --- | --- | --- |
| RI-01 ownership/native parity | N01, N04–N08 | A2a arbiter/adapter; sole existing host lifecycle owner. |
| RI-02 explicit eligibility | S01–S13, U09–U10 | A2a classifier/subscriptions/migration; shared decision across paths. |
| RI-03 four-layer composition | W01–W08, N05, N08–N09, U13 | A2a shared arbiter; existing host/launcher independent scheduling/supervision. |
| RI-04 durable facts/storage | U01, U07–U08, I01–I02, I11–I14 | Existing mailbox/attempt source; additive durable records. |
| RI-05 narrow versioned seam | N07, N17–N18, U05–U06, U13, I07 | A2a adapter and existing session-host attachment/authority. |
| RI-06 uncertainty/human safety | N02–N03, N12–N14, U01–U06 | Shared attempt reconciliation; native primitive/host arbitration. |
| RI-07 throttle/fairness/latency | N09–N12, I11, quantitative report | A2a existing spacing/budget policy plus enrolled trailing controller. |
| RI-08 native runtime capabilities | N01–N04, N15–N16, every native row | Separate Claude/Codex/Muse exact tuple owners. |
| RI-09 hooks/environment | N08, N16, U07, U11 | A2a installers and reviewed PR111 environment-policy integration. |
| RI-10 intake recovery | I02–I12, W08, S08 | Package F / intake leaf 116; compatible journal/payload pins. |
| RI-11 lifecycle/adoption | N05–N07, N18, U05, I12, W06 | Existing SH1 host, launcher/infra-exit and each installed-host keeper. |
| RI-12 health/rescue/retirement | N14, U12, W01–W13 | Independent host supervision, shared telemetry, owning keepers. |
| RI-13 owner sequencing | Extraction/dependency and source-identity review before affected code | Existing SH1 extraction/source dependency; host code requires the accepted exported-API/source receipt in contract §14. |
| RI-14 acceptance | All required rows, tuple inventory, review/check/landing/adoption ledger | Integrator, independent reviewers, Meta and actual host owners. |

Session-host-specific implementation MUST WAIT for the existing SH1 extraction
and exact accepted exported-API/source receipt defined in contract §14, then use
that source. Link the public extraction/source dependency and receipt on affected
leaves; retain private tracker mapping only in the controlled integration ledger.
Independent a2a contract/adapter work may proceed without inventing a
host in task-board. Curator owns public-host acceptance; launcher/infra-exit own
their composition, SH1 owns actual lifecycle/attachment/supervision, and each
keeper owns installed adoption. Use the A–H mapping in the
[contract](reliable-inbox.md#14-work-packages-actual-owners-and-sequencing-ri-13).

Issue 89 remains incident custody; 100 remains separate trusted-editor
conditional design; 110 remains legacy registry qualification; 106 metadata is
not a wake prerequisite; 113 retains its sole existing Muse Phase0. Package F
cannot be silently omitted because an initial native canary passed.

This brief supplies no separate attack-surface catalog/table. The coverage table
above is a requirement traceability map, not a claim of killed mutants or
executable coverage. Implementation handoffs MUST map each accepted surface to
actual named tests and narrowing controls, preserve reviewer tests under their
original names, declare uncovered/out-of-contract rows with exact acceptance
clauses, and bound any rework to its assigned module. Absence of a table must not
be used to waive a relevant requirement.

At this authoring stage all native, fault, soak, mutation, adoption and landing
evidence is **unrun/pending**. Required exact-head independent review, Meta
review, legitimate hosted checks, signed controlled-window landing and installed
artifact verification occur later under the integrator. No candidate document,
offline fixture or producer terminal result constitutes those approvals.

Before publishing these two docs, run the contract's structural privacy self-check
and inspect role-custody wording. Isolated scanner controls MUST include a fleet
agent-instance ID, private board locator, host path and native session identifier,
plus a role-only/public-source positive. Reject the planted category for the
intended reason; a scanner crash does not pass. Record the patterns and exits
privately. Passing detects only covered formats; it does not prove absence of
all private identifiers or authorize publication without exact-candidate review.
