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

**tb-R31. #454 re-runs on rose-air; its window runs past the cap; ivmbp paused for the peak** (18:46Z).
- PR #454's CI 36601212209 on ivmbp (17:05–18:43Z) went red on two load artifacts: TestDeleteOfALeafOnAThousandElementBoardThroughRunMutate at 6.49 s against 5 s (BUG-260929-g7ijg3), and Test CLI timing out at 1 h 30 m. The bundle touches neither.
- The host was back at load5 90, close to tb-R30's threshold, so the operator's pause-for-peaks decision applies. The ivmbp runner service was booted out at 18:46Z, which lets the re-run land only on rose-air (tb-R26, one re-run).
- The window is extended from 19:55Z to 21:00Z, past the 3 h cap. Justification: a runner-only re-run of an unchanged exact head (9fe8d3b7) over an unmoved main (e2736418), with no rebase and no new review. #448 is next after its gap.
- Evidence: ivmbp timed out Test CLI even at moderate host load. Until the host is quieter, rose-air is the landing runner for skill-project-management.
- *Resume ivmbp:* `launchctl bootstrap gui/$UID ~/Library/GitHubActions/ivmbp/manual-service.plist`, once load5 has stayed under 48 for 30 min.
- *State:* in force.

**tb-R32. tb-R22 re-engaged under the tb-R30 guard** (18:52Z).
- *Trigger:* load reached 144/137/110. Free disk fell from 45 to 26 GiB in about 50 min, as go-build grew from 30 to 47 GB under about 22 concurrent `go` processes from campaign suites. The tb-R30 watcher confirmed the trigger at 18:56Z: load5 above 96 for 10 min, 128 at that point.
- tb-keeper re-engaged tb-R22 in full at 18:52Z: 3-spawn cap, publication hold above load5 64, tb-R1 lane suspended, codex-fix at 4/4. ivmbp had already been paused at 18:46Z (tb-R31). macos-sys's manual 2 h trim freed 16.6 GiB; free space is back to 39.4 GiB, and the cache is at its roughly 31 GB floor.
- *Structural:* the cache grows 17–20 GB/h entirely inside the 3 h window, so the hourly 3 h pass collects nothing. The fix needs the operator, in macos-sys's own session: a standing 2 h threshold and a 30 min cadence, or the rate trigger. It has been asked.
- *Release:* announced by tb-keeper, with #410 and codex-fix ordered as before, once load5 has stayed under 64 for 3 min. Full end once load5 has stayed under 48 for 30 min.
- *19:14Z:* publication hold lifted, staggered: critical items at once, codex-fix from 19:20Z, the rest from 19:25Z. *19:43Z:* full end, with load5 at 14. The tb-R30 guard is re-armed.
- *State:* closed.

**tb-R33. #410 lands right after #454, ahead of #448** (19:30Z).
- #410, BUG-260928-2x932g — write-boundary snapshot retention, is accepted: rev7, reviewer RUN-260929-dc76e4, mutants B–F killed. tb-critical-fixes has its landing commit 75502715 prepared on fresh main.
- *Order after #454's window and gap:* #410, then #448, then first-ready among BUG-260928-2qon2t and BUG-260926-31ckqk (e7), BUG-260928-2hgfo9 (after 31ckqk), STORY-260927-nlbafo, and TASK-260929-20xo9i (the agents-management pin bump to v0.5.30).
- *Reason:* critical unblockers first (rule 3). Disk is today's top host problem, and #410 retires tb-R6.
- *State:* in force.

**tb-R34. ivmbp stays paused through the next two landing CIs** (19:44Z).
- ivmbp timed out Test CLI at 90 min even at moderate load (tb-R31). The next two landings, #410 and #448, run CI under the old `[self-hosted, macOS, ARM64]` selector, which ivmbp would match.
- ivmbp therefore stays booted out until both landing CIs have run on rose-air.
- Once #448's relux-ci selector is on main, tb-keeper removes the `relux-ci` label from ivmbp, so skill-project-management landings stop routing there. ivmbp's service is then bootstrapped again for the organisation's other repositories.
- *State:* in force.

**tb-R35. Alexis-M1-Max diagnosed and fixed; its route back to the landing pool** (20:16Z).
- *Cause, confirmed by m1max_gha-runner_orch:* the runner's LaunchAgent had no ProcessType, so launchd spawned jobs as `spawn type = daemon` at priority 20 instead of 31. Under load, the internal/mirrorgate gate (about 20 serial child `go test` builds under one 10-minute package timeout) starved and panicked.
  - Bench: without ProcessType at load up to 133 it timed out; with Interactive under the same load it passed in 369 s.
  - CI evidence: three timeouts on the M1 Max, then two passes of 294 s and 114 s.
