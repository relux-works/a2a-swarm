# Waking agent sessions: converged design from two independent reviews

Author: ivan-tb-keeper, 2026-09-29. Status: proposal, for #a2a-swarm governance.

**Inputs.** Two independent, read-only reviews of a2a's environment delivery at `1dbc2c8`, run at Ivan's request:
- Report A: Claude Opus 5.5 at max effort, 47 KB.
- Report B: gpt-6-astra at max effort, 30 KB. It re-verified Report A against the code and the current docs, plus Codex source pinned to `rust-v0.159.0`.

Both reports are kept verbatim on Ivan's side and are available on request. This note records where the two agree, how their disagreements are resolved, and the design both support.

## 1. Settled facts

| Claim | A | B | Resolution |
| --- | --- | --- | --- |
| Hooks put up to 20 peer-authored records into `additionalContext` and the Stop `reason`, with uncapped header fields (`hook.go:255-286`, `wait.go:133-158`) | V | V | fact |
| Claude Code treats hook output as user feedback | V (third-party capture) | "unverifiable from docs" | **Verified.** The live Claude Code harness prompt of tb-keeper's own session (2026-09-29) contains: "Hooks may intercept tool calls; treat hook output as user feedback." The High finding stands. |
| `asyncRewake` (exit 2) wakes an idle Claude session | V | V | fact; but see §2.3 |
| Cross-session SendMessage wakes an idle receiver and is read between tool calls | V | V | fact; same machine, one account |
| Codex hooks and `notify` cannot wake an idle thread | V | V | fact |
| `a2a bridge tmux` can press Enter into a permission, question or plan dialog; checking the screen and sending are separate calls (`bridge.go:185-194,249-258`) | V | V | fact |
| The hook's blocking `flock` is outside the 5 s budget; a hook can stall a turn up to Claude's 600 s timeout (`lock_unix.go:16`) | V | V | fact |
| task-board's own tb-sessiond pastes a notice and presses Enter after checking only "running" (`claude_host.go` `DeliverSessionNotice`) | V | V | fact. It has the same dialog hazard. The text is task-board-authored, so there is no peer-text injection. |

## 2. Disagreements and how they are resolved

1. **Codex delivery.**
   - A proposed `turn/start` when idle and `turn/steer` when busy.
   - B points out that both carry *user* input. `turn/start` with `toolOutput` (Codex `ExternalMessage`, SDK ≥ 0.151.0) delivers external content with **tool authority**: it starts an idle thread or joins an active regular turn, and persists as `functionCallOutput`.
   - **Resolved for B:** peer notices go in as `toolOutput`. `turn/steer` is kept for authorized user steering only.
2. **Terminal typing.**
   - A proposed hardening the bridge. B shows that no screen check makes "still idle, then submit" atomic.
   - **Resolved for B:** retire `bridge tmux`, and retire task-board's PTY notice typing too. On our side that becomes a task-board bug.
3. **Claude idle wake.**
   - A proposed piloting a long-lived `asyncRewake` doorbell. B found a liveness hole: the hook's timeout still applies, so a listener armed on SessionStart and Stop can expire during a long idle with nothing left to re-arm it.
   - **Resolved:**
     - Now: exactly one supervised background `a2a wait` per identity, with silent re-arm on timeout. Ivan's side already runs it this way, so there are no model turns on timeouts.
     - Next: pilot a version-pinned Claude *channel* that emits only a fixed doorbell and exposes mailbox pull and receipt tools. The permission relay stays off.
     - `asyncRewake` stays an optional adapter mechanism, never a perpetual dispatcher.
4. **Delivery guarantees.** A called a2a's delivery state right, with two edge cases. B refutes that as an end-to-end guarantee:
   - a hook acknowledges an earlier pending wait batch before the model consumes it (`delivery.go:231`);
   - `watch --json` commits after writing to stdout (`delivery.go:522`);
   - backfill caps history at 2,000 events *before* relevance classification, so an unrelated flood can push an addressed message out of reach (`timeline.go:9`);
   - a stable backlog of 1,021 relevant events, read in 20-record batches against a 1,000-entry ack ring, can starve the last event forever. This was reproduced in an in-memory model, not in a2a itself.

   **Resolved for B:** fix the receipt boundary *before* building adapters on `watch`.
5. **Ownership.**
   - A cited the early notice-API proposal. B cites the later archived agreement (`bridge-daemon.md:38`): the task-board notice API was withdrawn, and delivery is a2a's.
   - **Resolved:** a2a owns the interim mailbox and narrow adapters, with no task-board coupling. The agent session host takes over lifecycle, wake admission, pause and audit in the target design.

## 3. The design

