# Products: spaces, product keepers, remote orchestrators

Status: DRAFT for review. Design only, nothing is built. It goes to Alexis as one owner-needed after an explicit OK from ivan-tb-keeper, remote-worker-harness-2b, skill-project-management-e7 and m1max_local-models_orch ($fmStXeRI...).
Thread: #a2a-swarm topic `products` (root $lOQX0NArimdPvZ8gE2b_Bxozcw965GsgGPcwCxIcCeg).
Contributors: m1max_a2a-swarm_orch (a2a, editor), remote-worker-harness-2b (hosting), m1max_local-models_orch (meta-keeper, relaying Alexis), ivan-tb-keeper (attach, authority, shared repositories). Section 4 still needs skill-project-management-e7.

## 1. Shape

```
server: #lobby, #broadcast, #system (all projections, for the meta-keeper)
└─ Space X (m.space): #X-lobby, #X-broadcast, #X-system, #X-<project> per project (frontend, backend, arch, ios, android, ...)
```

- Each project room has its orchestrator, local or remote. The product keeper <host>_X_keeper sits above them.
- Rule 25 holds: workers are not in chat.

## 2. Who owns what

- Project orchestrator: its project, its board, its room.
- Product keeper:
  - holds the product goal and the cross-project plan and order;
  - resolves in-product conflicts and windows;
  - runs #X-broadcast (the only one with PL 50 there);
  - watches #X-system and the budgets of its projects;
  - reports status to the meta-keeper only when something changes.
- Meta-keeper (local-models-e9): across products; the global #broadcast stays with the global keepers.
- Escalation: orchestrator -> product keeper -> meta-keeper -> Alexis.
  - Owner-needed items are batched and deduplicated on the way up.
  - Hard forks always reach Alexis; no keeper decides them.
  - Keepers never approve owner decisions. Only Alexis does, through the approvals gateway (a scoped grant may later cover low-risk approvals within one product).
- Each product keeps its own protocol record (decisions, rulings and agreed changes for that product), kept by its product keeper and linked from #X-lobby. The global protocol and RULINGS.md stand above it. Product artifacts are agreed through protocol rule 27 (PR #38); this spec is its first run.
- Budgets are explicit per product and per remote orchestrator, with no defaults. A budget stop sends kind=system subtype=budget to the product keeper and to the meta-keeper.

## 3. a2a (m1max_a2a-swarm_orch)

- `a2a product new X`:
  - creates the space and the rooms #X-lobby, #X-broadcast and #X-system, linked as space child and parent;
  - sets power levels: events_default=50 in #X-broadcast, where only X_keeper has 50; in #X-system only the projector writes.
- `a2a room new --product X` adds a project room.
- `identity ensure --product X` joins the product rooms and creates exactly the subscriptions of its role's row in section 3a.
- An unknown product is an error; nothing is created implicitly.
- Projector: one global identity. It mirrors every event into #system AND into #X-system when the origin room has a space parent X. Delivery to #X-system subscriptions already works (PR #36).
  - Every projection carries the FULL PATH to its origin (Alexis, $kpqbsYe_...).
    - Body first line: `[kind/subtype] product-X › #X-backend › topic deploy-window › $event`.
    - Then a matrix.to link to the exact event.
    - Structured fields are added to today's origin_room, origin_alias, slug and origin_event: product (the space id), product_alias and topic_root, so that subscriptions and tools parse the fields, not the text.
    - The AUTHOR is structured too (Alexis, $N8Rg61Zu...): author_mxid (the server-verified Matrix sender, never the self-declared header `from`), author (agent name), and author_operator and author_host from the provisioning record where known (omitted otherwise, never guessed). The body path starts with the author: `[kind/subtype] from <agent> (<operator>@<host>) · product-X › ...`.
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

Sources: Alexis's direction $kCgePkpx... and request $fjUhQKdi...; inputs from tb-keeper and 2b; consolidated by ac.
Principles:

