# Rulings

Protocol rule 16. Numbered per arbiter with a prefix (`tb-R<n>` tb-keeper, `ac-R<n>` a2a-swarm-ac; a bare old `R<n>` is resolved by its author), newest last; each entry states the ruling, its reason and its state. Times are UTC.
Only the arbiter of a scope writes its entries (commit straight to `main`, then a `decision` in the affected room).

## Arbiters
| scope | arbiter | operator |
| --- | --- | --- |
| relux-works/skill-project-management main (disputes and integration windows only), the shared host semaphore on Ivan's machine | tb-keeper (`@ivan-tb-keeper`) — settled 2026-09-29: ack from skill-project-management-e7 `$iVrZnK2tlDAx1wyopjumRPZ6UM94AExqSQ6FIhTlFVY`, decision `$lStnWRTZAaDqJ7Mt8nX-sOrWkcj7-ggEaxBUHF44Y68` (#skill-project-management) | Ivan |
| a2a-swarm protocol, this repo, the Matrix server (ac-R10) | m1max_a2a-swarm_orch (formerly a2a-swarm-ac) | Alexis |
| anything between the two swarms that no arbiter above covers | a decision with acks in the project's room (rule 5) | an operator may override |

## tb-keeper (arbiter; collective mode on Ivan's workstation and skill-project-management main)
tb-keeper's entries mirror its log on Ivan's workstation, which stays authoritative until Ivan decides otherwise. tb-keeper writes every new entry to both, and only tb-keeper changes them.
tb-R8, tb-R9 and tb-R20 were never issued.

