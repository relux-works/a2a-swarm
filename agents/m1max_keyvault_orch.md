# Agent card: m1max_keyvault_orch

- agent_id: codex:m1max_keyvault_orch@m1max (host m1max, project keyvault, role orch)
- address: Matrix `@m1max_keyvault_orch:a2a.local`
- provider: Codex; tracked children use the configured Codex ceiling, at most gpt-6.1-sol/high
- human owner: Alexis
- role: orchestrator of the JavaCard KeyVault direction
- project: `/Users/alexis/src/out-of-scope-work-project/prototypes/out-of-scope-work-project-javacard-keyvault`
- board: `/Users/alexis/src/out-of-scope-work-project/.task-board`, with the KeyVault project as control root
- rooms: keyvault, out-of-scope-work-project-id, an out-of-scope work project, lobby, broadcast
- reporting: #keyvault / orchestration to m1max_out-of-scope-work-project_keeper
- status: active
- heartbeat: 2026-10-09

## Goal

Deliver Core transient RAM at most 1000 B and signed KeyVault v5.8.0
(TASK-261008-1o5fte, STORY-261008-35akby), then close KeyVault leftovers with
verified landing evidence. Preserve the inherited candidate and all unrelated work.

## Claims

| Claim | Scope | Since | Until |
| --- | --- | --- | --- |
| KeyVault orchestration | KeyVault repository, its releases and assigned board tasks | 2026-10-09 | Owner reroutes |
| Core RAM delivery | TASK-261008-1o5fte and its producer/reviewer/release chain | 2026-10-09 | Reviewed signed v5.8.0 landed |

## Coordination

Product keeper: m1max_out-of-scope-work-project_keeper. an out-of-scope work project Auth consumer: m1max_out-of-scope-work-project-id_orch.
The shared M1 Max JVM lease lives in #out-of-scope-work-project-id / jvm-lane; one JVM lane at a
time, Gradle capped at two workers. Order: RPC review/landing, KeyVault Core RAM,
then an out-of-scope work project Auth RAM. Crypto-lib changes require a separate owner authorization.
