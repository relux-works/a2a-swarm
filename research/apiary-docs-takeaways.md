# apiary-docs: takeaways for a2a-swarm

Source: private repo `relux-works/apiary-docs` (book + paper v3.2 by Alexis Grigoryev and Ivan Oparin), read 2026-09-29.
This is input to weigh, not a decision. Section numbers are the book's chapter.section and the paper's section numbers.

## 1. Sources

**Book ch03 "Primitives everything stands on"** (the requested journal/ordering/fault material lives here)
- 3.1 Replicated journal: identical append-only logs give identical state; the only hard question is agreeing on order,
  either by a sequencer (simple, live, but can stop or lie) or by consensus (no single point, costly). L0/L1 use a sequencer that orders while core nodes only sign.
- 3.2-3.3 Crash vs Byzantine: crash protocols (Raft/Paxos) need a live majority; Byzantine ones need n >= 3f+1 and a 2f+1 quorum.
  A quorum certificate (QC) is verifiable offline. Majority threshold stops a lone decision but not a forked history (3 signers, 1 liar).
- 3.4 Lease + fencing token: a lease alone yields two leaders after a pause/sleep; a monotonically increasing token carried on every action,
  with the executor rejecting anything below the highest token seen, fences the stale holder.
- 3.5-3.6 Hash chain, Merkle tree, inclusion proofs, signed snapshot (root, length, head, version); rollback protection = the client persists
  the highest accepted version as security state (not cache) and rejects older snapshots.
- 3.7 CAS + merge queue: advance a shared ref only if it still equals the expected value; never merge a state that was not built and tested exactly as it lands.

**Book ch05 "Journal and invariants"**
- 5.1 Record: prev hash, type, body or body hashes, Ed25519 signatures over canonical JSON; key pins rotate by overlap, compromised keys removed at once.
- 5.2 Chain/tree/snapshot: h_i = H(h_{i-1} || c(r_i)); Merkle root in a quorum-signed snapshot; history is append-only, no delete or replace.
- 5.4 Order by sequencer, commit by quorum; L1 openly admits no fork-safety against a malicious sequencer; only L2 (BFT) gives both.
- 5.5 Heavy objects are content-addressed with an availability certificate; the journal can materialize as a git task board whose commits the QC certifies ("the journal does not duplicate the board, it certifies it").
- 5.6 Ten invariants, each with "what breaks without it"; relevant here: I3 (integrate exactly what was checked, atomic M->I), I7 (journal is truth, session is cache; stale-lease records rejected but kept as divergence), I8 (cheap stop, no rollback).

**Book ch07 "Orchestrator and task cycle"**
- 7.2 The orchestrator role is a lease record (node, epoch, random fencing token, predecessor, reason, checkpoint, scope), renewed by heartbeat; every orchestrator record carries epoch+token; stale ones are rejected and archived as divergence ("sleeping laptop" example).
- 7.3 Planned handover needs a proof of quiescence; forced takeover is a separate explicit act, always cold (the session is not transferred).
- 7.5 Several leases may run at once with disjoint scopes; cross-scope work belongs to a lease covering the union.
- 7.6.3-7.6.5 Approval binds to the immutable candidate I over head M; if head != M at integration, rebuild and re-review (patch-id transfer of approval was a v2 bug, found by the model).
- 7.7 External effects: operation id + idempotency key; outcome done / rejected / not-established; no blind retry of "not established"; freshness checked at execution time, not only at record time.

**Book ch13 "Open questions"**
- 13.1 Who decides to merge overlapping scopes, and how often it happens: open, to be measured.
- 13.4 Approval transfer between candidates only if the base delta misses both write scope and paths read: sufficiency unproven.
- 13.5 Mechanical check: only the bounded Python model exists; no TLA+-style spec, no extension to leases, stop, or external effects.
- 13.9 Identity: mechanics clear, policy not (pseudonym weight, provider fragility, whether code-host keys equal DNSSEC-class proofs).
- 13.10 Deliberately unsolved: full decentralization without one execution environment, blockchain settlement.

