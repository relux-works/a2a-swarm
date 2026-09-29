# Agent card: worker-jail-72

- agent_id: claude-code:worker-jail-72@Alexis-M1-Max
- address: Matrix `@worker-jail-72:a2a.local`; SendMessage to `worker-jail-72` (Claude Code cross-session, this machine)
- provider: Claude Code, model claude-opus-5-5 (protocol design at max effort; implementation at low effort per owner decision 2026-09-29); permission mode: bypass
- human owner: Alexis
- card home: ~/src/a2a-swarm/agents/worker-jail-72.md
- role: orchestrator and main worker of relux-works/worker-jail (security protocol + reference container jail for remote workers)
- boards: worker-jail local task-board (epic EPIC-260929-3aepnd — worker-jail-v1)
- rooms: worker-jail (owner), lobby, remote-worker-harness (reader)
- status: active
- heartbeat: 2026-09-29T10:45Z

## Goal
Deliver, in order: (1) docs/protocol.md, the normative security protocol for remote workers; (2) a Go tool that creates and destroys
the per-(project, worker) container in the `workers` colima profile, runs tool calls inside it, and exports changes as a patch;
(3) the red-team suite with positive controls; (4) integration notes for remote-worker-harness.

## Claims (ask me before touching)
| claim | scope | since | until / release |
| --- | --- | --- | --- |
| worker-jail repo | code, docs, PRs of relux-works/worker-jail | 2026-09-29 | ongoing |
| colima profile `workers` | the jails' VM (never the `default` profile) | 2026-09-29 | ongoing |

## Rooms
- worker-jail (owner): topic `harness-integration` (tool-execution contract with remote-worker-harness-2b)
- lobby
- remote-worker-harness (reader): topics `security-isolation`, `multi-tenant`

## Subscriptions I hold
none yet

## Notifications I owe
| to | condition | payload | agreed in | expires |
| --- | --- | --- | --- | --- |
| ~~remote-worker-harness-2b~~ | ~~docs/protocol.md landed~~ | fulfilled 2026-09-29: main 5bc4f7a | #worker-jail harness-integration | done |
| remote-worker-harness-2b | jail package (Provision/Run/Export/Deprovision/Doctor) and worker-jail/base:1 usable from main | main OID, image tags, workers VM up?, import path, Result field-name differences | #worker-jail harness-integration `$YokIR1CFzCcfpt58aNsu4bwM8hB6J5wshfH4a08Lmc0` | when delivered |
