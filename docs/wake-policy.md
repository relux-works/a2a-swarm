# Wake policy proposal WP-1

Version: **WP-1, proposal revision 1, 2026-10-27**. Status: **PROPOSED / NOT ACTIVE**.
This is one versioned specification and migration contract for [issue 88](https://github.com/relux-works/a2a-swarm/issues/88),
based on the accepted v2 design and controlling Curator disposition. It is neither
shared-protocol ratification nor implementation, host activation or qualification.
The existing [protocol](protocol.md), [CLI contract](cli.md) and
[advice process](advice-process.md) remain in force. The proposed protocol appendix
is a reference, not a replacement for existing rules.

“MUST” below specifies the proposed implementation acceptance contract. It does
not assert that current code provides the behavior. Accepted input requirements,
newer attributed owner/lead deltas and proposed choices are identified separately.
A change to this document needs a new proposal revision and exact-head review;
policy revisions on deployed identities are a different record.

## Sources and precedence

The source and lead supplied authenticated input custody; original v2/final bodies
and the complete advice thread were read. Issue comments are attributed owner
requirements supplied through that custody, not a fresh producer Issues API read.
No private input filesystem paths or session metadata are part of this proposal.

| Ref | Source and scope |
| --- | --- |
| S1 | Curator final, `ivmbp_curator_orch`, 2026-10-25, #lobby / agent-wake-tiers, `$y1EoJmNB9mY45GH7Z_8UpDy4qc0sX47g7hnfKtIY2DQ`: controlling v2 disposition, not two-operator protocol adoption. |
| S2 | Consolidated v2, `ivmbp_skill-project-management_orch-validation-fixes`, 2026-10-25, same topic, `$Icuyovlb8xG1GtZqx1bDrndQnmKURy3soEH54KhvogE`; S1 wins where they differ. |
| S3 | Trim correction, `agents1_a2a_orch`, `$CRjLl4xVIdG-Sk-5ap8DnUPMWjRfZILClVHln2_eyjw`; accepted by validation-fixes `$JX-wTfN8Q1UdsRVN5XwFqNl0oODil27pzLOz45cODyI` and Curator `$-uC2XLFBg9ifq0fjVary2lHm9YUTdTXEajSA8lCAJHs`. PR76 already defines recorded trims; only parked-lane lifecycle is new. Source qualification `$VZDoP7rJglzBRb0Xh5p5PgRSzfeZweEqhxIQxQVT3Ig`: idle is not implicit park. |
| S4 | Advice, #lobby / agent-wake-tiers: agents1 `$6FuuonZezAj7tGcub5xt6Qyn4ABh53_1yIw57I1VRR0`, Meta `$CjQ-je8qioRy5mhza4rhwKDFbk1QruiRQhpfR4nVQwo` and `$sRipAgs2NEPUTv1oG9fhrZG7qGVt-wq_dxor5xzRIQQ`, source `$f8RW3awvGdDC_pWlCkq95uzEUswiSDNKCcu2Mhj1XvA`. Authenticated audience grants, nonclosing replies, retained mail and actual adapter limits remain required. |
| S5 | [Ivan requirements](https://github.com/relux-works/a2a-swarm/issues/88#issuecomment-6430072885), 2026-10-27, source/lead supplied: explicit thread subscription and self-unsubscribe; published importance/posting guide; guaranteed operator DM for human or agent, busy delivery at next idle. Accepted lead amendment: routine addressed notice/ack/handover quiet; owner-needed and addressed/mentioned question/request/proposal/decision/answer actionable; opt-in first, default change through rule 27. |
| S6 | [Lineage comment](https://github.com/relux-works/a2a-swarm/issues/88#issuecomment-6430086540) and tb-keeper pointer `$CBNYjQAx_3-dupAPH1wXtCalKfzgX-FmPLKnTTKSD7k`, #lobby / a2a-rollout-ivmbp: v2 is input, not a replacement-design commission. Parked source TASK-261025-kkh2bo — agent-wake-policy-spec-and-migration, STORY-261023-2ygl8v; implementation STORY-261025-8a3gqw — implement-wake-tier-policy-per-accepted-m1max-spec stays separately admitted after accepted spec. |
| S7 | [Field observation](https://github.com/relux-works/a2a-swarm/issues/88#issuecomment-6434134124), keeper report supplied by source/lead, 2026-10-27: approximately 40 minutes pending, unreproduced and undiagnosed; see [qualification evidence](#qualification-evidence). |
| S8 | Lead commission, #rise-platform-build / platform-iteration-1, `$3UP1dkTG00eScroXo-XRPqyVVfPjY_9PN2Xt57YCcPs`; custody handover `$go6Lj17mpeA1iA8btFSWqdWIBMwjN0d_jyNRVq72QFs`. Source #lobby / join-ivmbp-a2a-source-orch `$PUgrcnjM4jflXoUq42QxFQVvAICOU67v-tQDPnYMBYo` and agents1 `$dd8YIxmNNL67_TV2_TZeFDtfpXpR6XcNUyKpBG0xzak` admit only this proposal and appended reference. Commission/custody references are attributed lead provenance; both original path answers were read. Producer brief #agents1-niobe / wake-policy-88 `$EAmoUJ5e2ssdsuJ5MpJq2AO5KFOLA6TFqg-RFBBwpNo` explicitly carries owner/pinned-principal coverage. |

S1/S2 remain the design foundation. S3 corrects the trim mechanism. S5 is a
newer owner/lead delta, not evidence of shared default acceptance. Details marked
**P** are necessary proposed choices awaiting review; they are not accepted
numeric defaults, new CLI syntax or a grant to activate. Other daemon/E2EE/mirror
work mentioned in the advice thread remains separate; its design acceptance is
not implementation admission for this policy.

## WP-01: Policy, facts and authority

One logical identity has one effective policy and one cursor-owning consumer.
Adapters supply authenticated facts; the policy evaluates them. The conceptual
policy contains v2's `{direct, owner_channel, correlated, alert_audiences,
subjects, kinds, trims}`, plus explicit thread selectors, operator/principal
bindings, finite bounds and recovery state required by S5. This is a semantic
model, not a new live API or configuration file format.

Every classification and wake/suppress/dispatch result MUST record the policy
revision, immutable event identity, binding generation, matching reason(s), selected
disposition and, when suppressed, the exact gate and next review/due time.
Configuration changes require an authorized actor, old/new revision, reason,
scope, source decision and preserved trim/obligation state. A label never edits
policy. Adapters cannot grant themselves operator, principal or publisher rights.

Distinguish these facts:

- **Carrier sender:** the actual authenticated transport identity and assurance,
  including connection/server scope. Body `from`, display names, role names,
  mentions and copied headers are not authenticated identities.
- **Human owner / pinned principal:** an exact host-configured identity mapping
  with allowed scope and provenance. Preserve authenticated ordinary untyped
  owner input in joined rooms and valid owner DM. The commissioned principal
  requirement extends top-tier coverage to exact pinned principals within their
  configured scopes; it does not promote every human, room member or agent.
- **Session operator:** the exact configured controlling human **or agent** for
  this session/generation, authenticated through an admitted direct channel.
  An agent saying “I am your operator” is ordinary data. A pinned principal is
  not automatically this session's operator; an operator agent is not made a
  human owner or added to an owner-DM audience.
- **Authority:** permission to cause a wake is separate from permission to execute
  message contents, change settings, spend, land or override another operator.
  Existing owner/session permissions and rules 13/14/24/27 still apply.

Missing/ambiguous sender, pin, scope, operator binding or direct-channel proof
MUST NOT earn protected priority. Preserve available event/uncertainty evidence,
expose `unverified-binding` or `unverified-sender`, and request authorized recovery;
do not guess from a name. **P:** principal/operator/grant tables are protected
host-managed inputs, read-only to the policy worker, with revision-bound reload.
Same-UID credentials or writable configuration do not prove identity isolation.
Future signed envelopes must preserve these checks against the verified signer,
not replace audience authorization with a signature-present test.

## WP-02: Importance taxonomy and posting guide

The following is the proposed opt-in taxonomy. **Eligibility never means execution
authority.** All readable events are journalled under WP-07 regardless of wake
classification. Stronger protected reasons win over quiet scheduling and opt-out.

| Match | Tier / observable disposition | How to publish or configure |
| --- | --- | --- |
| Authenticated configured session-operator DM, any kind or untyped | Protected T1; durable guaranteed next-idle obligation, WP-03 | Use the admitted direct channel from the exact bound operator identity. A room named DM or a copied operator label is insufficient. |
| Exact authenticated human owner / scoped pinned principal, including ordinary untyped input in joined rooms; valid owner DM | Protected T1; wake-eligible, retain on safety gates | No tag, kind, mention or active new policy is required for existing pinned-owner coverage. Principal scope/channel must match the protected configuration. |
| Relevant reply to own open request/question/handover or explicitly owned finite-window obligation | Protected T1; retain until authorized closure/transfer/expiry handling, WP-04 | Cite immutable `re` or thread root and outstanding obligation; supply an authorized relevant answer, decision or handover. Routine-looking receipt may be essential here. |
| `owner-needed` in the identity's configured readable responsibility/subscription scope | T1 actionable | State evidence, what was tried, exact missing input and who waits in the owning project room. It is not a way to manufacture owner identity. |
| Addressed/mentioned `question`, `request`, `proposal`, `decision`, `answer` | T1 actionable | Address exact addressees or real identity mentions. Questions/advice openers keep explicit reply-by and blocking/non-blocking metadata under rule 32. |
| `system` with incident/impact-start/blocker/global-stop, valid audience, owner, expiry and publisher grant | T2 wake-eligible, WP-05 | Use structured class/scope, not an “urgent” body. Publish only in authorized scope; no new `alert` kind. |
| Explicit structured subject/kind watch, optionally restricted to a thread | T3 opt-in | Use a registered structured subject; specify exact or explicit segment-prefix match and finite watch expiry. No automatic historical-thread follow. |
| Other addressed agent `notice`, `ack`, `handover`, without a stronger reason above | Quiet queue and bounded digest, WP-06 | Do not relabel a routine ACK as a decision/request just to ring a model. Add real obligation correlation when a receipt is required. |
| Unmatched/unaddressed routine traffic or passive thread history | Journal/readable under retention contract; optional explicit quiet watch | Posting, reading or replying in a thread creates no implicit follow or permanent wake interest. |

A sender SHOULD choose the kind for the actual semantic act and the smallest
correct audience, name the subject in the structured header, and state the owner
and closing condition for an alert or obligation. A receipt is not a terminal
answer. A transfer is not a discharge. Publishers cannot upgrade priority by body
keywords, emoji, self-declared roles, fabricated principal labels or broad audience.
Unknown subjects warn; important untagged traffic still uses addressing/owner/
correlation paths. Important finite-window work must carry its deadline and actual
required receipt, not rely on an accidental thread-follow or routine digest timing.

Reconciliation with v2 is explicit: v2 direct matches are still recorded; S5 changes
the scheduling of ordinary addressed agent notice/ack/handover to quiet. It does
not quiet owner/principal/operator input, valid alerts or correlated obligations.
A terminal correlated handover can therefore ring and close; an unrelated handover
is quiet; a transfer rings for the outstanding obligation but keeps it open.
T3 subjects do not turn routine notice/ack/handover into per-message wakes. The
joint PR76 baseline is a separate compatibility layer, WP-09; this taxonomy does
not silently replace it for any identity that has not admitted the replacement.

## WP-03: Operator direct-message guarantee

S5 requires an authenticated DM from the session's configured OPERATOR, human or
agent, to be delivered and wake the session at its **next idle point**, never
lost, muted or digested away. This is a durable service obligation, stronger than
ordinary v2 wake eligibility. Busy means queue until idle; it never means drop,
receipt, downgrade to digest, or require a second arrival to retry. Quiet/thread
opt-out cannot remove it. The binding snapshot and operator authority at intake
are retained for recovery; revocation or session replacement requires an explicit
successor/recovery disposition, not silent deletion or blind cross-session send.

A qualified next idle point has an extant trusted bound consumer, a safe admitted
runtime, no human draft/manual pause, and available budget. **Busy is temporary
retention, not an operator-message suppression policy.** No forced input while
busy or while a draft exists. At the first safe idle opportunity the operator
obligation takes precedence over digest and ordinary backlog, using the admitted
single consumer. Multiple pending DMs can share one count notice but every event
remains individually readable and pending until explicit receipt.

Draft/pause/trust/budget/binding absence must be reported as blocked with pending
age and reason; missing/dead consumers produce visible recovery/wake-loss state
and keeper/owner escalation, not an assertion of successful delivery. A permanently
paused, untrusted, dead or exhausted session cannot be promised physical dispatch.
The guarantee is retained delivery plus next safe idle service when the admitted
conditions exist; no elapsed timeout cancels the obligation. Runtime acceptance,
mail receipt and an operator receiving an answer are separate evidence.

**Admission dependencies, not solved implementation:**

- D1: owner-DM/mirror remains owner-only, default-off under its existing contract.
  A non-owner principal/operator DM requires a separately specified and admitted
  authenticated inbound direct route, operator-to-session binding and lifecycle.
  It must not be implemented by aliasing owner DM, widening mirror audience or
  treating agent DM membership as owner input. No such route is delivered here.
- D2: current [30-second notification spacing](cli.md#shared-30-second-notification-spacing)
  has no urgent-message bypass. It may defer an otherwise idle session. WP-1 does
  not redefine idle to hide cooldown, weaken the next-idle requirement, or silently
  bypass that contract. Implementation admission must demonstrate a compatible
  next-idle delivery path (including already outstanding notices where safe), or
  obtain an explicit scoped scheduling amendment through the source/shared gates.
  Until D1/D2 are resolved and qualified, literal operator-DM service is unimplemented;
  a quiet-policy pilot cannot advertise it as guaranteed on that host/runtime.

## WP-04: Explicit subscriptions and correlated obligations

Thread interest is explicit and self-managed (S5). Posting, reading, being copied
or replying does not auto-follow a thread. An identity may unsubscribe its own
noisy optional thread watch; record selector, reason and policy revision. **P:**
a thread selector alone is a passive read/digest watch, not a wake grant. It can
narrow an explicit T3 subject/kind watch; following another thread never creates
T1 eligibility. Other participants may discover and subscribe without acquiring
execution or publisher authority. No automatic re-follow on restart or preset sync.

Self-unsubscribe/mute stops that optional interest prospectively. It cannot erase
journal/mail, receipt an event, trim mandatory owner/operator coverage, or cancel
an existing request, review, delivery or finite-window receipt obligation. If the
identity cannot carry an obligation it must explicitly hand it to an authorized
successor, with the successor acceptance recorded. Optional thread unsubscribe
is distinct from a prohibited raw unsubscribe of the joint baseline.

For an own `request`, `question` or `handover`, retain a watch keyed by immutable
request ID, actual thread root, accountable identity, authorized responders/aliases,
required scope/closing condition, creation and expiry. `re` or the actual thread
root must identify that own event; matching topic title, body quote or an arbitrary
reply-to-someone-else is insufficient. Cross-thread response needs explicit
validated correlation; carrier sender and responder authority are both checked.

| Input/state | Required obligation result |
| --- | --- |
| Authorized relevant terminal answer/decision; terminal handover that actually discharges the defined obligation | Record closure with actor, source event and reason. Preserve mail until receipt. |
| Unrelated/unauthorized reply, partial answer, arbitrary event of a closing-looking kind | No closure; retain watch and deadline. Same kind is not proof of semantics or authority. |
| Handover transfers responsibility rather than discharges work | Carry correlation, scope, deadline and responsibility to an accepted successor; do not mark fulfilled. Until acceptance the current owner remains accountable. |
| Watch expiry (accepted v2 default 72h) | Emit one unresolved-obligation notice with recovery owner/deadline; retain unresolved debt and relevant mail. No silent completion or permanent abandoned watch. |
| Required current window ACK/receipt, including ACK to a decision | Keep finite-window obligation actionable despite routine-ACK quieting; receipt must be verified for that specific window/head/token. A late or mismatched ACK does not satisfy it. |

**P:** semantic closure is evaluated from an explicit outstanding contract and
verified response, not a new free-text “closed” flag. Ambiguous classification
keeps the obligation open with `closure-unknown`. Generic `until-kind` subscription
expiry cannot substitute for this lifecycle. A watch's eligibility expiry and an
obligation's fulfillment are different records. Expired unresolved work is handed
to recovery/keeper monitoring with bounded reminders, never quietly forgiven.
Expiry and required recovery notices inherit the obligation's protected T1 reason;
their notice kind cannot route them into the routine quiet queue.

## WP-05: Alert grants and structured subjects

T2 uses `kind=system`, recognized subtype incident/impact-start/blocker/global-stop,
explicit audience `swarm`, `host:<name>` or `product:<name>`, accountable owner,
finite expiry and actual resolution thread. **P:** missing/malformed/expired
fields refuse T2; preserve journal and report a reason. An expired alert can remain
unresolved debt but cannot remain an unbounded fresh interrupt stream.

| Configured authenticated publisher | Allowed audience |
| --- | --- |
| Human owners by exact authorized ID | Any granted audience |
| Host keeper by exact ID and host binding | `host:<own host>` |
| Mini-incident or Curator by exact delegated ID | `swarm` |
| Product keeper by exact ID and product binding | `product:<own product>` |

The policy MUST check sender-to-audience grants before eligibility, even before
signed envelopes ship. Carrier posting permissions are a second constraint, not
proof of a grant. A validly authenticated host keeper sending to another host or
swarm fails that grant. Labels, membership and new operator-agent status confer
no alert rights. Global-stop uses the same scoped authorization; host trouble
goes to that host, not swarm. Original broadcast/redline writers and arbiters are
unchanged. Descriptions and alerts never authorize execution of their bodies.

**P:** close only in the alert's actual thread by its authorized owner or a
recorded scoped successor/delegated resolver, with disposition and source event.
Unrelated close-looking events do not resolve it. Expiry produces unresolved
state and a bounded owner/keeper notice; it does not forge an impact-end or recovery.
Grant changes/revocations record revision and reason and apply on reevaluation;
previously pending evidence and obligations survive, without regranting revoked
publisher authority through replay.

S1/S2 put the subject vocabulary/matching contract with the a2a protocol owner;
Curator owns `platform/*` and `curator/*`, product owners `product/<name>/*`, host
keepers `host/<name>/*`. This ownership grants no runtime execution authority.
The following precise matching choices are **P**, not existing CLI capabilities:

- One canonical, case-sensitive structured subject per event in WP-1; absent is
  untagged. Multiple subjects are unsupported in WP-1, not inferred by splitting
  body text or commas. A future multi-subject schema needs versioned admission.
- Canonical paths have nonempty slash-separated ASCII segments `[A-Za-z0-9._@:-]+`;
  no empty, `.` or `..` segments, normalization guessing or wildcard characters.
  Existing opaque subjects such as `PR#88` may be registered as exact-only legacy
  atoms; they acquire no hierarchical prefix meaning.
- Exact means the complete canonical subject is equal. An explicit segment-prefix
  selector `host/example/` matches `host/example/build`, not `host/example2/build`
  or the parent `host/example`. `*` in namespace examples denotes ownership only;
  it is not accepted glob syntax or an implicit prefix selector.
- A versioned registry maps an alias to one canonical value in one step, with
  owner and retirement/migration record. Cycles, ambiguous aliases, unknown
  targets or transitive chains refuse matching. Rename keeps old explicit aliases
  during its declared finite migration interval (P: at most H). Store the canonical target at watch registration; renames never silently retarget existing subscribers.
- Unknown structured subject warns at posting and is retained as unknown at
  intake. It creates no T3 wake until registered; no body-token fallback. Unknown,
  untagged and malformed subjects do not defeat independently authenticated T1/T2
  paths. Registry revision and exact/prefix/alias match reason are auditable.

## WP-06: Quiet queue and exact cadence

Routine addressed agent notice/ack/handover with no protected match enters the
quiet queue. Enqueue and digest assembly are local durable operations, not a
model turn. A digest contains immutable event references, counts/kinds, first and
last pending times and overflow/recovery indicators; peer bodies are read later
through the normal untrusted-content boundary. Digest publication is not receipt,
closure or proof of runtime wake. A crash after digest assembly does not lose the
constituent events; a repeated digest key does not create a new obligation.

Cadence, wake retry, wake-loss, retention and recovery need explicit finite values.
The following **P reference profile** is a candidate for review, not accepted
numbers or implicit live defaults. Every value must be materialized in the admitted
per-identity policy; omission/invalid values refuse new-policy activation and
preserve the prior admitted policy. No “zero means forever,” automatic budget or
unreviewed CLI flags. The accepted 72h watch expiry and existing 30s notification
spacing are separately sourced and not the origin of these proposed timings.
The candidate five-minute batch and bounded item/byte lists limit routine model
interruptions; finite retry/replay episodes expose failure rather than spinning,
and the retention/cap proposal makes storage pressure explicit. These values are
review choices, not measured service levels or a newly approved storage quota.

| Parameter | P reference value and exact meaning |
| --- | --- |
| Digest interval D | 300s; first event fixes due=`first_queued_at+D`; later arrivals never slide it. |
| Digest effect bound | At most one accepted or uncertainty-reserved digest wake effect per D, maximum 64 references /16KiB per digest; oldest first, remaining IDs retained. More chunks wait another D, not one turn per event. |
| Retry | Initial due attempt counts as attempt 1; after definitive safe refusal, retry after 1s, then 2s, 4s, 8s, 16s and 30s capped delays. Maximum 10 total attempts within 300s per episode. Gate changes permit reassessment, never reset these counters/backoff or start another consumer. Ambiguous effects require reconciliation, not blind resend. |
| Wake-loss | Failed/unknown dispatch reports immediately; eligible-but-undispatched pending age 120s raises a deduplicated wake-loss case. Busy/draft/pause/budget/absent-binding are separately visible blocked reasons, not automatically successful dispatch or a false busy-bypass trigger. |
| Blocked/recovery reminder | At most one repeated notice per open case per 3600s; initial failure/escalation not delayed. Track total age and eligible age separately. |
| Alert / optional thread / subject-watch expiry | Explicit future deadline, at most 72h per registration; renewal by authorized actor recorded. Correlated watch retains accepted 72h default, explicitly materialized; reply-by/window deadlines may be earlier. |
| Journal/decision retention | Retain journal/digest/reason entries for 7days after both receipt and obligation closure, then compact within 24h when the replay horizon permits. Unresolved or unreceived records are not time-pruned. |
| Storage | Candidate cap 256MiB across journal/mail/obligation/receipt/digest state or 100000 intake events per identity, whichever reached first; backpressure/recovery before cursor advance, no eviction of pending operator mail/obligations to fit. Capacity exhaustion cannot promise indefinite ingestion. |
| Replay horizon H / recovery episode | H=7days lookback from first durable intake time for automatic reevaluation; at most 100 pages, 10000 events and 60s per episode, stop at first bound. Older outstanding mail/debt remains retained but needs explicit recovery, not silent expiry. Incomplete coverage becomes recovery-needed with stored gap/cursor; resume in a new bounded episode at most once per 3600s unless an authorized recovery actor explicitly requests one; all budget gates still apply. |
| Clock discontinuity | Compare monotonic time within one process; persist UTC deadlines. A detected clock discontinuity or uncertain deadline ordering across restart enters clock-unknown, retaining fixed deadlines and mail until bounded reconciliation, never expiring debt merely to make a timer pass. This does not claim all wall-clock jumps are detectable. |

For a nonempty quiet queue, at due the sole consumer may offer one coalesced digest
wake, subject to all safety gates and the existing notification spacing. A blocked
due digest remains due without new arrivals; on the next admissible opportunity
it sends once, sets the next digest opportunity to effect completion+D, and retains
all unreceived events. Uncertain effects reserve D from intent time and still need
reconciliation; definitive no-output refusals may retry the same digest key under
the attempt bound and consume no model turn. These retries do not reset first due.
All watch/alert deadlines use explicit UTC; a clock-unknown interval is retained
for reconciliation rather than expiring an obligation from an untrusted timestamp. Empty queues cause no periodic model turn. A consumer crash
cannot reset the fixed due time, retry count, spacing reservation or digest key.
An operator DM uses WP-03, never this D cadence. A subject or kind watch cannot
turn a storm of routine ACKs into a model turn for each ACK.

Total pending age starts at durable intake. Eligible age counts only time a wake
is due and all admitted dispatch gates allow service; quiet time before D and
reported busy/draft/pause periods do not count as missed eligible service. A known
dead/missing consumer is an immediate recovery failure, not eligible-age success.
Suppression and reason transitions remain visible against total age.

The policy does not alter outbound operator-status cadence, owner-DM Stop mirror
budgets or [notification spacing](cli.md#shared-30-second-notification-spacing).
Retry exhaustion stops automatic attempts for that episode, exposes recovery-needed
and escalates; it never receipts or loses pending work. Wake-loss cases target the
host keeper and the owner DM under v2, using admitted authenticated routes; failed
alert delivery remains durable/visible, not recursively spawning model alerts.
Until the daemon is admitted, the existing keeper liveness sweep remains the
operational fallback. The profile's retry/retention bounds are design proposals,
not a statement that the current binary implements these mechanisms.

## WP-07: Durable intake, wake and receipt boundaries

The required future order is:

1. Authenticate/normalize carrier facts and persist **every event the identity may
   read** in the intake journal before relevance. “Every” means the admitted
   readable stream from the recorded cursor plus declared bounded backfill, not
   inaccessible rooms or an unsupported claim of unlimited server history.
   Undecryptable input retains an explicit metadata/uncertainty record, not a claim
   to have read plaintext. Invalid/untrusted bodies do not earn authority.
2. Under one serialized identity consumer, evaluate the policy revision; durably
   persist relevant mail, quiet/digest references, pending operator service and
   correlated/alert obligations before advancing the cursor. Any failed durable
   write leaves the cursor unadvanced. An event irrelevant now still has a journal
   record for declared replay; its absence from wake mail is not silent intake loss.
3. Schedule wake eligibility with reason, due time and retained gate state. Wake
   intent/reservation precedes the external effect. No second consumer or model
   polling loop is introduced. A notification contains fixed trusted counts/context
   pointers, not an instruction assembled from an untrusted peer body.
4. Reassess busy/draft/manual pause/binding generation/trust/budget at dispatch.
   Record definitive refusal versus accepted versus uncertain effect separately.
   Native acceptance or a terminal notice is not evidence the model read its mail.
   No reservation or notification counts as receipt or obligation closure.
5. Receipt only the exact event IDs explicitly delivered/read via the existing
   inbox/ack contract. Keep receipt tombstones for the declared replay horizon.
   An inbox receipt still does not prove an answer, fulfillment or human acceptance.

**P:** idempotency keys bind connection, authenticated identity, immutable carrier
event ID, obligation/digest key and relevant generation; reused bodies/new IDs do
not silently collapse genuine separate obligations. Duplicate ingestion is safe;
replay cannot redeliver a known receipted ID. Persist classification revision and
reevaluation provenance, not rewrite old decisions as if they happened today.
Restart retains pending mail, retries, digest due, receipts, gap markers and
obligations. Failed writes, crash after journal/before mail, crash after mail/before
cursor, and crash after effect/before confirmation require explicit reconciliation.

Unknown effect/receipt state is retained as unknown. Recovery must establish
complete relevant coverage before a resend that might duplicate an accepted
effect. A limited sync without the echo, an expired transaction window or missing
history is not proof of no send. Backfill only within the finite recovery bounds;
otherwise stop with recovery-needed, no blind resend, no forged receipt/closure.
No end-to-end exactly-once delivery is claimed across carrier expiration or
irrecoverable gaps. Same-UID tampering and unavailable history remain explicit
assurance limits, not repaired by policy prose.

Unreceived mail and unresolved obligations MUST survive retention cutoffs. Under
storage pressure, stop intake/cursor advance, retain debt, surface capacity and
history-risk to the keeper, and require explicit recovery/custody. Source/carrier
retention may still make unread history unavailable: record the gap, never call
it success. Closed/receipted history pruning is allowed only after the declared
retention and replay horizon, with compact tombstones/checkpoints sufficient for
the still-permitted replay; replay outside that proven horizon refuses rather
than treating forgotten IDs as fresh.

## WP-08: Policy revision and recovery contract

**P:** admitting a policy revision is an auditable compare-and-apply transition:
validate the full snapshot, authority, dependencies and finite bounds, durably
record it, then make it effective at a known journal/cursor boundary. On failure
keep the prior revision active and visible; do not partially adopt tiers. A stale
writer/replay cannot restore a revoked grant or clobber independent manual trims.
Changing connection/binding requires explicit migration, not copying cursors or
operator pins between identities. No history is receipted by a policy upgrade.

Replay names source and target policy revisions, cursor/event interval, retained
receipt set, reason and authorized recovery actor. It reevaluates only retained
unreceived events within the declared horizon and the current authenticated grants;
recovered open obligations remain open. Replaying an old quiet event into a new
important class schedules it once with provenance; a new rule cannot manufacture
history that was never journalled. Baseline clients that skipped unmatched events
cannot certify complete replay without actual bounded backfill and recorded gaps.
Downgrade/rollback carries all pending mail and debt into a compatible consumer or
stops with explicit custody/recovery; it never deletes new state to appease an old
schema. No model is asked to sit in a wait loop to accomplish this recovery.

## WP-09: Migration, admission and ownership

1. **Preserve active baseline.** PR76 joint ordinary-room system/owner-needed/
   decision coverage remains the default for every role until an admitted
   replacement. Recorded per-identity trims use the existing single mechanism,
   visible in `subs check`. Recording a trim does not itself rewrite server
   subscriptions; changing selectors, recording intent, syncing and checking
   effective coverage are separate version-qualified steps. No raw baseline
   unsubscribe, broad forced sync or silent reversal. No action is performed here.
2. **Preserve park lifecycle.** Park adds a separately attributable profile trim;
   unpark removes only that trim and restores the applicable effective coverage.
   Independent manual trims survive both transitions. Idle detection is not park
   and grants no implicit new trim/default. Mandatory direct owner/operator and
   open-obligation coverage cannot be trimmed away.
3. **Review one specification.** Source custodian obtains consumer/Meta advice,
   both operators' decisions under rule 27 for shared scope, and exact-head review.
   A source-code/docs landing alone does not enact shared policy. Existing repair,
   writer/rollout and window holds remain; this proposal freezes no separately
   admitted non-overlapping diagnostics. Original lineage stays intact; Niobe's
   delegated proposal does not wake the parked source producer or open a duplicate
   implementation lane.
4. **Admit opt-in implementation separately.** Resolve D1/D2 and approve the
   finite profile/matching/closure choices, schema, authentic bindings and concrete
   supported carrier/runtime slices before relying on their guarantees. Run the
   paired WP-10 fixtures in those admitted slices, with explicit unknown/skipped
   cases. No new CLI API is specified by the conceptual fields in this document.
5. **Qualify and opt in explicitly.** Record identity/host, exact installed build,
   active baseline/trim snapshot, proposed/effective policy revisions, bounds,
   consumer/binding/trust evidence, finite replay plan and rollback/custody owner.
   Prove no gaps or receipt loss across cutover. A pilot is individually authorized
   and stays opt-in; no profile install, hook, principal table or room membership
   is changed by merging this proposal.
6. **Shared defaults remain a later decision.** Ivan's importance-default goal
   is retained as a proposed replacement target, not silently substituted for
   PR76. Any common default/adoption requires both operators through rule 27 in
   their authorized decision routes. Source custodian owns later landing and
   source/live custody gates; implementation remains the agents1 lineage after
   accepted spec. A PR approval is not operator adoption or host rollout.

Open admission items are D1 authenticated principal/operator DM route; D2 next-idle
versus current 30s spacing; approval of all P matching/timer/storage/recovery choices;
and shared operator dispositions. These belong in this spec/PR's decision record,
not hidden as runtime flags or a second implementation. They do not block drafting
this proposal, but block a claim that its guarantees are implemented or active.

## WP-10: Documentary acceptance matrix

These are named **SPEC FIXTURES / UNRUN**, not executable tests, killed mutants or
current platform coverage. For each future supported carrier/runtime mapping,
record actual authenticated facts and assurance differences; an unsupported route
must be reported as unqualified, not passed through a mock. Each row pairs a
required positive with a neighboring forbidden case and a visible invariant.
Use the explicitly admitted finite profile, not implicit test-only defaults.

| Fixture pair | Positive disposition | Negative neighbor / retained invariant and limit |
| --- | --- | --- |
| F01 OperatorNextIdle / SpoofedOperator | Bound authenticated human, then separately bound agent operator DM: durable priority, busy queued, serviced at next safe idle without a second arrival. | Copied operator body/role, unbound principal, wrong session/generation or false-DM room gains no operator priority. Mail/uncertainty retained, no forged binding; D1/D2 must first be resolved. |
| F02 UntypedOwnerPrincipal / LookalikePrincipal | Exact pinned owner untyped joined-room message and valid owner DM; exact separately pinned scoped principal: protected coverage without kind/tag/to. | Display-name/alias/body-owner claim, wrong carrier identity or out-of-scope principal cannot gain protected status. Existing owner-DM audience unchanged, no mirror route invented. |
| F03 EligibleIdleService / LostWake | Known relevant event, qualified idle consumer, no draft, trusted binding and budget: fixed notice, resumed inbox for exact ID, reply with no manual ring. | Idle remains eligible but no dispatch by configured bound: durable pending plus wake-loss case to keeper/owner, no successful receipt claim. Proves only observed interval/build/mapping. |
| F04 CrashRestart / AmbiguousEffect | Crash after journal, after mail-before-cursor, and after intent: replay/reconcile preserves one logical event and debt; confirmed unsent effect can retry. | Crash after uncertain effect or limited-sync hidden echo never causes blind resend, lost mail or fabricated receipt. Exhausted bound becomes recovery-needed, not success. |
| F05 StormDigest / PerNoticeTurn | Many ordinary addressed notices/ACKs/handovers: journal all, first due fixed, bounded aggregated references, at most one accepted/uncertainty-reserved digest wake effect per D; definite no-output retries obey the separate attempt bound. | Sliding deadline starvation, overflow eviction, extra turn per event or treating digest as receipt fails. An interleaved operator DM is not delayed to D. |
| F06 BusyDraft / ForcedWake | Busy followed by safe idle: pending priority survives; real draft/manual pause/binding/trust/budget gate retained and reason visible. | No typing into nonempty/unknown draft, busy bypass, automatic trust change or drop on budget exhaustion. Permanent gate remains blocked, never reported delivered. |
| F07 ScopedAlert / WrongAudience | Authenticated keeper publishes valid host:own alert, owner/expiry/thread present; records T2 reason. | Same authenticated keeper to another host or swarm, spoofed keeper or self-labelled global-stop fails T2; normal independent reasons remain possible, no execution authority. |
| F08 ClosingReply / NonClosingReply | Relevant authorized terminal answer/decision/discharging handover closes exactly the defined obligation. | Unrelated, unauthorized, partial, title-only correlation or transfer handover leaves debt open; accepted transfer carries successor/deadline. No arbitrary until-kind closure. |
| F09 WatchExpiry / SilentExpiry | At recorded 72h or earlier declared deadline, unresolved watch produces one notice and recovery ownership; true closure before expiry stops that notice. | Expiry cannot receipt mail, fulfill work or discard a missing required window ACK. Repeated recovery notices respect configured bound. |
| F10 ExplicitOptOut / AutoFollow | Explicit optional thread/T3 watch can be removed by its identity and stays removed after restart; unaddressed historical reply alone never wakes. | Posting in thread cannot auto-subscribe; unsubscribe/mute cannot erase existing operator/open-request/window obligation or raw-trim PR76 coverage. |
| F11 SubjectHeader / BodyToken | Registered exact, explicit segment-prefix and one-step alias matches carry registry revision; untagged addressed action still eligible. | Body-only token, near-prefix, unknown/malformed tag, alias cycle or wildcard creates no T3 wake; owner/direct paths remain intact. Single-subject limit explicit. |
| F12 ParkUnparkTrim / LostManualTrim | Joint baseline retained; explicit park adds role trim and unpark removes only it; manual trim and reason survive sync/replay/restart. | Idle is not park; raw unsubscribe, silent default change or stale revision restoring removed grants fails. Effective selector audit separate from recorded intent. |
| F13 WindowReceipt / RoutineAck | Verified ACK for the actual current head/window/lease fulfills only that receipt duty and remains actionable through correlation. | Unrelated routine ACK stays quiet; late/wrong-head/unauthorized ACK cannot open a window, close duty or silently release an obligation. |
| F14 RetentionReplay / CapacityGap | Current revision replay within proven horizon preserves pending IDs/receipts/debt; receipt tombstone prevents duplicate delivery. | Full storage or unavailable backfill fences cursor/replay and raises visible gap/capacity; never prune pending operator mail, reissue expired-history events blindly or claim infinite retention. |

No executable surface table was supplied for this docs-only assignment. This
matrix is documentary behavior coverage, not runtime proof or a substitute for
implementation tests, source review, live authorization or platform qualification.
All executable, crash, carrier, busy/draft and operator-route checks remain UNRUN.

## Qualification evidence

S7 is a reported **unreproduced, undiagnosed approximately 40-minute delay**, not a
source regression or diagnosed PR83 failure. The keeper associated the item with
a `system` subscription; the 04:53:26 UTC scan reported `count=1`, `kinds=other`,
`oldest_min=40`. That discrepancy is unresolved. Around 04:53:40 the prompt appeared
idle/empty; a manual ring around 04:53:50 was followed by work within ten seconds.
The intervention invalidates any claim that the eventual drain proves automatic
wake. There is no complete event ID or pre-ring binding/readiness snapshot.
Later cooldown/armed/no-new-mail/busy observations are post-intervention and prove
nothing about pre-ring readiness. An earlier approval-dialog history or successful
wake is context, not a diagnosed stale-dialog cause. No own notice was observed
in the composer, so the report is not established as the typed-stranded-notice
case. PR92's surfacing fix is not claimed to repair it.

A separately authorized future qualification must capture immutable full event ID,
authenticated sender/selectors/obligation, ingestion and cursor boundary, journal/
mail/receipt times, pending and eligible age, policy revision, exact build/runtime,
binding/consumer generation and contemporaneous readiness, reservation, dispatch
and failure reasons **before** any intervention. Record fixed notification due
and gates separately. Pair missed service with adjacent eligible-idle success and
busy/draft preservation on the same admitted mapping. Manual ring, operator prompt,
reset/restart or another consumer contaminates automatic-wake success; record it.
No live probe, approval-dialog interaction, binding reset/restart or timer-policy
change is commissioned by this document.

## Requirement traceability

| Requirement | Grounding | Contract / documentary witnesses |
| --- | --- | --- |
| R01 T1/T2/T3 and exact owner/principal/operator identity, no text trust | S1/S2/S4; S5 operator delta; S8 commissioned principal coverage | WP-01/02/03/05; F01/02/07 |
| R02 Explicit thread subscriptions, self-opt-out, structured subjects, retained correlation | S1/S2/S4 plus S5 | WP-04/05; F08/09/10/11/13 |
| R03 Quiet routine notices/ACKs/handovers without losing protected priorities | S5 lead amendment reconciled with S1/S2 | WP-02/06; F01/05/08/13 |
| R04 Finite explicit cadence, retry, wake-loss, retention/replay bounds | S1/S2 require migration bounds; S4 separates controls; numeric/matching choices P | WP-06/07/08; F03/04/05/09/14 |
| R05 Journal before relevance, mail/debt before cursor, notification never receipts | S1/S2/S4 | WP-07/08; F03/04/14 |
| R06 Human/agent operator DM next-idle service without safety bypass or owner-mirror alias | S5; S1 safety; separate owner-DM active contract | WP-01/03, D1/D2; F01/02/03/06 |
| R07 Authenticated scoped alert grant/owner/expiry/thread and reason/revision | S1/S2/S4 | WP-01/05/08; F07/09/14 |
| R08 PR76 joint baseline, one trim model, explicit park lifecycle, opt-in/default governance | S1/S2/S3/S5/S6/S8; existing rules 14/24/27 | WP-09; F10/12/14 |
| R09 Named positive/negative neighboring fixtures with retained-obligation observations | S1/S2/S4/S5, this proposal's documentary scope | WP-10, F01–F14; all UNRUN |
| R10 Preserve field uncertainty and define future qualification without live probe | S7 | Qualification evidence; F03/06 |

Local proposal checks use git for exact changed paths, preserved old protocol byte
prefix, signed head and diff whitespace; local text inspection validates Markdown
file/anchor targets and the R01–R10 / F01–F14 cross-references. They prove document
structure only. No Go/build/race/mutant/full suite or live runtime check is part
of this prose-only delivery; implementation acceptance remains separately UNRUN.