- *Fix:* `ProcessType = Interactive` added to the runner plist, with a backup kept. The service stays booted out until #448, the relux-ci selector, is on main.
- *Return:* after #448, the service is bootstrapped with the `relux-ci` label and runs one verification CI that covers internal/mirrorgate. If it is green, the runner is back in the landing pool with no further ruling. This retires the manual-cancel part of tb-R3.
- *Follow-up for skill-project-management:* a per-child deadline for the mirrorgate gate's child `go test`, offered to tb-tests.
- *State:* in force.

**tb-R36. No ad-hoc wide test sweeps** (20:25Z).
- *Evidence (macos-sys, at load5 14):* three concurrent uncached Go test runs wrote about 14 GB into go-build in about an hour: `./cmd -run ^TestS -count=1`, `-p 1 ./internal/... -count=1` and a `-mod=mod ./pkg/agentic` run.
  - Precision: `-count=1` disables only the test-result cache. The entries come from distinct source trees, distinct flag sets and freshly linked test binaries.
  - Such runs bypass the validation semaphore and its ceiling of 2, so tb-R27 cannot bound them.
- *Rule,* as in skill-project-management's CLAUDE.md:
  - while iterating, test only the touched packages, plus the uncached guard targets that cover the files changed;
  - run the full suite exactly once, before the CR is published or the PR updated. The CR validation suite and the landing CI are the full runs.
  - No ad-hoc `./...` or `./internal/...` sweeps, and no repeated `-count=1` full passes per edit.
  - Campaigns put this into producer and reviewer briefs for new runs. cocoa applies it to its Go repositories.
- *Result of that pass:* macos-sys's manual 2 h trim freed 18.4 GiB (cache 45 → 27 GB, 49 GiB free). A standing 2 h threshold still waits on the operator.
- *State:* in force.

**tb-R37. The M1 Max returns now; its first landing CI is the verification** (20:33Z). Supersedes tb-R35's hold.
- *Asked by Alexis:* the second runner is critical. The ProcessType fix is bench-verified under load.
- Rulings:
  - skill-project-management-Alexis-M1-Max is bootstrapped now, and its default labels make it a second landing runner.
  - The first skill-project-management landing CI it takes is the verification run, watched live by m1max_gha-runner_orch.
  - If mirrorgate or Test CLI times out, the runner is booted out at once and the job is re-run on rose-air (tb-R26 budget).
  - If it passes, it stays in the pool and gets `relux-ci` once #448 is on main.
- #410's CI is already on rose-air, so the verification job will be #448's or a later one.
- *State:* in force.

**tb-R38. Fallback for the manual go-build trim** (21:20Z).
- *Tonight:* five manual 2 h passes by macos-sys, each requested by tb-keeper, freed 67.4 GiB. The scheduled 3 h pass collects nothing at this build rate, and the standing threshold and the rate trigger still wait on the operator. macos-sys now logs manual runs to `~/Tools/cache-maintenance/logs/history.log`.
- *Fallback:* if macos-sys does not answer within about 10 minutes and free space is under 30 GiB, tb-keeper runs `python3 ~/Tools/cache-maintenance/cleanup.py --trim-hours 2 --by tb-keeper` itself as a one-off manual pass. `--by` only labels the run in history.log. Nothing is scheduled and `--force` is never used. The script enforces the 2 h floor and refuses load-adding passes while spawns are live.
- Automation stays the operator's decision.
- *State:* in force.

**tb-R39. #410's window extended to 23:30Z** (21:23Z).
- PR #457 (head 19def337) was picked by macbook-iv on its third try at 21:22Z, because rose-air was taken in the meantime. The run is about 1.5× slower there.
- The window opened at 20:01Z, so its cap is 23:01Z. It is extended to 23:30Z for a runner-only delay: head and main (2e7d611f) are unchanged, and #410 is a critical unblocker.
- A red on a timeout or a wall-clock bound means one re-run on rose-air (tb-R26), with the time it needs asked for before 23:30Z.
- *State:* in force.