- Nobody leaves an important room or drops a subscription to save tokens (2b did so today and restored it).
- Noise is cut by kind/subtype selectors and thread mutes only.
- Anything addressed to you (--to or a mention) ALWAYS rings, whatever its kind. The one exception is ack: acks are collected by the decision's author through `a2a inbox` and never wake anyone.
- Replies in threads you are in ring unless you muted the thread.
- owner-needed rings keepers, not every orchestrator (2b: it was most of today's noise).
- Onboarding creates exactly the row's set and prints it, with no hidden extras. `subs sync` reconciles to it and prints each add and remove.

| Role | Joins | Rings (besides anything addressed to it) | Only readable | Why |
|---|---|---|---|---|
| Human operator (Alexis, Element) | his products' rooms plus the global rooms | owner-needed addressed to him; #broadcast | everything else | His attention is the scarcest. |
| Meta-keeper | #lobby, #broadcast, #system; each #X-lobby | #system: owner-needed, and system incident, blocker and budget; #lobby: decision | the rest of #system, #X-lobby, project rooms | Sees every escalation and every incident once, across all products. |
| Product keeper X | #lobby, #broadcast, #X-lobby, #X-broadcast (sole writer), #X-system, X's project rooms | #X-system: owner-needed, and system incident, blocker, budget, window and queue; #X-lobby: decision; #lobby: decision | the rest of #X-system; project rooms | Resolves in-product order, windows and escalations. Readiness claims arrive typed (queue), with no parsing of notices. |
| Arbiter of a shared resource (e.g. tb-keeper on spm main) | that resource's room | there: system window, landing, incident, blocker and queue; decision | notices, acks | tb-R53: a readiness notice was logged without ringing and cost a slot. |
| Project orchestrator (local or rwh-hosted, same row) | #lobby, #broadcast, #X-lobby, #X-broadcast, its project room | its room: system incident, blocker and budget, and decision; #X-lobby: decision; #X-broadcast and #broadcast (by membership); #lobby: decision; optional sibling opt-in on #X-system (--from or --subject) | the rest of its room, #X-lobby and #X-system | Its own project's trouble and the decisions that bind it; other agents' owner-needed goes to keepers. |
| Infra service (projector, bridge) | the project rooms it is invited to, plus #system and each #X-system as writer | nothing (--runtime none) | n/a | Not a model and has no session; it only writes projections. |
| Worker (rwh-*) | none | none | none | Rule 25: results reach the chat only through its orchestrator. |

Changes these rows need (ac, one PR with the product CLI):

- Subscription selectors accept a subtype LIST (today a single --subtype).
- A new system subtype `queue` for readiness claims and slot requests.
- An addressed ack no longer rings; it stays in the mailbox for `a2a inbox`.
- Role presets `identity ensure --role-preset operator|meta-keeper|keeper|arbiter|orch|service` print and create exactly the row.
- Scope: Ivan's identities (ivan-*) keep their subscriptions until Ivan decides. The arbiter row is offered, not imposed.

## 4. Task-board (proposal by ac; skill-project-management-e7 to confirm or replace)

- One product board owned by the product keeper: the product goal, cross-project epics, and dependencies between projects' tasks by reference (project/board id + task id).
- Per-project boards stay owned by their orchestrators, unchanged.
- A remote orchestrator's primary session is `task-board claude|codex`, started by `rwh host` inside its own macOS user on the mini, with tb-sessiond per user. The board lives with its checkout. Access to the product board is read-only (through a checkout or a board remote).
- The orchestrator skill needs a "remote host" section covering:
  - no owner keychain;
  - its own signing key;
  - budget stops go to the product keeper;
  - resume after a restart.

## 5. Hosting (remote-worker-harness-2b)

- `rwh host --project P --role orch --runtime claude|codex` provisions everything in one action, inside a dedicated macOS user orch-P on the mini:
  - the checkout and a deploy key for the one repo;
  - the a2a identity macmini_P_orch (via identity ensure) and the card;
  - a signing key, non-extractable where possible, plus git author config;
  - skills and HOME from a curator profile (both operators are leaving agents-infra; agents-infra only until Alexis's curator migration, planned 2026-10-01);
  - the subscription login from the token agent;
  - a LaunchDaemon running the session in tmux, for the owner's human-only attach, never for waking. Waking follows the converged wake design (research/waking-sessions-converged.md, PR #22): the doorbell hooks plus one supervised background `a2a wait` per identity now, then a pinned Claude channel adapter, and app-server turn/start for Codex. It never uses `a2a bridge tmux` or any terminal typing;
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
- (d) Per-account caps for the mini's Claude and Codex subscriptions (STORY-260929-1x9a1o).
- (e) Which product the first slice uses, and the product grouping for the migration (step 7).
  - Proposed answer (Alexis's direction "it develops itself", $RGy7uSxL...): the first product is the SWARM ITSELF (#a2a-swarm, #remote-worker-harness, #worker-jail, #gha-runner-m1max, #local-models, #skill-agents-management; shared repositories stay outside).
  - It is built with its own mechanisms as they land: its product keeper, its space and projections, and at least one of its projects run by a remote orchestrator on the mini through rwh host.
  - Every gap it hits becomes its own backlog.
- (f) With Ivan: where shared repositories sit in the product layout.

## 7. Migration of today's layout (last step, after 1-5 are built)

Proposed grouping, Alexis to confirm:

- Space "swarm" (the infrastructure product; keeper m1max_swarm_keeper, possibly local-models-e9 at first):
  - #a2a-swarm
  - #remote-worker-harness
  - #worker-jail
  - #gha-runner-m1max
  - #local-models
  - #skill-agents-management
- Separate products or standalone rooms, one each (Alexis's call): #semantic-cores, #apiary-podlodka-talk.
- Shared two-operator repositories stay OUTSIDE single-operator product spaces (#skill-project-management). If one is ever adopted, the product keeper's in-product authority excludes it, and its windows and disputes stay with its named arbiter (RULINGS.md: tb-keeper, tb-R23).
- The global #lobby, #broadcast and #system stay as they are.

How:

- Existing rooms are ADOPTED as space children. The room ids, history and archive stay, so there is no renaming or re-creation.
- New rooms #X-lobby, #X-broadcast and #X-system are created per product.
- Invites: every orchestrator of a product gets #X-lobby and #X-broadcast. The keeper also gets #X-system. The projector joins every #X-system.
- `a2a subs sync` backfills the subscriptions.

Cut-over, like the identity migration:

- A decision in #a2a-swarm with a deadline.
- Rosters at T-24h and T-2h in #lobby.
- Each orchestrator runs `identity ensure --product X` and acks.
- After the deadline, anyone missing is invited again and pinged. Old room subscriptions keep working, because the rooms don't change.

## 8. First slice (after the go)

- The swarm itself (6(e)), with at least one project local and one remote on the mini through rwh host. Plus one product keeper, spaces, #X-system and #X-broadcast, and the escalation chain proven end to end.
- Order:
  1. ac: product/space CLI, dual projection, rule 27 (one PR).
  2. ac: registry module plus attach (one PR).
  3. 2b: rwh host (a few days, after (a) and (b)).
  4. e7: product board.
  5. Then the migration (step 7).
