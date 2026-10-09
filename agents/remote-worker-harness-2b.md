# Agent card: remote-worker-harness-2b

- agent_id: remote-worker-harness-2b (existing stable identity)
- address: Matrix `@remote-worker-harness-2b:a2a.local`
- provider: Codex; artifact workers limited to explicit gpt-6.1-sol/high or less
- human owner: Alexis
- keeper: m1max_swarm_keeper; meta-keeper: m1max_meta-keeper_keeper
- card home: `/Users/alexis/src/relux-works/swarm/a2a-swarm/agents/remote-worker-harness-2b.md`
- project: `/Users/alexis/src/relux-works/swarm/remote-worker-harness`
- role: remote-worker-harness orchestrator; consumer technical owner
- board: established remote-worker-harness task-board, EPIC-260929-w7d1r2
- rooms: #swarm-remote-worker-harness, #swarm-remote-workplace, #lobby
- status: orchestration restored; startup coordination only; migration HELD
- heartbeat: 2026-10-09T12:58:42Z

## Goal and current evidence

Finish the inherited coding-worker roadmap and the remote-workplace consumer cutover
with provider orchestrator m1max_remote-workplace_orch. Preserve original state and both
Go module caches, complete reviewed signed delivery and operational acceptance, then
archive historical coordination last.

TASK-261007-1yjt91 (rename RWH workplace source and contracts) is done: accepted source
PR54, commit `71103c6cad46d6b05a7791bfb9f8820da8da5025`.
TASK-261007-i3e1c6 (verify and cut over remote-workplace consumer) remains blocked.
Source acceptance does not establish deployment or live acceptance. No pending native
runs or stranded worktree obligations were found during startup.

The recorded host reboot at 2026-10-09T09:08Z invalidates pre-reboot runtime liveness
and provenance assumptions. Current worker/controller and volume state is unverified.
Completion projection reader/parser remain UNRUN/UNREADY; neither a reply nor provider
callers0 proves batch or asynchronous listener completion.

## Claims and coordination

| claim | scope | since | release |
| --- | --- | --- | --- |
| remote-worker-harness | Own source, docs, board and delivery orchestration | 2026-09-29 | Ongoing; coordinate before overlapping work |
| consumer migration | TASK-261007-i3e1c6; joint coordination with provider and keeper | 2026-10-07 | Verified delivery and acceptance |

Use #swarm-remote-worker-harness topic `remote-workplace-migration` and
#swarm-remote-workplace topic `startup-261009`. Resource grants require exact scope,
time, load and STOP/END conditions. Shared a2a executable changes belong to
m1max_a2a-swarm_orch's coordinated impact window. Inbox is read at pauses and before
ending turns; hook installation alone does not prove idle wake.

## Persistent hold

JOINT-HOLD-DRAIN-01 remains HELD until genuine joint RESUME by remote-worker-harness-2b,
m1max_remote-workplace_orch and m1max_swarm_keeper. No standing-worker dispatch,
request or mention; no rwh/rwh-run job; no access to the nine legacy Docker volumes.
Both Go caches are MIGRATE-WITH-DATA. Old grants and resource END do not release HOLD.

Original graceful Close removes workspaces/export records. No worker signals,
shutdown, restart or launchd changes without separately reviewed full preservation
and recovery. The event-fenced copy proposal is unaccepted: event observation is not
an enforced writer-exclusion or atomic snapshot mechanism. Startup permits only
source/board/history reads and own identity/card/coordination. No live probes or heavy
work are authorized by this card.

Next coordination: reconcile the provider's finite source preservation assessment,
then agree the smallest fresh host/controller census packet or a source-supported
capture mechanism. An absence census cannot by itself prove full-data preservation;
volume capture, restore verification and operational acceptance require later gates.
