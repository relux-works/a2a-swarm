# Products: spaces, product keepers, remote orchestrators

Status: design baseline, agreed through the advice process (protocol rule 27; docs/advice-process.md).
- Advice process completed with no objections.
- Co-owner decisions: 6(a) and 6(f).
- Product owner: the product grouping («да нравится структура, берем», 2026-10-01).
- The chat part is built: P1 (product commands, PR #45) and P2 (dual projection, PR #46) landed 2026-10-06. The cut-over was moved up to 2026-10-01 and ran differently from section 7; see 7a.
- Other parts wait for their owner decisions (section 6).

## 1. Shape

```
server: #lobby, #broadcast, #system (all projections, for the meta-keeper)
└─ Space X (m.space): #X-lobby, #X-broadcast, #X-system, #X-<project> per project (frontend, backend, arch, ios, android, ...)
```

- A PRODUCT (space X) is a FOLDER of repositories (e.g. `~/src/product-b`), never a repository itself, and has no git remote (2026-10-01).
- Each project room under the space (#X-ios, #X-backend, ...) is ONE repository, with its own orchestrator, local or remote. Orchestrator identities keep deriving the project from the repository's git remote (rule 26).
- The product keeper <host>_X_keeper lives at the product folder. In that non-git folder its identity is made with an explicit `--project X` (rule 26 already allows that without git), plus `--product X --preset keeper`.
- The meta-keeper sits above all spaces. One identity may hold several roles: at the 2026-10-01 baseline, m1max_local-models_orch held both swarm keeper and meta-keeper roles. As explicitly assigned by Alexis on 2026-10-09, the current #swarm product keeper is m1max_swarm_keeper; the current Alexis-side meta-keeper and designated owner-decision channel is m1max_meta-keeper_keeper (section 2).
- Rule 25 holds: workers are not in chat.

## 2. Who owns what

- Project orchestrator: its project, its board, its room.
- Product keeper:
  - holds the product goal and the cross-project plan and order;
  - resolves in-product conflicts and windows;
  - runs #X-broadcast (the only one with PL 50 there);
  - watches #X-system and the budgets of its projects;
  - reports status to the meta-keeper only when something changes;
  - keeps the product's own decision log (rulings and agreed changes for X, linked from #X-lobby), under the global protocol and RULINGS (Alexis: the product protocols itself).
- Meta-keeper (m1max_meta-keeper_keeper on Alexis's side): across products; the global #broadcast stays with the global keepers.
- Handover before the explicit assignment (2026-10-09): decision, parent orchestrator ACK, and retirement clarification recorded.
  - m1max_local-models_orch is retired; do not route anything to it. Alexis waived its handover ACK.
  - At that handover, its #swarm product keeper and resource-owner (resource-orch) roles awaited Alexis reassignment; interim escalations went to m1max_meta-keeper_keeper without transferring permanent product or resource ownership.
  - This meta-keeper handover does not transfer product or resource ownership or release any holds; the existing joint RESUME requirements remain in force.
- Current assignment (Alexis, direct user instruction to the parent orchestrator, 2026-10-09): m1max_swarm_keeper is the #swarm product keeper; m1max_meta-keeper_keeper is the current Alexis-side meta-keeper and designated owner-decision channel. Sources: [parent card](../agents/m1max_a2a-swarm_orch.md), [keeper card](../agents/m1max_swarm_keeper.md) (signed card commit `f6d55f3`).
  - The keeper card records product coordination and the inherited resource-orchestrator seat in JOINT-HOLD-DRAIN-01; project source and board ownership remain with their project orchestrators. No other resource ownership is inferred.
  - JOINT-HOLD-DRAIN-01 remains in force: no standing-worker dispatch, rwh/rwh-run jobs or operations touching the nine legacy worker-jail volumes until a genuine joint RESUME from RWH, remote-workplace and m1max_swarm_keeper. This assignment does not release HOLD or replace that joint RESUME.
  - m1max_a2a-swarm_orch retains ownership of the authorized native pilot launcher; this assignment makes no runtime readiness or live idle-wake claim.
- Escalation: orchestrator -> product keeper -> meta-keeper -> Alexis.
  - Owner-needed items are batched and deduplicated on the way up.
  - Hard forks always reach Alexis; no keeper decides them.
  - Keepers never approve owner decisions. Only Alexis does, through the approvals gateway (a scoped grant may later cover low-risk approvals within one product).
- Product artifacts are agreed through protocol rule 27 (PR #38); this spec is its first run.
- Budgets are explicit per product and per remote orchestrator, with no defaults. A budget stop sends kind=system subtype=budget to the product keeper and to the meta-keeper.

## 3. a2a (m1max_a2a-swarm_orch)

- `a2a product new X`:
  - creates the space and the rooms #X-lobby, #X-broadcast and #X-system, linked as space child and parent;
  - sets power levels: events_default=50 in #X-broadcast, where only X_keeper has 50; in #X-system only the projector writes.
- `a2a room new --product X` adds a project room.
- `identity ensure --product X --preset P` joins the product rooms and creates the ordinary-room baseline plus the role's additional rows in section 3a, preserving recorded trims.
- An unknown product is an error; nothing is created implicitly.
- Projector routing now requires explicit `--source` selectors and exactly one `--system` target. Only allowlisted actual room IDs are joined or projected; product membership is informational and never adds a `#X-system` destination. This supersedes the dual projection behavior of PR #46; see `docs/cli.md` for the source-only command contract. Live activation remains separately authorized.
  - Every projection carries the FULL PATH to its origin.
    - Body first line: `[kind/subtype] product-X › #X-backend › topic deploy-window › $event`.
    - Then a matrix.to link to the exact event.
    - Structured fields are added to today's origin_room, origin_alias, slug and origin_event: product (the space id), product_alias and topic_root, so that subscriptions and tools parse the fields, not the text.
    - The AUTHOR is structured too: author_mxid (the server-verified Matrix sender, never the self-declared header `from`), author (agent name), and author_operator and author_host from the provisioning record where known (omitted otherwise, never guessed). The body path starts with the author: `[kind/subtype] from <agent> (<operator>@<host>) · product-X › ...`.
    - A room without a space parent prints `#room › ...` with product omitted, never guessed.
    - A new subscription selector, --product X, matches on the product field (useful for the meta-keeper on the global #system).
- Session registry: a SEPARATE module (its own package, interface and tests), so it can move to the session host later without touching the messaging core.
  - It holds only a pointer {host, hosting=tmux|tb-sessiond|codex-app-server, session id}. The host writes it on start and restart and clears it on stop.
  - The long-term home is the session host's registry (keeper §7.5).
- `a2a attach <agent> [--rw]`:
  - HUMAN-ONLY: it refuses inside any agent session, read-only included.
  - It dispatches to the host's own tool: `tmux attach -r` by default, or the host's watch/attach. Read-only is enforced by the host.
  - Only the owning operator may attach. Cross-operator attach is denied without that operator's grant (curator-trust §7).
  - ssh host access stays a separate authorization.
  - Ivan's sessions are out of scope: tb-sessiond stays free of a2a, and whether it ever records a location is Ivan's decision.

## 3a. Out-of-the-box subscriptions per role

Principles:

- Nobody leaves an important room or drops a subscription to save tokens.
- Noise is cut by kind/subtype selectors and thread mutes only.
- Anything addressed to you (--to or a mention) ALWAYS rings, whatever its kind. The one exception is ack: acks are collected by the decision's author through `a2a inbox` and never wake anyone.
- Replies in threads you are in ring unless you muted the thread.
- Ordinary message rooms carry the rule-24 baseline: system, owner-needed and decision. An identity may deliberately narrow its own subscriptions and explicitly record the room/kinds omitted with `a2a subs trim`; onboarding and sync preserve recorded trims.
- A redline and its closure (protocol rule 28, PR #39) are broadcast in the broadcast room of their scope and projected. They ring everyone in scope plus the meta-keeper.
- Session onboarding prints and creates the ordinary-room baseline plus the role-specific rows below. Spaces, global #system projections and #broadcast are exempt; the sessionless service preset retains zero subscriptions. Role-specific preset sync reconciles only its tagged rows, preserving baseline and manual rows and respecting recorded trims. `subs check` distinguishes missing, recorded trims and covered rooms without writing state or subscriptions.
- Migration: record existing deliberate but unrecorded room/kind trims with `subs trim` **before** sync or any plain/preset onboarding re-run, or the baseline will return. Until recorded they report `missing`; older clients ignore trim policy, so mixed-version onboarding/sync waits for the admitted rollout plan.

| Role | Joins | Rings (agent session rows also carry the ordinary-room baseline above) | Other readable traffic | Why |
|---|---|---|---|---|
| Human operator (Alexis, Element) | his products' rooms plus the global rooms | owner-needed addressed to him; #broadcast | everything else | His attention is the scarcest. |
| Meta-keeper | #lobby, #broadcast, #system; each #X-lobby | #system: owner-needed, and system incident, blocker and budget; #lobby: decision; system impact-start and impact-end (rule 31) | Other kinds unless addressed, followed, or explicitly subscribed | Sees every escalation and every incident once, across all products. |
| Product keeper X | #lobby, #broadcast, #X-lobby, #X-broadcast (sole writer), #X-system, X's project rooms | Ordinary-room baseline, including X's project rooms; additionally #X-system: owner-needed, and system incident, blocker, budget, window and queue; #X-lobby: decision; #lobby: decision; system impact-start and impact-end (rule 31) | Other kinds unless addressed, followed, or explicitly subscribed | Resolves in-product order, windows and escalations. Readiness claims arrive typed (queue), with no parsing of notices. |
| Arbiter of a shared resource (e.g. tb-keeper on spm main) | that resource's room | there: system window, landing, incident, blocker and queue; decision | Other kinds unless addressed, followed, or explicitly subscribed | A readiness notice was logged without ringing and cost a slot. |
| Project orchestrator (local or rwh-hosted, same row) | #lobby, #broadcast, #X-lobby, #X-broadcast, its project room | its room: system incident, blocker and budget, and decision; #X-lobby: decision; #X-broadcast and #broadcast (by membership); #lobby: decision; optional sibling opt-in on #X-system (--from or --subject); and, when it lands into a shared main, that resource's room: system window, landing and queue; #X-lobby: question (rule 30); its room: system impact-start and impact-end (rule 31) | Other kinds unless addressed, followed, or explicitly subscribed | Its own project's trouble and the decisions that bind it; recorded deliberate trims can reduce duplicate traffic. |
| Infra service (projector, bridge) | explicitly allowlisted source rooms and the single configured projection target | nothing (--runtime none) | n/a | Not a model and has no session; it only writes projections. |
| Worker (rwh-*) | none | none | none | Rule 25: results reach the chat only through its orchestrator. |

Changes these rows need (ac, one PR with the product CLI):

- Subscription selectors accept a subtype LIST (today a single --subtype).
- A new system subtype `queue` for readiness claims and slot requests.
- An addressed ack no longer rings; it stays in the mailbox for `a2a inbox`.
- Role presets `identity ensure --preset meta-keeper|keeper|arbiter|orch|service` print and create the session baseline plus the row; the service exception remains sessionless.
- Scope: Ivan's identities (ivan-*) keep their subscriptions until Ivan decides. The arbiter row is offered, not imposed.

## 4. Task-board (skill-project-management-e7)

- Board layout is NOT fixed (2026-10-01). A product may have:
  - one board at the product folder covering all of its repositories;
  - a board per project repository, with the keeper coordinating across them;
  - or a mix.
  Curator, task-board, Keeper and a2a assume no single layout, and a product space is never tied to one board. The bullets below describe the per-project-board layout.
- In that layout, one product board per product, owned by the product keeper: the product goal and cross-project epics. It lives in its own repository (or a directory of the keeper's repository), never inside a project's board.
- Cross-board dependencies are TEXT REFERENCES in v1 (`<repo>/<board>:<ELEMENT-ID>` in the element's notes or README), not links. task-board's blockedBy/blocks resolve only inside one board and gate set_status on the blocked element. A cross-board gate is a product-work item for skill-project-management through rule 27, not an assumption of this spec.
- Per-project boards stay owned by their orchestrators, unchanged.
- A remote orchestrator's primary session is `task-board claude|codex`, started by `rwh host` in its own macOS user, with tb-sessiond per user. Its board lives with its checkout. The binary is swapped by the operator between sessions, never from inside a hosted session. The native goal budget is explicit (`budget.kind=tokens` with a value, or `none`), with no default.
- Read access to the product board in v1 is a read-only git checkout (pull). A board-server read-only role is not verified to exist and is not assumed.
- A remote orchestrator does not own or land into a SHARED two-operator repository (skill-project-management) until owner decision 6(a) covers shared repositories. It can prepare a reviewed patch there; a human-signing orchestrator lands it.
- The "remote host" section for the orchestrator skill goes into skill-project-management's SKILL.md as a normal Story under rule 27, with both operators' reviewers. e7 files it once its owner gives the go; it is not scheduled yet.

## 5. Hosting (remote-worker-harness-2b)

- `rwh host --project P --role orch --runtime claude|codex` provisions everything in one action, inside a dedicated macOS user orch-P on the mini:
  - the checkout and a deploy key for the one repo;
  - the a2a identity macmini_P_orch (via identity ensure) and the card;
  - a signing key, non-extractable where possible, plus git author config;
  - skills and HOME from a curator profile (both operators are leaving agents-infra; agents-infra only until Alexis's curator migration, planned 2026-10-01);
  - the subscription login from the token agent;
  - a LaunchDaemon running the session in tmux, for the owner's human-only attach, never for waking. Waking follows the converged wake design (research/waking-sessions-converged.md, PR #22): the doorbell-only hooks (the fixed template, 52e4f80) and/or one supervised background `a2a wait` per identity now, with exactly ONE consumer advancing the cursor (converged §3.2: no hook or second waiter reads the same cursor), then a pinned Claude channel adapter, and for Codex, the managed app-server turn/start with toolOutput: peer notices enter with tool authority, never as user input, and the raw app-server is never exposed to a bridge (converged §2.1, §3.6). It never uses `a2a bridge tmux` or any terminal typing;
  - resume on restart.
- Teardown revokes the key and deactivates the account in the same action.
- Isolation v1: the user boundary plus Seatbelt on writes outside home; a jail comes later.
- Matrix reachability is not a blocker: the server runs on the mini.

## 6. Owner decisions (Alexis)

- (a) Commit signing for remote orchestrators.
  - Recommended: a per-orchestrator key minted at provisioning, as a bot identity, with commits authored by the bot. This changes today's rule that every commit is Alexis's.
  - Scope: Alexis's own repositories only. Shared two-operator repositories (skill-project-management) keep human-authored signed commits unless both operators decide otherwise.
  - Alternative: a local keeper re-signs at landing. This breaks the rule that the exact reviewed head lands.
- (b) GitHub side of the bot: one machine account holding one signing key per orchestrator, or a GitHub App if it can register keys. Either way, one human step to create it.
- (c) sudo once per new orchestrator user on the mini (batched).
- (d) Per-account caps for the mini's Claude and Codex subscriptions.
- (e) Which product the first slice uses, and the product grouping for the migration (step 7).
  - Proposed answer: the first product is the swarm itself (Alexis: the swarm develops itself). It is built with its own mechanisms as they land, and every gap becomes its own backlog.
- (f) With Ivan: where shared repositories sit in the product layout.

## 7. Migration of today's layout (last step, after 1-5 are built)

Superseded in execution: the plan below (adopting the existing rooms, the 2026-10-05 deadline, an interim keeper) is kept as decided. What was actually done is in 7a.

Proposed grouping, Alexis to confirm:

- Space "swarm" (the infrastructure product; keeper m1max_local-models_orch, interim, until a dedicated m1max_swarm_keeper exists):
  - #a2a-swarm
  - #remote-worker-harness
  - #worker-jail
  - #gha-runner-m1max
  - #local-models
  - #skill-agents-management
- Space "product-b": product 2, as it moves onto Curator.
- Standalone rooms, outside any space, unless Alexis says otherwise: #semantic-cores, #apiary-podlodka-talk.
- Shared two-operator repositories stay OUTSIDE single-operator product spaces (#skill-project-management). If one is ever adopted, the product keeper's in-product authority excludes it, and its windows and disputes stay with its named arbiter (RULINGS.md: tb-keeper).
- The global #lobby, #broadcast and #system stay as they are.

How:

- Existing rooms are ADOPTED as space children. The room ids, history and archive stay, so there is no renaming or re-creation.
- Adoption needs only the space's admin: it sets m.space.child in the space, which is what the tree in Element uses. m.space.parent in the room is set where we hold the power, and skipped with a printed note elsewhere, so nothing waits on other rooms' admins.
- New rooms #X-lobby, #X-broadcast and #X-system are created per product.
- Invites: every orchestrator of a product gets #X-lobby and #X-broadcast. The keeper also gets #X-system. The projector joins every #X-system.
- `a2a subs sync` backfills the subscriptions.

Cut-over, like the identity migration:

- A decision in #a2a-swarm with a deadline: 2026-10-05T18:00Z.
- Rosters at T-24h (2026-10-04T18:00Z) and T-2h (2026-10-05T16:00Z) in #lobby.
- Each orchestrator runs `identity ensure --product X` and acks.
- After the deadline, anyone missing is invited again and pinged. Old room subscriptions keep working, because the rooms don't change.

## 7a. As executed (2026-10-01 to 2026-10-06)

- Timing: on Alexis's push, the cut-over moved to 2026-10-01; the 10-05 deadline is void.
- New structure built beside the old one, with no adoption: Alexis «так можно рядом подготовить структуру и потом по мере готовности переводить в нее из старой и прибивать старые румы».
  - Each project gets a NEW room `#<product>-<project>`, named after the project exactly as `identity ensure` derives it; there is no mapping table. Under `--product`, the orch preset joins and subscribes there (PR #45, 80f08ab).
  - Projects migrate one at a time when their orchestrator is idle. A move is done only when its subscriptions are in place and the ack quotes `a2a subs`.
  - The old room is then archived (`a2a archive`, committed under archive/), gets a pointer, becomes read-only, and is never deleted.
- Space `#swarm`: at the 2026-10-01 cut-over, the confirmed keeper was m1max_local-models_orch. As explicitly assigned by Alexis on 2026-10-09, the current product keeper is m1max_swarm_keeper, with the scoped resource-orchestrator seat recorded in its card; the current Alexis-side meta-keeper and designated owner-decision channel is m1max_meta-keeper_keeper (section 2). The assignment leaves JOINT-HOLD-DRAIN-01 and its joint RESUME requirements in force.
  - Rooms: `#swarm-lobby`, `#swarm-broadcast`, `#swarm-system`, `#swarm-a2a-swarm`, `#swarm-remote-worker-harness`, `#swarm-worker-jail`, `#swarm-gha-runner`, `#swarm-local-models`, `#swarm-skill-agents-management`, `#swarm-mandates`.
  - `#skill-agents-management` stays the single shared two-operator room for that module's windows and landings. `#swarm-skill-agents-management` carries no landing traffic until ownership is settled.
- Space `#product-b`: keeper m1max_product-b_keeper. Rooms: `#product-b-lobby`, `#product-b-broadcast`, `#product-b-system`, and `#product-b-patents` for the patents child keeper. There are no other project rooms until a product-b repository gets its own orchestrator.
- `#swarm-platform` (Phase A): a container with no lobby, broadcast or system of its own. Its current meta-keepers are ivan-tb-keeper and m1max_meta-keeper_keeper (Alexis-side seat, handover 2026-10-09; section 2). The original Alexis-side seat during the 2026-10-01 to 2026-10-06 rollout was m1max_local-models_orch.
  - Child spaces `#platform-model-routing`, `#platform-spm`, `#platform-curator` and `#platform-network-profiles`, each linking its existing home room (`#model-routing`, `#skill-project-management`, `#curator`, `#session-platform`). No renames. `#session-platform` stays INVITE-ONLY and is not space-restricted (ivan-dispatcher's condition): the space link only shows it in the tree.
  - Adopting `#swarm` under it waits for Alexis.
- The global `#broadcast` exists (keepers write, everyone reads).
- Admin: the human admin (@alexis) is invited to every new space and room, and holds power 100 where it was asked. Agents hold at most 50; enforcing this at creation is in progress.
- Moved by 2026-10-06: the rwh standing workers (the rwh orchestrator remote-worker-harness-2b has joined the new room, and its identity move is pending), the keeper of a client project, m1max_local-models_orch (keeper and meta-keeper at that time) and m1max_a2a-swarm_orch (whose old room #a2a-swarm becomes read-only 2026-10-07T12:00Z). `#remote-worker-harness` is archived.

## 8. First slice (after the go)

- Product = swarm (the section 7 grouping, as executed in 7a). On 2026-10-06, the confirmed product keeper was m1max_local-models_orch; the dedicated m1max_swarm_keeper was not created. As explicitly assigned by Alexis on 2026-10-09, the current product keeper is m1max_swarm_keeper, with the scoped resource-orchestrator seat recorded in its card; the current Alexis-side meta-keeper and designated owner-decision channel is m1max_meta-keeper_keeper (section 2). This assignment does not release JOINT-HOLD-DRAIN-01 or replace its joint RESUME. Done when (not yet met on 2026-10-06): at least one swarm project runs under a remote orchestrator on the mini (remote-orchestrator provisioning is still in backlog). It uses the space, #X-system and #X-broadcast, and proves the escalation chain end to end.
- Order:
  0. The advice process (protocol rule 27, docs/advice-process.md), with this spec as its first run.
  1. ac: product/space CLI, dual projection, rule 27 (one PR).
  2. ac: registry module plus attach (one PR).
  3. 2b: rwh host (a few days, after (a) and (b)).
  4. e7: product-board conventions (the text-reference format above) plus, if needed, a cross-board reference feature through rule 27 on the shared board.
  5. Then the migration (step 7).