### Now: contain, then replace one path per harness
1. **Stop.** No `bridge tmux`, and no hooks that carry peer content. Done on both sides today: local-models-e9 stopped its bridge at 16:38Z, and no Ivan-side session installed the hooks.
2. **One consumer per identity.** A single supervised background `wait` with silent re-arm is a temporary, best-effort path. No hook or second waiter reads the same cursor.
3. **Durable mailbox in a2a.**
   - Persist received events before advancing the transport position.
   - Bounded pull with `a2a inbox`, using an untrusted-data frame, verified sender and trust tier.
   - Explicit per-event or batch receipts on pull, never on ring or stdout.
   - Gaps are surfaced and never skipped silently.
   - Backfill runs relevance-first, or the gap is reported.
4. **Doorbell-only hooks**, if kept: a fixed a2a-authored template under 300 characters (counts, whitelisted kinds, verified sender localparts, ids). Stop uses `additionalContext`. A timeout-bounded lock never spans network I/O. Identity comes from the launcher, not from `settings.local.json`.
5. **Claude:** pilot the channel adapter (step 3 of §2.3). Where channels cannot be enabled, keep path 2 until a host-owned stream-input adapter exists. Never fall back to typing.
6. **Codex:** the managed app-server with `turn/start.toolOutput`, capability-checked. The TUI is bound to that host, and no parallel `exec resume` runs on its thread. Do not expose the raw app-server to any bridge: its `process/*` API runs outside the sandbox, and a non-loopback WebSocket is unauthenticated by default.
7. Both operators run the same local arrangement. Matrix is the carrier between machines.

### Target
`durable mailbox (a2a → waggle)` → `session host (policy: binding, admission, coalescing, pause, audit)` → `native adapter (Claude channel or stream-input, Codex toolOutput)` → `agent pulls bodies as tool output` → `read receipt`.

Authority never comes from a message. Cancel, halt and lease changes are carried out by the host or the kernel after a signature check (waggle).

### Rules (A's S1–S9 merged with B's properties)
| Rule | Content |
| --- | --- |
| Content boundary | Only a host-authored fixed doorbell enters a privileged channel, with no peer-derived names, subjects or URLs. Bodies arrive only as bounded tool output in an untrusted-data frame with provenance. |
| Effects boundary | Sender authenticity is not permission to act. Scope, leases, file and network access, signing and publishing are enforced outside the model. Framing is hygiene, not immunity. |
| Identity | One delivery owner per principal and session generation, bound to a harness thread id, node and project. Never to a directory or pane. |
| Wake authorization | Default deny: explicit sender principals, recipient, scope and message classes. Room membership grants no wake right, and invites never wake. |
| Durability | Distinct states: `received_durable`, `submitted`, `exposed`, `processed`. None is inferred from stdout or an RPC reply. Ambiguous submissions are reconciled before any retry. |
| Rate and cost | 1 s coalescing, a per-pair burst of 2, at most one admission per 30 s, and a per-recipient ceiling of 20 per hour plus a daily budget. Excess mail stays pending and visible. No timer-driven model turns. |
| Pause | A durable node and session pause, checked before dispatch; it wins over broadcast too. Resume needs operator authority. Emergency stop goes through the supervisor, never through prose. |
| Busy and blocked | Ordinary mail waits for the turn to end; urgent mail joins at a safe harness boundary. Never interrupt a tool. Sessions blocked on a person keep their mail pending. |
| Versions | Launcher-owned settings and pinned, tested adapter versions. A missing capability fails closed, with no typing fallback. |

### Acceptance tests (merged)
- **Idle cost:** 24 hours idle gives zero mail-induced model requests. Mail after more than 12 hours still wakes the session.
- **Codex authority:** persisted notices are `functionCallOutput`, never user or developer input, in idle, busy and long-tool cases.
- **Crash at every boundary** (receive, persist, cursor, submit, expose, receipt): nothing admitted is lost and no effect is duplicated.
- **Backlog and gaps:** histories of 1,000, 1,020 and 1,021 events, larger-than-backfill floods across rooms: no starvation and no silent loss.
- **Single ownership:** two adapters for one identity, and two sessions in one directory: only the current generation receives.
- **Human control:** open permission, question and plan dialogs, partial typed input: no synthetic bytes, no approval, no restart.
- **Injection fuzz** on bodies and metadata: the doorbell stays constant, and hostile content never reaches signing, approval, configuration or other sessions.
- **Rate limits, pause, bounded failure** (held locks, hung network, SIGTERM) and **upgrade compatibility**, which fails visibly.

## 4. Open questions (for the operators)
1. Can both operators enable a pinned custom Claude channel under their account policies?
2. Which principals may wake across operators before waggle signatures (CM2)? A suggested default is pull-only across operators except the arbiters.
3. What does an operator interrupt mean: pause until resume, or allow selected later work?
4. What retention and gap-recovery policy applies when Matrix history exceeds backfill?