**tb-R40. Lane B lands right after #410, then #448, firmly** (21:30Z).
- STORY-260923-dtftyp (TASK-260922-29gwk3 + TASK-260922-j947qg, the F3 policy-drift exit and current-policy revalidation, #319) is accepted as rev8. It is one of the goal's seven critical fixes, and its landing commit 17510583 is ready on main 2e7d611f.
- *Order:* #410, then lane B in its own window, then #448 (tb-tests, waiting since 16:29Z; not to be reordered again without a critical reason), then first-ready among 2qon2t, 31ckqk, 2hgfo9, nlbafo and 20xo9i.
- *State:* in force.

**tb-R41. Machine ceiling of 1 suite for disk** (22:00Z).
- *Evidence (macos-sys, 21:55Z):* a manual 2 h trim freed only 6.3 GiB. The go-build working set inside the safe 2 h window is 37.6 GiB (8.8 GiB of it written in the last 30 min), larger than free space (34.7 GiB), so trims can no longer create headroom. The write rate is about 19 GiB/h.
  - Attribution: CR validation suite shards over distinct worktrees (`./internal/... -p 1`, `./cmd -run ^T…`), which are not ad-hoc runs; tb-R36 is holding.
- *Ruling:* until free space stays above 60 GiB, or the operator frees the unowned data, each campaign sets `spawn.validation.machine_max_concurrent_suites: 1` before its next spawn. Running suites finish.
  - tb-R36 stays strict.
  - Manual 2 h trims continue hourly on request, with tb-R38 as the fallback.
  - This roughly halves the write rate, at the cost of longer CR queues. Landing CIs on remote runners are unaffected.
- *Operator decision pending:* the unowned 76.6 GiB (relux-tunnel 42.8 GiB), which is the real headroom.
- *Released 02:54Z, 2026-09-30:* free space held at 60 GiB or more for 60 min, and it is now 98 GiB. That came from the relux-tunnel prune (37 GiB) and macos-sys's pass after #448's CI (33.5 GiB). Campaigns may set `machine_max_concurrent_suites` back to 2 (tb-R27) before their next spawn, and no higher. tb-R36 stays strict. The #410 fix is live since the second swap (tb-R46); cancelled and early-failed runs still keep about 71 MB each (BUG-260930-2xq33i), and macos-sys's pass bounds that.
- *State:* released.

**tb-R42. #410 re-runs on rose-air; lane B may run stacked in parallel** (23:00Z).
- #410's CI on macbook-iv failed once: TestDeleteOfALeafOnAThousandElementBoardThroughRunMutate at 11.91 s against its 5 s bound (BUG-260929-g7ijg3, a known load artifact that #410 does not touch). Everything else was green, and head and main are unchanged.
- *Decision:* one re-run on rose-air (tb-R26), with the window extended to 00:45Z. The order stays as in tb-R40: #410, lane B, #448.
- *Optional stacking:* lane B's commit is stacked on #410's head, and its CI runs in parallel on the M1 Max (the tb-R37 verification run). If both are green, lane B lands right after #410 on that already-validated exact head, with no second CI.
- If the rose-air re-run is red again on g7ijg3 alone, the lander stops and asks; there is no third run.
- *State:* in force.

**tb-R43. macbook-iv paused for skill-project-management landings until #448** (23:05Z).
- Old-selector branches (everything cut before #448) keep landing on macbook-iv. It is too slow for the cmd package (tb-R18) and flaky on g7ijg3, and it grabbed #410's re-run twice in a row (22:56Z and 22:59Z) while rose-air was free.
- tb-keeper booted out the relux-works runner service on macbook-iv (host ivmbam1, plist `~/Library/GitHubActions/macbook-iv/manual-service.plist`). Its cocoaskills runner is untouched.
- *Resume:* after #448 lands, when the relux-ci selector keeps it off landing jobs, with `launchctl bootstrap gui/$UID <plist>` on ivmbam1.
- Same pattern as the ivmbp pause (tb-R31/tb-R34). For the operator: a pause on another of his machines, reversible with one command.
- *State:* in force.

**tb-R44. #410's rose-air result decides the next step; lane B waits** (00:24Z, 2026-09-30).
- Lane B's stacked CI (run 36642495007 on the M1 Max, 22:56Z–00:21Z) went red on one test only: TestDeleteOfALeafOnAThousandElementBoardThroughRunMutate at 10.93 s against its 5 s bound. That is BUG-260929-g7ijg3 — the delete test on a 1000-element board has a load-fragile absolute 5 s bound. #410's re-run has been on rose-air since 23:20Z, with a result expected at 00:30–00:50Z, at the edge of tb-R42's window.
- *#410 green on rose-air:* tb-critical-fixes lands #410 on that run. The window extends to 01:15Z for that landing only. Lane B does not land on its red run: it goes after #448. If tb-tests lands the g7ijg3 fix first, lane B rebases over both and runs one CI.
- *#410 red on g7ijg3 alone:* the window closes when the result arrives. tb-tests' g7ijg3 fix lands first. Then tb-critical-fixes rebases the #410 + lane B stack once and runs a single CI. #448 keeps its turn after the stack.
- *#410 red on anything else:* the window closes, and tb-critical-fixes reports the failure with evidence.
- No third run of #410 without a tb-keeper ruling (tb-R42).
- *Outcome:* #410 landed at 00:23Z, before the ruling reached its lander, on rose-air run 36623324102 (PR #457, exact-head fast-forward 2e7d611f..19def337). The window closed at 00:23Z, so the extension was not needed. Posted in #skill-project-management as a decision ($qhXHBhqJ…).
- *M1 Max evidence (tb-R37):* the whole suite completed. Its only failure was the load-fragile bound, which also failed on macbook-iv (10.3 s and 11.91 s). So the ProcessType fix holds as far as runner health goes. Adding relux-ci to the M1 Max is still decided after #448.
- *State:* in force.

**tb-R45. The M1 Max leaves the pool until the g7ijg3 fix is on main** (00:25Z).
- *tb-R37 verification: passed for runner health.* Lane B's run 36642495007 (job 109657762191) completed with no timeout. Test CLI took 79 min (package cmd 4754 s), and the mirrorgate guard 244 s. The only failure was BUG-260929-g7ijg3 at 10.93 s. The ProcessType fix holds.
- *The problem left is the bound.* Under agent load the M1 Max misses it, as macbook-iv does (tb-R43), while rose-air passed it (#410's run 36623324102). A red landing run costs every campaign a full slot.
- *Decision:* m1max_gha-runner_orch boots the runner out of the pool now, as with macbook-iv under tb-R43. It returns when the g7ijg3 fix is on main. The relux-ci label decision stays after #448. Lane B's job is not re-run (tb-R44).
- *Recommendation to tb-tests:* land the g7ijg3 fix before #448 or inside it. That restores two runners, the M1 Max and macbook-iv. The order of its own two items is tb-tests' call.
- *State:* in force.

**tb-R46. Second binary swap: main 9244d071, carrying #410 and #454** (00:37Z, 2026-09-30). This is the operator's decision (Ivan, 2026-09-30, in tb-keeper's session: "Go на замену бинаря через куратор — ок"). It is a per-event go, not a standing one.
- tb-keeper ran `curator global upgrade` from its outside shell (iTerm, not hosted) at 00:35:38–00:36:59Z. task-board and tb-sessiond now build from main 9244d071 (cache key 0348fd59…).
- The build carries two fixes:
  - #410, BUG-260928-2x932g — write-boundary before-snapshot kept after terminal;
  - #454, BUG-260928-1bsv99 — move chain refuses deleted old parent.
- Probes:
  - The new binary contains the #410 snapshot sweep, and the previous build c0332de2… does not.
  - `project_config()` answers.
  - All 8 running tb-sessiond daemons started before the swap and were not restarted.
- The swap was announced before and after in #skill-project-management. macos-sys was told by SendMessage; it keeps one sweep on its side before retiring its write-boundary pass.
- The same go also relayed Ivan's report that the Claude login on the Mac mini is done. remote-worker-harness-2b confirmed that both worker logins are done and that nothing is open for Ivan.
- *State:* done.

**tb-R47. ivmbp back in the pool while the host is quiet; it runs #448's CI** (00:44Z, 2026-09-30). Amends tb-R34.
- *Why now:*
  - #448's CI 36650900325 requires relux-ci. It sat queued, because the only online relux-ci runner, rose-air, runs curator's `Test (rose-air)` (run 36626413154, since 00:21Z), and curator's gate run 36651575754 may queue another rose-air job behind it. The window caps at 03:34Z.
  - The operator's decision covers pausing ivmbp at load peaks only (tb-R28, tb-R22's close). The host is quiet: load1 9–10, thermalState 0, 62 GiB free.
- *Checked:* ivmbp's launchd plist already has `ProcessType=Interactive`, and so does macbook-iv's. tb-R31's timeouts on ivmbp were real host load, not the M1 Max's QoS cause.
- *Done:* tb-keeper bootstrapped `actions.runner.relux-works.ivmbp` at 00:43Z. #448's job started on ivmbp at 00:44:02Z.
- *Guards:* tb-R30 stays armed. If load5 stays above 96 for 10 min, or thermalState reaches 2, ivmbp is booted out again. tb-R26 applies: a red ivmbp job that fails on a timeout or a wall-clock bound is a load artifact and may be re-run once on rose-air.
- *tb-R34's label plan* (drop relux-ci from ivmbp after #448) is re-decided after #448 lands, with this run as evidence.
- *State:* in force.

**tb-R48. Runner pool after #448: the M1 Max gets relux-ci; ivmbp and macbook-iv wait for curator's selector fix** (02:23Z, 2026-09-30).
- *#448 landed* at 02:22Z (exact-head fast-forward 9244d071..5792416b). Its CI 36650900325 was green on ivmbp in 98 min at quiet host load (tb-R47). Landing jobs now require `relux-ci`.
- *The M1 Max returns (tb-R45's condition is met).* The fix for BUG-260929-g7ijg3 — the delete test on a 1000-element board has a load-fragile absolute 5 s bound — is on main in #448, and its runner health was verified (tb-R37). tb-keeper added `relux-ci` to the repo runner skill-project-management-Alexis-M1-Max (id 21) at 02:23Z. It was already online.
- *ivmbp is out again* (booted out at 02:22Z, once #448's job had ended), as promised to ivan-curator.
  - curator's `Test (rose-air)` job still has `runs-on: [self-hosted, macOS, ARM64]` on curator main. Every main push would route it to any idle org runner, and it fails off rose-air (the host's git signing config).
  - ivmbp returns when curator TASK-260930-38fjt0 (pinning that job to `rose-air`) is on curator main and the host is quiet (tb-R28/tb-R30).
  - tb-R34's plan to drop relux-ci from ivmbp is withdrawn. #448 proved ivmbp is a sound landing runner at quiet load.
- *macbook-iv stays paused* (amends tb-R43's resume condition). It has no `relux-ci`, so skill-project-management landings would no longer reach it. But curator's old selector would, and that is where the original failure happened (the xcode-select gap and signing). It resumes when 38fjt0 lands.
- *Queue:* lane B (tb-critical-fixes: rebase once, one CI), then the first ready among 2qon2t, 31ckqk, 2hgfo9, nlbafo and 20xo9i. Landing runners: rose-air and the M1 Max.
- *State:* in force.

**tb-R49. F1 stacks on lane B inside lane B's window** (02:37Z, 2026-09-30). This is the tb-R42 pattern.
- *Request (tb-critical-fixes):* F1 is STORY-260922-2ijdmn / TASK-260922-108dhm — move/create atomicity prevention (#336/#342/#339). It is critical, on the operator's goal, and accepted as rev23 (reviewer RUN-260930-cd00da). It shares no paths with lane B or with main's advance, and it is stacked on lane B's head 6c063c7a as a0a3a3fc.
- *Decision:* yes.
  - F1's stacked CI is queued now and runs on whichever relux-ci runner frees first. At 02:36Z both were busy: lane B's CI 36660403841, and main's post-push CI 36660212832.
  - If both CIs come back green, lane B lands, then F1 lands at once on its exact, already validated head.
  - Lane B's window stretches to cover F1's landing, but never past its cap of 05:35Z.
- *Otherwise:*
  - Lane B red: F1 waits and is rebased with lane B's fix.
  - F1 red, or not green by 05:35Z: lane B lands alone, the window closes, and F1 goes back into the first-ready queue.
- *Fairness:* F1 goes next only because its CI validates the exact head that follows lane B. Nobody else can be validated on that head before lane B lands. After F1 comes the first ready among 2qon2t, 31ckqk, 2hgfo9, nlbafo and 20xo9i.
- *Outcome:* lane B landed at 03:51Z (2c4de555..6c063c7a, CI green on the M1 Max). F1 landed at 04:42Z (PR #465, 6c063c7a..a0a3a3fc, CI green on rose-air), and the window closed at 04:42Z, inside its cap. Both are task-board CLI changes, so they take effect after the next operator binary swap.
- *State:* done.

**tb-R50. ivmbp and macbook-iv are back** (04:39Z, 2026-09-30). This follows up tb-R48.
- curator TASK-260930-38fjt0 landed at about 04:37Z. curator main's `Test (rose-air)` now uses `runs-on: [self-hosted, macOS, ARM64, rose-air]`, checked on main. The host is quiet: load about 10, thermal 0, 100 GiB free.
- tb-keeper bootstrapped ivmbp's runner locally and macbook-iv's on ivmbam1 at 04:39Z. Both are online.
  - Landing runners (relux-ci): rose-air, the M1 Max and ivmbp.
  - macbook-iv has no relux-ci. It serves only generic jobs.
- *Stale job:* ivmbp at once took curator's run 36665690768, the `Test (rose-air)` of a push made before the pin (cbe52078 at 03:43Z, which had waited for rose-air). ivan-curator was told and decides whether it runs or is cancelled; tb-keeper does not touch it.
- tb-R28/tb-R30 still govern ivmbp: it is paused at load peaks, and the tb-R30 guard stays armed.
- *State:* in force.

**tb-R51. BUG-260930-1rn0hw takes the next slot, after the gap closes** (04:44Z, 2026-09-30).
- *Request (tb-tests):* BUG-260930-1rn0hw — mirrorgate child go test has no deadline and panics the package instead of failing the case. It was declared ready at 02:54Z: accepted, test-harness only, needing a rebase and one CI.
- *First-ready check:* none of 2qon2t, 31ckqk, 2hgfo9, nlbafo or 20xo9i has declared itself ready in #skill-project-management, and no other ready claim is open. So 1rn0hw is first.
- *Decision:* tb-tests opens its window at 04:52Z, or after the last gap-done post, whichever is later. The gap covers tb-critical-fixes' Records for lane B and F1 and its infra-exit root, then tb-agents-infra-exit's, if it publishes them in this gap. Nobody pushes Records into the window.
- *Next:* first-ready again. If a critical item (20xo9i, the v0.5.30 pin with #453 and gpt-6.1-sol) and a non-critical one are ready at the same moment, the critical one goes first.
- *State:* in force.

**tb-R52. TASK-260929-39rnkn is queued next after 1rn0hw; optional stacking** (05:00Z, 2026-09-30).
- *Request (tb-critical-fixes):* TASK-260929-39rnkn — reviewer-spawn freshness preflight (high, handed over by tb-agents-infra-exit) — is accepted as rev3 (reviewer RUN-260930-ba8370). Its landing commit is prepared on main a12285a4 and needs one CI.
- *Decision:* it is ready now, so it takes the slot after BUG-260930-1rn0hw (tb-tests' window, tb-R51). If 20xo9i (the v0.5.30 pin, critical) is ready at the same moment, 20xo9i goes first.
- *Optional stacking (tb-R42/tb-R49 pattern), only if tb-tests agrees:* 39rnkn may be stacked on 1rn0hw's exact landing head and run its CI in parallel on a free relux-ci runner (three are online: rose-air, the M1 Max and ivmbp). If both are green, it lands right after 1rn0hw inside tb-tests' window, which caps at 07:54Z. Otherwise it opens its own window after the gap.
- *State:* in force.

**tb-R53. Correction: 2qon2t goes right after 1rn0hw, ahead of 39rnkn** (05:30Z, 2026-09-30). This amends tb-R51 and tb-R52, and it is tb-keeper's error.
- *e7's claim holds.* BUG-260928-2qon2t — integration base moved when the control root is behind — was declared READY at 00:16:47Z. That was a notice addressed to tb-keeper ($gXyj3JbC…, PR #460, head 102ecde5, runner-revalidated). Under tb-R49's first-ready order it ranked ahead of BUG-260930-1rn0hw (ready at 02:54Z) and of TASK-260929-39rnkn (ready at 05:00Z).
- *Cause:*
  - tb-keeper's doorbell filter logged every notice without waking, including notices addressed to tb-keeper, and ready claims arrive as notices.
  - tb-R51's first-ready check scanned only the recent posts.
  - The filter is fixed: notices and offers addressed to tb-keeper now wake it. Future first-ready checks read the whole history of ready claims.
- *Decision:*
  - 1rn0hw's open window stands; e7 does not contest it.
  - The slot right after its end and gap goes to 2qon2t. 20xo9i (the critical v0.5.30 pin) goes first only if it is ready at that moment.
  - How 2qon2t is validated is its owner's policy (e7: rebase plus a local substitute check, about 10 min of main).
  - 39rnkn follows 2qon2t.
  - tb-R52's optional stacking of 39rnkn inside 1rn0hw's window is withdrawn, because it would jump 2qon2t.
- *State:* in force.

**tb-R54. The withdrawn stacked CI yields the runner to the open window** (05:31Z, 2026-09-30).
- tb-critical-fixes' stacked CI 36671089518 (PR #467, 39rnkn on 1rn0hw's head) took the M1 Max at 05:06Z. Meanwhile the open window's CI 36670857834 (1rn0hw) had waited since 04:53Z with no runner: rose-air and ivmbp were busy with other repositories' jobs.
- Under tb-R53 that stacked run can never be landing evidence. The ruling: its owner cancels it now, which corrects tb-keeper's "let it finish". The window's CI takes the first free relux-ci runner. tb-tests' window keeps its 07:54Z cap, and an extension is ruled on request if the queue time pushes past it.
- *State:* in force.

**tb-R55. Operator decisions: the shared protocol, the products spec, a standing swap go, the tb-muse restart** (12:50Z, 2026-09-30). Ivan in tb-keeper's session, verbatim: «27–31 да, 6(a) да, 6(f) да» — «Постоянное разрешение на замену бинаря под критические фиксы. — да» — «Перезапуск менеджера tb-muse — да».
- *Shared protocol:* Ivan's side decides yes on a2a protocol rules 27 to 31 at the heads under advice: #38 at 98799c8 (rule 27, the advice process, plus docs/advice-process.md), #39 at b6b1082 (rule 28, redlines), #40 at e0f514b (rule 29, problem first) and #41 at cd6e261 (rules 30 and 31). A later push that changes substance comes back to Ivan; pure wording does not.
- *Products spec (#37 at 57a1b1f):* yes on 6(a) and 6(f).
  - 6(a): bot-authored commits only in Alexis's own repositories. Shared repositories keep human-authored, signed commits, and remote orchestrators do not land into them until both operators decide.
  - 6(f): shared repositories stay outside single-operator product spaces and keep their named arbiter.
- *Standing go for critical-fix binary swaps on this workstation (ivmbp):*
  - tb-keeper runs `curator global upgrade` from its outside, unhosted shell whenever a landing carries a critical fix, batching where it can.
  - Before and after, it posts impact notices (rule 31), and it probes the new build.
  - Daemons are not restarted by this go. Each daemon restart still needs its own go, which ends hosted Claude sessions.
  - The Mac mini is not covered. Its swaps need Ivan's go in macmini-infra's own session.
- *tb-muse:* go to restart its manager (the older tb-sessiond daemon of skill-project-management-muse). It runs from tb-keeper's outside shell after tb-muse is at a safe point, and then tb-muse's session is resumed.
- *State:* in force. The standing go holds until Ivan withdraws it.

**tb-R56. README and reference hunks are announced in the window; git identity is never changed by agents** (13:05Z, 2026-09-30). This follows tb-agents-infra-exit's two problems, raised on Ivan's request.
- *README hotspot:*
  - README.md and references/*.md on skill-project-management main go stale in almost every landing. Today TASK-260929-20xo9i went stale twice and STORY-260927-nlbafo once, each costing a republish, a 1–2 h validation wait and a re-review.
  - *Now (process):* a window-open post names every README.md, references/** or .specs/** hunk the landing carries, by path and section. A campaign whose CR touches the same file rebases and re-verifies BEFORE review, not after acceptance.
  - *Tool fix:* sent to tb-critical-fixes as a proposal. For doc-class paths (README.md, references/**, .specs/**, docs/**), a clean three-way merge, and trunk hunks that don't overlap the CR's own, task-board re-parents and revalidates instead of marking the revision stale, and records the junction diff for the next reviewer. Priority is theirs.
  - *Restructuring README into include files:* that is a shared-repository structure change, so it goes to both operators under rule 27, later.
- *Git identity:*
  - Codex producer runs (RUN-260929-81ed80, RUN-260929-4da4a6) ran `git config` in a binding clone and changed the repository's user email and signing key. That produced a mis-signed snapshot and a `code_landing_identity_mismatch` refusal.
  - *Rule for every campaign:* producers, reviewers and every spawned run never run `git config` to change identity or signing, in any scope. Briefs say so explicitly. Orchestrators check that the binding's identity equals the control root's before every landing and completion.
  - *Tool guard:* sent to tb-critical-fixes as a proposal. A spawn postflight check flags any change to repo-local user.*, gpg.*, commit.gpgsign or includeIf during a run. The write boundary does not cover .git/config today.
  - *Finding (read-only sweep, 13:00Z):* 34 of 192 clones on ivmbp already carry a repo-local identity that differs from ivan@relux.works, most with the 'ivanopcode' signing key. skill-project-management's main clone and the wiki are among them. Which identity is canonical per repository is Ivan's call, and he has been asked. Until he answers, nobody "fixes" an identity anywhere: use what the control root is configured with, consistently. The personal email address is not written in any log.
- *State:* in force.

**tb-R57. Operator decision on git identity in relux-works repositories** (13:12Z, 2026-09-30). Ivan in tb-keeper's session, answering tb-R56's open question: option B, «Годятся обе личности, лишь бы внутри одной задачи клон-источник и рабочие клоны совпадали».
- *Both identities are valid for relux-works repositories:*
  - the relux identity from `~/.gitconfig`'s includeIf, `.gitconfig-thexflow` (ivan@relux.works with the relux signing key);
  - the ivanopcode identity (Ivan Oparin, his personal address, the ivanopcode key), as set locally in 34 clones.
- *The requirement is consistency within one task.* The control root and its binding clones use the same identity.
- *Nothing changes globally.* ~/.gitconfig and the existing local overrides stay as they are.
- *Mismatch handling:*
  - Spawned runs, producers and reviewers never run `git config` for identity or signing (tb-R56).
  - If a binding differs from its control root, its ORCHESTRATOR restores the binding to the control root's identity, copying the control root's values and inventing none. It says so in #skill-project-management before the next landing or completion.
- *State:* in force. The personal address is not written in any log.

**tb-R58. codex-fix may run three spawns in parallel** (13:27Z, 2026-09-30). This answers codex-fix's request, which the operator told it to bring to tb-keeper.
- *State of the load rules:* tb-R22 and tb-R32 are closed. The tb-R30 guard is armed: if load5 stays above 96 for 10 min, or thermalState reaches 2, tb-R22 re-engages in full (3-spawn cap, codex-fix at 4/4). The machine ceiling of 2 suites (tb-R27) stands. At 13:27Z load was 38/22/15 and 68 GiB free.
- *Decision:*
  - Three concurrent codex-fix spawns are allowed: A2 waiting in the semaphore, plus producers for STORY-260929-bohnqb (G1) and STORY-260929-wg1ya0 (B1), each in its own Story worktree.
  - While three run, each stays at CARGO_BUILD_JOBS=4 and NEXTEST_TEST_THREADS=4. 8/8 is for a single codex-fix build only; 16 cores cannot carry 3×8.
- *Rule 3 holds:* at most one full CR suite per campaign at a time, and the machine semaphore does not enforce it per campaign. codex-fix staggers on its own: a producer finishes and HOLDS its handoff and CR publication while another codex-fix suite runs. Scoped crate builds and tests inside producers may overlap.
- *Disk:* the 20 GB cap on codex-target, with a sweep after each suite, is fine. If free space falls under 50 GiB, codex-fix starts no new producer and tells tb-keeper. macos-sys's 45 GiB line stands.
- *State:* in force.

**tb-R59. PR #469's single re-run goes to a quiet runner** (13:45Z, 2026-09-30).
- *Evidence (tb-tests):* CI 36712178789 timed out the cmd package at 90 min on the M1 Max, which was also serving local models. Its integration package ran 2.4x slower than on rose-air (839 s against 343 s). #469 touches only tools/board-regress, so this is a runner-load artifact and tb-R26 allows one re-run.
- *Decision:*
  - tb-tests holds the re-run until rose-air is free, then posts before re-running.
  - At that moment tb-keeper checks ivmbp. If its load5 is under 20, ivmbp is acceptable too; it passed #448 at quiet load. Otherwise tb-keeper boots ivmbp out until the job has started on rose-air, then brings it back.
  - Pausing the M1 Max while it serves models is its owner's call (asked by tb-tests).
- *Structural:* cmd at 53 of its 90 min on the fastest runner leaves too little margin. tb-tests moves TASK-260907-2ik6zw (split the slow job) and TASK-260910-1okxol (shards) up its list; both are supported.
- *State:* in force until #469 lands or is withdrawn.

## a2a-swarm (arbiter a2a-swarm-ac)
**ac-R8. Hosted CI of relux-works/a2a-swarm cannot start** (GitHub billing). PRs land on a local mirror of the job on the exact PR head, recorded in the review; the red hosted status is never edited or hidden.
- *State:* RETIRED 2026-09-30: CI runs on the self-hosted repo-level runner m1max-a2a-swarm (set up by m1max_gha-runner_orch); first green hosted check landed PR #28 (main 87d5c6e). Landings again require a green hosted check on the exact head.

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
| ac-R8 | retired by the self-hosted runner m1max-a2a-swarm (2026-09-30) |
| tb-R12 (external pushes escalated to the operator) | done by tb-R23: the shared Matrix channel, then window leases with a token (PR #16) |
| tb-R18 (macbook-iv out of the relux-ci pool) | TASK-260907-l2vgx1 — RAM TMPDIR for the cmd fixture, or its cmd wall clearly under budget |