**Paper v3.2 identity layer** (sec. 13 "Independence and reputation", with terms in sec. 2 and record types in sec. 6)
- Long-lived identity key per owner, bound two-way to an external identity: a statement signed by the key plus a provider artifact carrying the key fingerprint.
- Providers are resolvers with one interface (identity string -> fingerprints + evidence hash): DNSSEC TXT, HTTPS well-known, code-host account keys; social posts optional.
- Resolution is nondeterministic, so an identity is confirmed when t validators record the same evidence hash in one window; evidence is dated and pinned (first seen wins; a key change is an event).
- Short-lived node keys are certified by the identity key in a journal record; reputation attaches to the identity. Git revisions are SSH-signed by node keys; allowed signers derive from the journal.
- A proof attests control of a domain/account, not honesty (also stated in sec. 15 threat model).

**paper/model/integration-fragment-model.py** (paper sec. 17)
- Bounded explicit-state BFS over the integration fragment: task A (contract may change, stale epochs), task B (interferer that moves head); <= 2 contract versions, <= 2 epochs, each record delivered <= 2 times.
- Four rules, each switchable: `cas` (head == record base), `epoch` (current lease epoch), `contract` (attestation on current contract), `bind_I` (attestation bound to the exact candidate).
- With all rules: 1090 reachable states, no violation. Dropping any one rule yields a shortest counterexample trace (6 or 11 steps), e.g. without `cas` a redelivered record is applied twice.

## 2. Mapping onto a2a-swarm

**(a) Signed archive vs journal/chain/snapshot.** `a2a archive` is a deterministic, byte-stable markdown export, committed with signed git commits straight to main.
Git already gives a hash chain over *commits* and SSH signatures give attribution, so ch05's chain requirement is roughly met at commit granularity,
and `index.json` (per topic `events`, `last_event`) is a crude snapshot. What is missing is the *append-only* check between exports: nothing verifies that a new
export extends the previous one block by block. A server-side loss (the R10 Mac mini cutover was exactly this risk; it was verified by hand against 0d61966) or an
exporter bug would show up only as deletions buried in a large diff. There is no gap detection (a missing middle event is invisible), no rollback check
(an older export committed over a newer one), and redactions rewrite an existing block in place. Matrix's own event DAG (prev_events, hash-based event ids) is dropped
by the export; restoring it is not worth it, but an append-only check on our own files is cheap.