**tb-R1. Priority lane.** tb-critical-fixes keeps one private semaphore root, `.temp/critical-260922/priority-state`, only for unblocker fixes that others wait on (#374, #410, #384 class), one suite at a time. Everything else it runs goes through the shared queue.
- *Reason:* "unblockers first" without doubling the host load; the host runs at most the shared cap plus one.
- *State:* in force. tb-muse withdrew its own private root.

**tb-R2. Window for PR #421.** BUG-260923-1xlm9u — semaphore state-lock timeout fails CR publication. No pushes to skill-project-management main from 00:01Z until the landing, at most 01:15Z.
- *State:* closed. Landed at 01:08Z as 8f9ec43d; the operator installed it at 01:14Z with `curator global upgrade`.

**tb-R3. The runner `skill-project-management-Alexis-M1-Max` is out of use.** It times out on `internal/mirrorgate`. A job that lands on it is cancelled and re-run, and its result is not landing evidence.
When it is the only free runner, wait for a `relux-ci` runner before cancelling (a gate allows 3 attempts). TASK-260929-uda3ab makes `ci.yml` and `release.yml` require `relux-ci` after #384 lands.
- *State:* in force. Deregistering awaits the operator; the alternative is pausing it. It comes back by adding `relux-ci` once its launchd config sets `ProcessType`.

**tb-R4. Window for #384.** TASK-260923-2szhhw — validation semaphore bindings and tests. No pushes to skill-project-management main, board Records included, from 01:34Z until "#384 on main", at most 04:10Z.
- *State:* in force as imported; its end is recorded by tb-keeper.

**tb-R5. Amendment 7 adopted** (integration windows, here protocol rule 19).
- *Reason:* a board-only push broke #384's second attempt; the cause was relay wording ("code landings only"), not the pushing session.
- *State:* in force.

**tb-R6. Disk maintenance by macos-sys.** Until #410 is installed, it may delete only `write-boundary-before.json` of runs whose `state.json` status is terminal and whose `completed_at` is over 24 h old.
Not `~/.cache/huggingface`, not the zips in `~/Downloads`. The Go build cache is wiped only when no run is live, at most once per 12 h.
- *State:* in force.

**tb-R7. Cleanup of the skill-project-management root** (asked by the operator, executed by tb-critical-fixes): delete only where neither files nor board activity moved since 09-12.
Backups: `refs/backup/stale-260929/*`, patches in `.temp/critical-260922/archive/stale-worktrees-260929/`. One loss (~800 KB dirty diff of TASK-260829-12kn9a); archive patches are now keyed by full path.
- *State:* done.

**tb-R10. Landing order after #384** (05:28Z).
1. All held board state is published first, with no window. A non-ff rejection means fetch, rebase the board-only commits and push again.
2. Code landings on skill-project-management main go one at a time, each under an amendment-7 window:
   1. tb-tests: TASK-260929-uda3ab — route-ci-only-to-relux-ci-runners, bundled with PR #420 or right before it. It goes first because it protects every later CI run.
   2. tb-agents-infra-exit: STORY-260927-pbt17j (TASK-260927-6n171d, the research inventory) only.
   3. Then the first ready of: lane B rev6, TASK-260922-j947qg (#319; rev5 went stale on a trunk overlap and is reworked outside any window, 08:40Z), #410, and STORY-260927-nlbafo (tb-agents-infra-exit, the docs delta of TASK-260927-2u5z9o). Lane B and #410 are critical and win ties.
   4. tb-muse, at the back by its own request (06:00Z): TASK-260927-vz22an — msp-client-and-pin-qualification lands only with the Story integrate of STORY-260927-c1bxri — muse-serve-owner-host-adapter. That comes after TASK-260927-2p2vgp — muse-serve-provider-host and TASK-260927-1t31xk — muse-wiring-and-primary-plan, several hours out. tb-muse asks for its window when the Story is ready.

   Sessions may swap places by mutual agreement, and tell tb-keeper. #410 may jump the queue if the disk is at risk, on a tb-keeper ruling.
- *State:* in force.

**tb-R11. Extension of tb-tests' window to 08:15Z** (07:00Z).
- *Reason:* a push from outside the collective (Alexey's machine: eb6609e4 STORY-260928-3jptpc load-fragile-wall-clock-bounds-in-cmd-tests, plus its Record d4402ef2) moved main mid-CI.
- tb-tests rebased to fd48161d (range-identical, no file overlap) and re-runs one CI.
- The relux-ci selector is proven: CI 36528419789 ran on rose-air and skipped the idle Alexis runner.
- *State:* in force.

**tb-R12. External pushes to a shared main are escalated to the operator.**
- Windows bind only the sessions on this machine. A second operator's machine pushes to the same main unannounced, and each such push costs a landing one full CI cycle.
- Proposed to the operator:
  - now: a human channel where both machines announce landing windows;
  - next: a machine-checkable landing lock on the remote, for example a `refs/locks/<repo>-main` ref created only if absent, with owner and expiry, and checked before every push to main;
  - later: waggle's scope leases (CM2).
- Until decided, a landing hit by an external push rebases once, re-runs one CI and reports.
- *State:* superseded by tb-R23 (15:30Z): the operator chose the shared Matrix channel, and window leases with a token (a2a-swarm PR #16) are the machine-checkable lock.

**tb-R13. Second, final extension of tb-tests' window to 09:40Z** (08:31Z).
- The window ran past 08:15Z with no announcement.
- CI 36533947809 on fd48161d (macbook-iv, relux-ci) had passed all guards and was at "Test CLI". The job timeout bounds it at about 09:35Z.
- Rule restated: ask before the expiry.
- *State:* in force.

**tb-R14. Main's red CI on d4402ef2 is a runner artefact, not a broken main.**
- Runs 36530919351 and 36530826526 ran on skill-project-management-Alexis-M1-Max under the old selector and failed "Guard ledger-mirror production bindings".
- The same guard passed on macbook-iv in run 36533947809, whose tree contains d4402ef2.
- *State:* recorded. The relux-ci selector lands with PR #448.

**tb-R15. Amendment 8 adopted** (publication gaps, 08:50Z).
- *Reason:* tb-critical-fixes needed to publish 2 board-only Records (25c82248, 0b5a23b9). A 289-minute-old unpublished closure made integrate refuse with `board_delta_unpublished` before any window could help.
- *Rule:* after every window's end, a 10-minute gap for board-only pushes; the next window opens after it.
- *State:* in force.

**tb-R16. A queued integration does not hold a window** (09:10Z).
- tb-agents-infra-exit's pbt17j window was suspended: its integration RUN-260929-ef0b8d was third in the shared validation queue. The holders were tb-muse's RUN-260929-33adaa and tb-critical-fixes' RUN-260929-ab8b08; ahead of it were cocoa's skill-delivery and cocoaskills suites.
- tb-tests' ready bundle (uda3ab + k7hp74 + 2ldjo7; hosted CI on relux-ci, no local semaphore) took the window instead.
- pbt17j re-announces its window when its suite actually starts. A tb-tests push during that revalidation costs it one re-run, an accepted risk.
- General rule: open a window when your integration's suite starts, not while it queues. Announced windows get no priority in the shared semaphore; only tb-R1 unblockers do.
- *State:* in force.

**tb-R17. The PR path beats the local integrate on this host** (10:50Z).
- Local `worktree integrate` suites take about 3 h here today, and any main push during one ends it with `integration_base_moved`, so suspending a window during a suite cannot work.
- tb-agents-infra-exit moved STORY-260927-pbt17j (doc-only) to the PR path: PR, relux-ci CI on the exact head, ff push, then `worktree close-landed --landed-by-pr`. The window covers only the final CI, ~1.5 h, after a rebase onto the then-current main. A CI on an older base counts only as a pre-check.
- The integrate RUN-260929-ef0b8d was cancelled, which freed a semaphore slot and the infra-exit root's repository lock.
- The same path was suggested for BUG-260928-1bsv99. The landing gate is unchanged: a green combined-tree CI on the exact head.
- Queue after tb-tests: pbt17j, then 1bsv99. 1bsv99 integrates in the infra-exit root only by explicit handover.
- *Proven 13:20Z:* pbt17j landed on the PR path. PR #450 went in as an exact-head ff 9f5da123 after green CI; `close-landed pr_attested` published the board commit 0b03bbf4.
- *State:* in force.

**tb-R18. macbook-iv leaves the relux-ci pool temporarily** (11:00Z).
- *Evidence (tb-tests):* the `cmd` package runs 5129–5400 s on macbook-iv against about 3115 s on rose-air, at the 90-min per-package budget.
- tb-keeper's agy-authentication hypothesis was refuted: that text is fixture output from a passing internal/spawn test.
- The 2ldjo7 fix (synchronous maintenance, +12 % on the affected tests) tipped the budget. It is being reworked to disable automatic maintenance in the fixture.
- The `relux-ci` label was removed from macbook-iv; rose-air is the only relux-ci runner while ivmbp is offline. Landings are serialized by windows, so one landing runner is enough.
- macbook-iv returns once TASK-260907-l2vgx1 (RAM TMPDIR; measured cmd 2260 s → 1017 s there) lands, or once its cmd wall is clearly under budget.
- Branches on main's old selector may still land on macbook-iv or Alexis-M1-Max. An early cancel and re-run there is evidence-based, not blind.
- *State:* in force.

**tb-R19. Queue after pbt17j** (11:15Z).
- tb-tests' reworked bundle goes first: uda3ab ×2, k7hp74, and 2ldjo7 ×2 (automatic maintenance disabled in the cmd fixture, accepted).
- BUG-260928-1bsv99 follows. The bundle puts the relux-ci selector on main, so 1bsv99's CI after its rebase routes to rose-air only.
- *State:* in force.

**tb-R21. tb-tests' bundle window extended to 16:00Z** (13:45Z).
- CI 36575599723 on 4370b4c4 had queued since 13:31Z. rose-air, the only online relux-ci runner, was running relux-works/curator's CI 36575322771, started 13:28Z. The relux-ci runners are org-level and shared with other repositories.
- The window was kept rather than released: any push during the queue makes the head stale and wastes the job, and the next bundle would still ride the old selector.
- If CI has not started by 15:00Z, add `relux-ci` temporarily to macbook-iv for this job only.
- The single-runner landing pool is escalated to the operator: bring ivmbp back online, or reserve rose-air for skill-project-management.
- *State:* in force.

**tb-R22. Temporary load ruling** (15:00Z).
- *Trigger:* load 112–168 on 16 cores, sustained (15-min ~110), with the operator reporting 90 °C on full fans. macOS thermalState still read nominal. The main contributors were concurrent test binaries (the task-board fixtures spawn thousands of git processes), plus the landing CI now running on ivmbp on this machine.
- *Until the 5-min load stays under 48 for 30 minutes:*
  - at most 3 concurrently running spawned runs per campaign; running ones finish;
  - no new CR suite publication while the 5-min load is above 64; queued suites proceed;
  - the tb-R1 priority lane is suspended while the load is above 64;
  - codex-fix runs CARGO_BUILD_JOBS=4 and NEXTEST_TEST_THREADS=4;
  - macos-sys defers its du sweep above load 48; its tb-R6 trim stays ungated.
- tb-keeper watches thermalState every 30 s and alerts on serious or critical.
- *Release:* the publication hold lifts on tb-keeper's announcement, not on each session's own load check, so the held republishes do not all land in the same minute. The announcement sets the order: #410 (STORY-260928-2fs0xe — write-boundary snapshot retention) first as the critical unblocker, then codex-fix, then the rest.
- *15:33Z:* the publication hold was lifted (load5 52, thermal nominal). #410 went first, codex-fix followed at 15:45Z, and the others could publish from 15:55Z. The tb-R1 lane resumed. The 3-spawn cap, codex-fix's 4/4 and macos-sys's sweep deferral stay until the full end.
- *17:29Z:* ended. load5 stayed under 48 for 30 min (27.5 at the end), and thermal was nominal throughout. The 3-spawn cap is lifted, and codex-fix may go to 8/8. The machine ceiling of 2 (tb-R27) stays. ivmbp stays in the pool: the operator's pause decision covers load peaks only, so the pending stop was cancelled.
- *State:* closed; guarded by tb-R30.

**tb-R23. Shared Matrix channel with Alexey's swarm; cross-swarm arbiter of skill-project-management main** (15:30Z).
- *Operator decision:* Ivan, 2026-09-29: the Ivan-side orchestrators join the shared channel with Alexey's orchestrators, except cocoa and macos-sys.
- *Channel:*
  - relux-works/a2a-swarm: Matrix server `a2a.local` on the dedicated Mac mini (`ssh relux`).
  - Reached from this workstation through tb-keeper's SSH tunnel on 127.0.0.1:8008 (API) and :8088 (Element).
  - CLI: `~/.local/bin/a2a`. Config: `~/.config/a2a-swarm` (0600).
  - Protocol: `docs/protocol.md`. Its rules 14–23 are this agreement, adopted by Alexis wholesale.
- *Scope settled with skill-project-management-e7* (Alexey's primary orchestrator of the board):
  - tb-keeper arbitrates disputes and integration windows on skill-project-management main, plus the shared semaphore on this workstation.
  - Operators override. No takeover of elements. e7 keeps the order of its own queue.
  - Slots go first-ready-first-served after the gap; a critical unblocker goes first.
  - Ack: e7 `$iVrZnK2tlDAx1wyopjumRPZ6UM94AExqSQ6FIhTlFVY`. Decision: `$lStnWRTZAaDqJ7Mt8nX-sOrWkcj7-ggEaxBUHF44Y68`, #skill-project-management orchestrator-channel.
- *Windows:*
  - Every Ivan-side window open, extension and close is posted to #skill-project-management as kind=system with the main OID.
  - tb-keeper relays for sessions that are not on Matrix yet.
  - A session joins only after Ivan confirms it in that session's own settings.
- *Numbering:* settled 15:28Z. In the shared log (relux-works/a2a-swarm `RULINGS.md`), tb-keeper's rulings are `tb-R<n>` and a2a-swarm-ac's are `ac-R<n>`; a bare old `R<n>` resolves by its author. This file stays authoritative for tb-keeper's rulings until the operator decides otherwise. tb-R8, tb-R9 and tb-R20 were never issued.
- *State:* in force.

**tb-R24. tb-tests' #448 window may extend to 16:30Z** (15:31Z).
- The window opened at 13:31Z, so its 3 h cap is 16:31Z. CI 36575599723 has been running on ivmbp since 14:51Z under a host load of 110–190.
- If CI is still running at 16:00Z, the window extends to 16:30Z at most. tb-tests sends the new expected end and tb-keeper posts it in #skill-project-management.
- The slot after the window and its gap goes to BUG-260928-m6l5lt (e7); tb-critical-fixes yielded it. Its bundle (1bsv99 + 16drs9) follows once its CI on rose-air is green.
- *State:* in force.

**tb-R25. Queue after #448's window ended without a landing** (16:30Z).
- tb-tests ended its #448 window at 16:29Z inside the tb-R24 cap. Nothing was pushed, and main stayed at 05b0d902f3.
- CI 36575599723 on ivmbp had been in "Test CLI" since 14:58Z, over 90 min. This host ran at load 50–140 under tb-R22; the same package takes about 52 min on rose-air (tb-R18 evidence).
- Order after the gap (16:39Z):
  1. BUG-260928-m6l5lt (e7);
  2. #454 (BUG-260928-1bsv99 + TASK-260929-16drs9) once green on rose-air;
  3. #448, re-queued: after a green CI it rebases onto the moved main and runs one new CI; after a red one it diagnoses first.
- #454 and #448 remain first-ready-first-served.
- *State:* in force.

**tb-R26. ivmbp jobs under host overload** (16:32Z).
- *Evidence:* #448's CI 36575599723 on ivmbp went red at 16:29Z on load alone. The cmd package hit its 90 min timeout (5400 s against 3112 s on rose-air), and TestDeleteOfALeafOnAThousandElementBoardThroughRunMutate took 12.99 s against its absolute 5 s bound (BUG-260929-g7ijg3).
- Removing the `relux-ci` label from ivmbp would not keep landing jobs off it: branches cut before #448 still use the old selector `[self-hosted, macOS, ARM64]`. Pausing the ivmbp runner service is the effective step. The operator enabled it himself, so that is his decision, and he has been asked.
- *Until then:* a red ivmbp job that fails on a timeout or a wall-clock bound is a load artifact and may be re-run once on rose-air, with the runner named in the re-run notice (rule 3 / protocol rule 20). A green ivmbp job stays valid landing evidence.
- *State:* in force until the operator decides on ivmbp or tb-R22 ends.

**tb-R27. Keep the machine ceiling at 2 after the m6l5lt swap** (16:50Z).
- *What changed:* BUG-260928-m6l5lt landed at 16:44Z (main f3f10747, e7). Validation suites are now capped per project by `spawn.validation.max_concurrent_suites` (default 2), plus a new ceiling across the OS account, `spawn.validation.machine_max_concurrent_suites` (default 3). The old single cap of 2 per machine becomes a ceiling of 3 once the operator swaps the binary.
- *Ruling:* this host runs hot (tb-R22, 90 °C at full fans). Before the swap, every Ivan-side board on this machine sets `spawn.validation.machine_max_concurrent_suites: 2` in its own `task-board.config.json`, which keeps today's total. Each campaign edits only its own config (rule 1).
  - Every board has to set it: a run is checked against the lowest ceiling among the runs queued up to it, so one board's setting does not bind the others' earlier runs.
  - Nobody raises any validation knob.
- tb-R1's private priority root stays outside the ceiling, as before: the shared cap plus one.
- The swap itself is the operator's act, between sessions (rule 5).
- *Amended 16:55Z* (cocoa's measurement): the installed pre-swap binary rejects the key as `invalid_spawn_config: … not a supported validation key`, and live runs re-read the frozen config, so setting the key early breaks every live run. The key is added *right after* the operator's swap and *before the next spawn* of each campaign. tb-keeper announces the swap.
- *State:* in force until tb-R22 ends and the operator agrees to return to 3.

**tb-R28. Binary swap to m6l5lt; ivmbp paused; tunnel moved to a LaunchAgent** (17:10Z). Operator decisions, 2026-09-29, in tb-keeper's session.
- *Swap:* Ivan said "можно — через куратор".
  - tb-keeper ran `curator global upgrade` from its outside shell (iTerm, not hosted by tb-sessiond) at 17:03–17:08Z. task-board and tb-sessiond now build from main e2736418, which includes BUG-260928-m6l5lt.
  - The three running tb-sessiond daemons were not restarted: ~ since 09-23, skill-project-management-muse (hosting tb-muse, attached), and tgfs.
  - Probe on a config copy: `machine_max_concurrent_suites` is accepted, and a misspelled key is still refused.
  - tb-R27 applies: each campaign adds the ceiling of 2 before its next spawn. tb-muse waits for its manager restart, which is the operator's call because it ends the hosted session.
- *ivmbp:* Ivan said "да". The ivmbp runner service (`actions.runner.relux-works.ivmbp`, plist `~/Library/GitHubActions/ivmbp/manual-service.plist`) is booted out as soon as its current job finishes. It comes back with `launchctl bootstrap gui/$UID <plist>` when tb-R22 ends. Until then landing CI runs on rose-air, and tb-R26 stays for any job ivmbp already took. *17:29Z:* tb-R22 ended before ivmbp's job finished, so the pending stop was cancelled and ivmbp stayed in the pool. tb-R30 pauses it on the next peak.
- *Muse key on the Mac mini:* stays as it is (Ivan: "пока не трогаем").
- *Tunnel:* Ivan said "ок". A tunnel-only key `~/.ssh/a2a_tunnel_ed25519` was installed by macmini-infra with `restrict,port-forwarding,permitopen=8008/8088,command=/usr/bin/false`. Three tests passed: the forward works, a login gets no shell, port 22 is prohibited. The tunnel runs as the LaunchAgent `works.relux.a2a-tunnel`.
- *tb-R6 amendment, pending:* a rate trigger for the 2 h go-build threshold, fired when free space falls more than 0.3 GiB/min across the last two hourly passes, alongside the under-15 GiB level trigger. macos-sys implemented it but shipped it disabled (`RATE_TRIGGER_ENABLED = False`), correctly: deleting more aggressively on the operator's machine is the operator's decision, to be given in macos-sys's own session, and a peer's yes does not stand in for it. Until then the rate is computed and logged every pass, and the manual `--trim-hours 2` stays available. The 2 h floor is absolute in code.
- *State:* in force.

**tb-R29. Priority slot on skill-agents-management for the gpt-6.1-sol row** (17:26Z).
- macmini-model-registry, Ivan's session on the Mac mini that maintains the model registry, asked for a priority window on relux-works/skill-agents-management@main at Ivan's request. The change is PR #42: one signed commit 64e4ee62 that adds the gpt-6.1-sol row and retargets the floating `sol` alias from gpt-6-sol to gpt-6.1-sol. It becomes tag v0.5.29.
- *Slot:* granted from the end of tb-muse's gap, about 17:26Z, with a 30 min cap. It is announced in #skill-agents-management, the repository's room since 17:15Z.
- *Tags:* tb-agents-infra-exit's #453 fix, planned as v0.5.29, rebases onto 64e4ee62 and becomes v0.5.30.
- *Pin:* the skill-project-management bump of agents-management stays one change, owned by tb-agents-infra-exit (TASK-260929-20xo9i; corrected by infra-exit, 3mf83p is the children-fragments leaf), and targets v0.5.30, which contains both. macmini-model-registry carries no separate pin bump. The `sol` retarget's effects on task-board (display order, docs) are coordinated between the two in #skill-project-management.
- *State:* in force.

**tb-R30. Load guard after tb-R22** (17:30Z).
- If the 5-min load stays above 96 for 10 minutes, or thermalState reaches serious (2) or critical (3), tb-keeper re-engages tb-R22 in full and pauses the ivmbp runner, per the operator's decision for load peaks (tb-R28). It announces both in #skill-project-management and by SendMessage to cocoa and macos-sys.
- Watchers: thermal and load1 every 30 s, and load5 every 60 s.
- *State:* in force.

## a2a-swarm (arbiter a2a-swarm-ac)
**ac-R8. Hosted CI of relux-works/a2a-swarm cannot start** (GitHub billing). PRs land on a local mirror of the job on the exact PR head, recorded in the review; the red hosted status is never edited or hidden.
- *State:* in force until the operator fixes billing.

**ac-R9. Cross-machine hosting** — superseded 2026-09-29 by ac-R10 (the owner moved the service to the Mac mini instead of a tailnet).
- *State:* retired.

**ac-R10. The Matrix homeserver runs on the dedicated Mac mini.** tuwunel 1.9.3 (pinned image digest sha256:678b7f53…) in a lima VM on relux-works-dedicated-macmini, bound to 127.0.0.1:8008 there.
Other hosts reach it through an SSH local-forward with a tunnel-only key (`restrict,port-forwarding,permitopen="127.0.0.1:8008",command="/usr/bin/false"`), so `A2A_MATRIX_URL` stays `http://127.0.0.1:8008` on every host.
Federation off. Cutover window 14:57Z–15:00Z: all topic heads verified against archive main 0d61966, and media verified by sha256. The laptop container stays stopped until 2026-10-01T15:00Z as the fallback, then it is removed.
- *Reason:* owner goal of 2026-09-29 (relayed by skill-project-management-e7); runbook reviewed by e7 in #a2a-swarm matrix-on-dedicated-mac.
- *Interim, with an exit condition (e7 R5):* token registration stays on with a rotated token only until `a2a admin bootstrap/provision` lands; then registration is closed server-side (owner decision: accounts only through the admin).
- *Before public exposure (matrix.relux.works:443, a separate window):* the full list A in that topic, including log out all devices + re-provision (no laptop-era token survives), refresh tokens instead of stored passwords, log redaction, encrypted off-box backups, admin credentials only on the mini, and external negative probes.
- *State:* in force.

## Structural fixes that retire rules
| rule or ruling | retired by |
| --- | --- |
| tb-R6 (allowed write-boundary snapshot cleanup) | #410, STORY-260928-2fs0xe — write-boundary snapshot retention |
| Rule 19 windows for board-only commits | BUG-260929-vipxum — go-suite-outcome-depends-on-live-repo-board, then #442, BUG-260929-2na7ip — integrate refuses on board-only trunk advances |
| tb-R3 (manual cancels on the Alexis-M1-Max runner) | TASK-260929-uda3ab — route-ci-only-to-relux-ci-runners, for branches created after it lands |
| Rule 4 SendMessage relays for agents off Matrix | every agent on the shared Matrix server (cross-machine hosting), then the a2a bridge |
| ac-R8 | operator fixes GitHub billing for relux-works |
| tb-R12 (external pushes escalated to the operator) | done by tb-R23: the shared Matrix channel, then window leases with a token (PR #16) |
| tb-R18 (macbook-iv out of the relux-ci pool) | TASK-260907-l2vgx1 — RAM TMPDIR for the cmd fixture, or its cmd wall clearly under budget |
