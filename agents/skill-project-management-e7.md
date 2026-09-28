# Agent card: skill-project-management-e7

- agent_id: claude-code:skill-project-management-e7@Alexis-M1-Max
- address: SendMessage to `skill-project-management-e7` (Claude Code cross-session, this machine); managed session SES-tOV6MZxAMlRP8qs6MA-xBMi0 (hosted by tb-sessiond)
- provider: Claude Code, model claude-opus-5-5; permission mode: bypass
- human owner: Alexis
- role: orchestrator (primary for this board; spawns tracked producer/reviewer/landing runs, works inline only on operator config and diagnosis)
- boards: relux-works/skill-project-management (owner of PRIMARY-GOAL-260824-3m8ezf)
- rooms: skill-project-management
- status: active
- heartbeat: 2026-09-28T21:18:02Z

## Goal
PRIMARY-GOAL-260824-3m8ezf (rev 107): LANDING-FIX MODE, fix the delivery path itself, no product backlog work.
Land every finished delivery-path fix through the serial landing queue (README-touching CRs converge one after another), keep every run notice routed.
Owner directives in force: do not touch concurrency or validation limits; land everything finished that is hanging.

## Landing queue (serial; owner of the order)
BUG-260928-17y57p (trunk load-flake fix, gates all) -> BUG-260927-2koa2z -> BUG-260928-36b6x1 -> BUG-260928-2v4mk3 -> BUG-260928-m6l5lt -> BUG-260925-2l8umm -> BUG-260927-3ib9pa -> BUG-260926-31ckqk -> **BUG-260928-2hgfo9 (local-models-e9, acked slot)** -> then as accepted: BUG-260928-2qon2t, BUG-260925-32dplj, TASK-260928-6k5mvz, TASK-260928-1gkk2b, BUG-260928-33o6k2.

## Claims (ask me before touching)
| claim | scope | since | until / release |
| --- | --- | --- | --- |
| landing queue | the elements listed above, their Change Requests, Story branches `task-board/story/<STORY>` and worktrees `.temp/<STORY>/worktree` | 2026-09-28 | each landing |
| live runs | every RUN-260928-* spawned by this session; spawn/cancel/nudge directives on them | 2026-09-28 | terminal + routed |
| control root | fast-forwards of the main checkout (CAS update-ref, disjoint paths only), write-boundary clearances, landing PRs and ff-pushes to origin/main | 2026-09-28 | session end |
| operator config | task-board.config.json (gitignored); currently frozen by owner directive, no edits | 2026-09-28 | owner |
| stopgap | TASK-260928-1z4j37 (parked) and ~/.local/share/task-board/validation/ | 2026-09-28 | superseded by TASK-260928-1gkk2b |
| parked, not mine to change | RUN-260925-89b114 / RUN-260925-6a6cf7 (deadlock repro kept alive by owner decision 2026-09-28) | 2026-09-28 | BUG-260928-36b6x1 lands |

## Topics I take part in
- rooms/skill-project-management/topics/orchestrator-channel.md (with local-models-e9)

## Notifications I owe (protocol v0.2 (l))
| subscriber | event | channel | status |
| --- | --- | --- | --- |
| local-models-e9 | BUG-260926-31ckqk landed on trunk (with trunk commit OID; mention any change to how provider_condition is built in rev5) | post to rooms/skill-project-management/topics/orchestrator-channel.md + SendMessage ring | open |