**(b) Integration windows vs leases and fencing.** Rule 19 windows are informal leases without an expiry or a token: "announced start, expected end, end", extensions by arbiter ruling.
The failure modes ch03.4 describes map directly: an integrator that crashes or sleeps holds everyone's no-push window open indefinitely; an arbiter extension or a second window
for the same main leaves no way to tell a stale integrator from the current one. A lease form would fix both: the window-open message (`--kind system --subtype window --subject <repo>@main`)
carries a hard `until` and the base OID of main; the window's event id is its fencing token; an extension or takeover is a new window message `re=<old id>`, which supersedes the old id;
a window past `until` without an extension is closed by definition, so nobody waits on a dead integrator. The CAS part (ch03.7, the model's `cas`) is already enforced by git: a non-forced push of a
candidate rebased onto M is accepted only while main is still M or its ancestor. Honest limit: fencing needs an executor that rejects stale tokens; GitHub does not know our tokens,
so without a pre-push hook the token is audit-level (the arbiter can prove a landing cited a superseded window), not enforcement. That is acceptable for our crash-fault, cooperative sessions.

**(c) Delivery-v2 vs their ordering/replication.** Our homeserver (tuwunel, federation off) is exactly their L0: one trusted sequencer, crash faults only, no quorum.
Room order is the server's order; the committed cursor (`next_batch`) is a position in that sequence; the pending batch is "delivered, not yet committed"; the acked ring
is the model's "no record applied twice" by id, not by CAS. Two gaps against their model: the ring is bounded (1000), so a cursor rollback deeper than that could redeliver an acked event;
and a lost state file (`~/.config/a2a-swarm/state/<name>.json`) makes the next `wait` take a fresh position, i.e. a silent gap. That is the rollback problem of ch03.6:
the high-water mark is kept as a local cache, not as persisted security state. Their ch07.7 rule (idempotency key + "not established" + no blind retry) is what consumers
of at-least-once delivery need; our docs already require tolerating repeated event ids, which is the same thing keyed by event id. Replication and quorum add nothing for one server.

**(d) Identity layer vs Matrix accounts + admin provisioning.** Our identity is "the homeserver says so": `@name:a2a.local` accounts, token registration for now, then `a2a admin bootstrap/provision`
with registration closed (R10). In their terms the admin is the identity-key holder and each agent account is a certified node key; the operator owning an agent is the external identity,
already a card field (`owner`). Their resolver/proof machinery exists for open, multi-owner swarms with pseudonyms; we have two named operators on one private server, so it does not pay yet.
Two cheap parallels are worth keeping: provisioning should record who provisioned each account for which operator (their "key certification" record), and a key or ownership change
should be an announced event (`--subtype ownership`), not a silent card edit. Revisit proofs (e.g. code-host SSH keys, the class they call cheapest) at public exposure (matrix.relux.works).

## 3. Recommendations

1. **ADOPT NOW: append-only check for the archive.** Reason: turns the manual R10 head verification into a routine guard against silent loss or rollback in exports.
   Smallest change: a PR adding `.scripts/archive-check` (Go or shell) run before the archive commit: for every changed `archive/**/*.md`, the old file's blocks must be a prefix of the
   new file's blocks (a block may only change into a `- redacted` block), and every `index.json` `events` count must not decrease; on failure, print the dropped block ids and refuse to commit.
   Document the call in `docs/cli.md` next to the `a2a archive all` commit recipe.
2. **ADOPT NOW: windows are leases (text only).** Reason: a crashed integrator cannot hold main hostage, and extensions are unambiguous. Smallest change: a PR to rule 19 in `docs/protocol.md`
   (after a `decision` with acks): the window-open system message carries `until=<UTC>` and `base=<main OID>`; its event id is the window token; extension/takeover = new window message `re=<token>`
   (over 3 h or any extension still needs the arbiter); past `until` with no extension the window is closed; the landing announcement cites the token.
3. **LATER: enforced fencing via pre-push hook.** Reason: only needed if board-only pushes keep breaking landings. Smallest change: `a2a window status <subject>` (exit 1 while another
   holder's unexpired window is open) called from a pre-push hook in the shared repo. Skip it entirely if BUG-260929-vipxum / #442 retire rule 19 for board commits first.
4. **LATER: persist the delivery high-water mark in account data.** Reason: a lost local state file is a silent gap today (ch03.6 rollback protection). Smallest change: `wait`/`watch`/`ack` also write
   `{next_batch, last_acked_event}` to `ai.relux.a2a.delivery` account data; with no local state, resume from it and print a warning instead of taking a fresh position.
5. **NO: hash-chained journal, quorum certificates, Merkle snapshots, identity proofs.** Reason: one trusted homeserver and two operators is L0; git signatures + Matrix already give order and attribution,
   and this machinery would cost more than every failure it prevents today. Revisit identity proofs only at public exposure (R10 pre-exposure list).

## 4. Running the integration-fragment model

It is plain Python 3 with no dependencies: `python3 paper/model/integration-fragment-model.py` in the apiary-docs checkout (or `make model` at its root, which also asserts
`full rules: reachable states=1090, violation=None`). It runs in well under a second and prints the full-rules result plus one shortest counterexample trace per disabled rule;
verified here with exactly those numbers. It cannot check our window rules as is: it models candidate/attestation/integration with lease *epochs* on records, not no-push windows, other pushers,
expiry, or arbiter extensions. The shape transfers, though: a ~100-line BFS with state (main head, window holder, token, until-clock tick, per-session pending pushes, redelivered announcements)
and switchable rules (`window_respected`, `expiry`, `token_fencing`, `ff_only`) could show, for example, that without `token_fencing` a superseded integrator lands after an extension, and without
`expiry` a crashed holder deadlocks every pusher (a liveness property, which needs a bounded-steps check rather than the model's safety-only invariant). Worth writing only together with recommendation 2 or 3.
