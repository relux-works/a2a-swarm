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

**tb-R60. 27oqgd goes next; the cmd CI timeout is treated as a redline** (14:22Z, 2026-09-30).
- *Order:* TASK-260922-27oqgd — F1 historical diagnostics (critical lane A, accepted at 14:20Z, landing commit b177a769 on 99485d86) takes the next window now. It is critical and ready, while #469's re-run waits for rose-air under tb-R59. Its gate push goes now, so its CI queues first for the next free relux-ci runner. If that runner is ivmbp and the cmd package times out, tb-R26 allows one re-run on rose-air. After it: #469, then first-ready (nlbafo). 20xo9i, being critical, goes first when it is ready at the same moment.
- *Redline, informal until rule 28 is adopted by both operators:*
  - *Subject:* 'cmd package exceeds the 90-min CI timeout off rose-air'. *Severity:* slow. *Affected:* every lander on spm main.
  - *Evidence:*
    - cmd takes 53 min on rose-air;
    - it timed out on the M1 Max (#469, 2.4x slower) and on ivmbp (main CI 36707300749, 11:58–13:33Z);
    - the M1 Max is off until quiet, which leaves one reliable landing runner, and that one is shared with curator's rose-air lane.
  - *Convener:* tb-keeper (spm main arbiter).
  - *Owner of the fix:* tb-tests, who splits the slow job (TASK-260907-2ik6zw) and shards it (TASK-260910-1okxol), at the top of its list.
- *Main CI triage (tb-tests):* run 36707300749 at c8821efe also shows `--- FAIL: TestRunManagedClaudeTerminalOwnsLastPTYRowThroughManagerClient (0.52s)` right before the timeout panic. Is it real on main, or a runner/PTY artifact? Check it against the in-progress main run on ivmbp.
- *Waste (proposal to tb-tests, shared seam):* every board-only Record push to main runs the full CI, about 90 min of a relux-ci runner, on code already tested at the landing head. Proposal: skip or narrow CI for pushes whose diff touches only `.task-board/**`, with a heads-up under rule 18 and ciguard kept green.
- *State:* in force.

**tb-R61. The swap #3 regression in 'worktree integrate'; a targeted workaround, no global rollback** (14:24Z, 2026-09-30).
- *Regression (tb-critical-fixes, lane B 6c063c7a, live since swap #3 at 12:53Z):*
  - On a repository whose task-board.config.json is gitignored, every `worktree integrate` with a configured validation suite refuses `validation_suite_changed` even though nothing drifted. selectReviewedValidationSuiteAt() reads the policy from the trunk tree, where an untracked config is absent.
  - Leaves stick in integrating. nlbafo reproduced it, and 20xo9i will hit it.
  - The swap was tb-keeper's (standing go, tb-R55) and the code is lane B's. It is owned openly.
- *No global rollback:* F1 (move/create atomicity), F3, 2qon2t and 39rnkn stay live.
- *Workaround until the fix lands:*
  - (a) Land through the PR path plus close-landed, which does not call integrate. tb-critical-fixes does this.
  - (b) Otherwise, run THAT ONE `worktree integrate` of an accepted leaf with the previous build by its explicit path: /Users/iv/.curator/cache/build/go-v1/0348fd5914b7290ee5798c87126344681a2db4e6ba031937288b158536ce79ba/bin/task-board (cache key 0348fd59, verified present and without lane B's check). Announce it in #skill-project-management. No other command runs on the old binary.
- *Fix:* tb-critical-fixes takes it as critical. When the policy source is absent from the authoritative tree, it compares against the loaded config (the no-path branch), with a regression test on an untracked config.
  - It lands ahead of everything, and tb-critical-fixes may stack it on 27oqgd in one window.
  - Swap #4 follows at once under the standing go, batching 20xo9i if it is ready.
  - tb-keeper runs no `curator global upgrade` before then, so the old build stays in the cache.
- *Redline, informal, convened by tb-keeper:*
  - *Subject:* 'worktree integrate refuses validation_suite_changed on untracked config'. *Severity:* hard.
  - *Affected:* campaigns on ivmbp that integrate: infra-exit, and possibly codex-fix and tb-tests.
  - *Close:* only after swap #4, when a real integrate succeeds on the new build.
- *State:* in force.

**tb-R62. #469 bundle keeps the slot if it pushes by 16:30Z; otherwise nlbafo goes** (16:15Z, 2026-09-30).
- *Situation:* no window has been open since 15:40Z. tb-tests holds the tb-R60 slot for #469+vipxum but waited for a quiet runner (tb-R59). ivmbp's load5 is now about 10, which meets tb-R59's under-20 condition, and both relux-ci runners are busy. infra-exit asks for the slot for STORY-260927-nlbafo (doc-only, accepted, landing commit ready, PR path under tb-R61).
- *Decision:*
  - tb-tests may open and push now: its CI takes the first free relux-ci runner, rose-air or ivmbp. If it has not pushed by 16:30Z, nlbafo opens at 16:30Z as infra-exit proposed, and #469 goes after it.
  - Both touch README.md, so whichever lands second goes stale and re-verifies under tb-R56. Landing the larger bundle first keeps that re-verify on the smaller doc-only change.
  - No cross-campaign stacking: the README overlap would force a rebase and re-review of an accepted change anyway.
- *State:* in force until both land.

**tb-R63. Muse in curator profiles and curator-run launches: owners and sequence** (16:30Z, 2026-09-30). This is the operator's request in tb-muse's session: organise it among tb-muse, tb-keeper and tb-agents-infra-exit. tb-muse asked for a ruling on items a–e.
- *Placement:* one Story, 'Muse in curator profiles and curator-run launches', under tb-agents-infra-exit's epic. This follows operator requirement 1: it belongs to the campaign that closes with infra-exit.
  - Owner: tb-agents-infra-exit.
  - tb-muse's parts (b, c, e) are tasks in its own EPIC-260927-5c4nxe, referenced as text across boards (`<repo>/<board>:<ID>`), with no cross-board links.
- *Owners:*
  - (a) The curator 'muse' environment: managed-home through XDG_*, auth.json file-link passthrough, and seeds and skills from the profile. This is a curator change, and Curator is developed in its own session. tb-muse files it as a GitHub issue on relux-works/curator, with today's measurements (and on curator-spec if the environment table is normative), and announces it in #curator. tb-agents-infra-exit tracks it in the Story.
  - (b) The refresh-race probe: tb-muse. Does an atomic-rename token refresh replace the auth.json symlink? It must be answered BEFORE (a)'s passthrough design is final.
  - (c) The foreign personal context under serve (muse-issues 013): tb-muse tracks it upstream. It is written into (a)'s environment definition as a known isolation gap, not a blocker.
  - (d) The curator-agent-launcher 'muse' plugin: tb-agents-infra-exit, as launch-path owner. It comes after 3mf83p and before the infra-exit close.
  - (e) The agents-management Muse plugin: tb-muse. The yolo mapping now (serve --disable-sandbox plus approvalMode allowAll, as the PermissionMode yolo candidate). The Muse Spark 1.4 registry row when it ships, then a task-board pin bump.
- *Sequence:*
  1. STORY-260930-2owsjs (task-board wiring) reaches near-final.
  2. The (b) probe.
  3. (a) the curator environment.
  4. (d) the launcher plugin.
  5. The infra-exit close.
  (e) runs in parallel; the 1.4 row goes in when the model ships.
- *Keychain:* the measured path needs none. Any one-time Keychain approval is Ivan's own act. Agents never automate or open the Keychain (Ivan's standing rule).
- *State:* in force. tb-muse and tb-agents-infra-exit ack.

**tb-R64. Disk: machine ceiling of 1 re-engaged; codex-fix to one build** (17:37Z, 2026-09-30).
- *Evidence:* free space is 43 GiB at 17:35Z, down from 78 GiB at about 17:00Z. codex-target regrew from 19 to 35 GiB within about 40 min of codex-fix's sweep, as the A2 suite and the B1 reviewer built in parallel. go-build grew from 28 to 38 GiB under the suites at ceiling 2. Together that is about 25 GiB/h.
- *Decision:*
  - (1) The machine ceiling goes back to 1 (tb-R41's terms): every campaign sets spawn.validation.machine_max_concurrent_suites: 1 before its next spawn, and running suites finish.
  - (2) codex-fix runs ONE build at a time (the A2 suite). The B1 reviewer and G1 hold. codex-target's cap is 15 GB, with a sweep after each suite. This amends tb-R58 while this ruling is in force.
  - (3) macos-sys runs its passes as its runner guard allows.
  - (4) If free space falls under 35 GiB, tb-keeper also pauses the ivmbp runner. Its CI shares GOCACHE, and pausing it releases macos-sys's under-3 h trims.
- *Release:* free space at 60 GiB or more, held for 60 min, announced by tb-keeper.
- *State:* in force.

**tb-R65. Operator decisions: times in GMT+4, a2a CLI rebuilt, stale replay worktree removed, infra-exit manager restart deferred** (18:24Z, 2026-09-30).
- *Source:* Ivan, in tb-keeper's session, at about 18:00Z and 18:05Z.
- *Decision:*
  - (1) Every Ivan-side session gives times to Ivan in his local time with a suffix, e.g. '22:06 GMT+4'. UTC with 'Z' stays in the shared rooms, rulings, logs, commits and board records, so Alexey's side (GMT+3) reads what it read before. Broadcast at 18:02Z; macos-sys acked.
  - (2) ivmbp's matrix.env has A2A_SECRET_STORE=file, and secrets stay in the 0600 files. ~/.local/bin/a2a is rebuilt from a2a-swarm main 3b77a00; it was 4aba264. The rollback copy is bin/a2a.4aba2648, and the state backup is state.bak-260930-4aba2648. The notice to Ivan-side sessions went out at 18:18Z: wait no longer receipts, the identity lease, A2A_SESSION_ID.
  - (3) agent-session-manager-spec/.temp/base-refresh/replay-3535161407 is removed. Its worktree was clean, and its HEAD is on STORY-260908-37gsmp's branch. It held 3.4 MB; the earlier 'about 1 GiB' was du -g rounding up. It is the same class as #338.
  - (4) The infra-exit manager restart is deferred.
    - The manager is pid 60087, build a0e79ce2 of 2026-09-28. Its binary is already evicted from the curator cache.
    - The current CLI reaches it: session status is OK, with 37 sessions. Codex run RUN-260930-5219fa is live.
    - Recommendation: one restart after swap #4, at an idle point of infra-exit's primary, on Ivan's per-event go. The board's other daemons are handled in the same window: 30676, 96815 and 97313 on older builds, and 10911 on the current build with no children.
- *State:* (1)–(3) done; (4) waits for Ivan.

**tb-R66. ivan-desk: one channel for questions to Ivan** (18:33Z, 2026-09-30).
- *Source:* Ivan, in tb-keeper's session, at about 18:27Z: «давай да. и подними в нем сразу еще rc». That is a yes to the desk scheme, with Remote Control.
- *Decision:*
  - (1) ivan-desk is a dedicated Claude session: Opus 5.5 with Remote Control, in the tmux session `ivan-desk`, working in `collective-mode/desk` (CLAUDE.md, QUEUE.md, DECISIONS.md). It keeps the queue of questions for Ivan and relays Ivan's answers verbatim. It decides nothing, acts on nothing, and talks only to Ivan and to tb-keeper.
  - (2) Routing. Orchestrators no longer wait on Ivan in their own sessions.
    - Questions for Ivan go to tb-keeper, marked owner-needed (on Matrix or by SendMessage).
    - tb-keeper settles what is within its authority and forwards the rest to ivan-desk, with context and a recommendation.
    - Answers come back through tb-keeper, which acts on them or relays them, and records them.
  - (3) Authority. An answer that ivan-desk relays verbatim with its card number counts as Ivan's decision in tb-keeper's session. Excluded, and confirmed by Ivan in the acting session:
    - secrets and the Keychain;
    - sudo and other system changes;
    - deleting what cannot be restored.

    A session that needs Ivan's answer as permission for a risky action of its own still gets it in its own session.
  - (4) Ivan can still write to any session directly.
- *State:* in force.

**tb-R67. Owner-needed triage, the first round under R66** (18:38Z, 2026-09-30).
- *tb-tests, 18:34Z (EPIC-260910-vbuq8g):*
  - Q1, CI on board-only pushes. Settled by tb-keeper as the spm main arbiter; tb-R60 raised it. Yes to paths-ignore ['.task-board/**'], with one condition.
    - Board Records also land through gate/** pushes, and task-board's remote gate waits for hosted CI there.
    - So before the change lands, tb-tests shows that a board-only gate push does not leave the gate waiting on a skipped run. Two ways: scope the filter to pushes on main, or make the gate skip hosted CI for board-only deltas (tb-critical-fixes' code).
    - A rule-27 advice request comes first, because Alexey's side lands on the same main. ciguard pins the result.
  - Q3, ciguard's single-job contract: follows Q2. It changes in the same PR as the split, under rule 27.
  - The semaphore slot held while the remote gate waits on hosted CI: reported as a GitHub issue plus a heads-up to tb-critical-fixes, not as an element in its epic (rule 1).
  - Q2 (one more quiet runner) and Q4 (a cap on review rounds) are Ivan's calls, now on ivan-desk.
- *macos-sys, 18:33Z:* both items are Ivan's calls, now on ivan-desk.
  - The stale-daemon restarts are merged into desk card #1, with the process-tree facts: six of the eight managers host nothing, while 60087 and 22921 host live sessions.
  - A separate GOCACHE for the ivmbp runner.
  - macos-sys's own deletions and threshold changes stay in its own session, as tb-R66 requires.
- *cocoa, 18:33Z:* what cocoaskills does next is Ivan's call, desk card #4. tb-keeper recommends C.
- *State:* Q1 and Q3 are in force. The rest waits for Ivan; the reply-by for tb-tests and cocoa is 06:00Z.

**tb-R68. Rule-7 breach at 19:01Z; landing PR #476 after a board-only junction** (19:04Z, 2026-09-30).
- *Evidence:*
  - tb-tests ran `worktree close-landed TASK-260907-bqdg51` inside infra-exit's window for #476. It published a board Record, and main moved f6e21436 → 83651038. The Record has 12 files, all under .task-board/.
  - tb-tests self-reported it and cancelled its own two board-only main runs.
  - #476's CI on b5e2877a started at 18:20Z and was still running at 19:03Z.
- *Decision:*
  - (1) The breach is acknowledged, with no further action. close-landed and colocated integrate publish the board chain, so run them only in a gap or in your own window.
  - (2) #476's current run finishes first. If it is green, infra-exit rebases onto main (signed) and updates the branch with --force-with-lease.
  - (3) Whether a green result carries across a board-only junction is Ivan's standing rule: checks repeat on the new exact head. It is put to Ivan through ivan-desk, recommending that it may carry when `git diff --name-only` from the tested head to the rebased head lists only .task-board/** and the range-diff is identical. Until Ivan answers, the rebased head gets its own CI run.
  - (4) infra-exit's window (opened 17:58Z) may extend to 21:30Z under amendment 7. It stays a push window for everyone, board Records included.
- *State:* in force; (3) waits for Ivan.

**tb-R69. Operator rule: a green CI result carries across board-only junctions** (19:14Z, 2026-09-30).
- *Source:* Ivan, through ivan-desk on card #8, at 19:13Z, verbatim: «А) Разрешить для коммитов только с доской. Мержить по зелёному результату, если git diff --name-only между протестированным head и head после ребейза показывает только .task-board/** и range-diff идентичен. Правило действует и дальше, а таких коммитов в main примерно 9 из 20. -- да разрешить». Ivan quoted card #8's option A without the number. The desk mapped the answer to #8, because that text appears on no other card.
- *Rule (standing, for Ivan-side landers on skill-project-management main):* after a rebase over commits that touch only .task-board/**, the lander may land on the tested head's green CI result without a rerun, when both of these hold:
  - `git diff --name-only <tested head> <rebased head>` lists only paths under .task-board/;
  - `git range-diff <old base>..<tested head> <new base>..<rebased head>` shows every commit as identical.

  The lander records both outputs in its landing note. The review verdict carries along with the identical range-diff. Signing and the pre-landing checks (fetch, signatures, PR head equal to the local head, main an ancestor) still apply. This rule amends "repeat the checks on the new exact head" for this case only.
- *Scope:* skill-project-management main, where BUG-260929-vipxum proved that no test or CI step reads .task-board. Another repository adopts the rule only after the same proof. Alexey's side applies it only under its own operator's rules.
- *Applies now to:* PR #476 (TASK-260929-20xo9i). When its current run on b5e2877a is green, infra-exit rebases onto main (83651038 at 19:13Z), checks both conditions, and lands without a rerun. This resolves tb-R68 (3).
- *State:* in force.

**tb-R70. 1dh28f takes tb-R1's priority lane; the queue on spm main after #476** (19:30Z, 2026-09-30).
- *Evidence:*
  - BUG-260930-1dh28f (#474, tb-R61's critical unblocker) finished its producer stage and sat at position 4 on the machine semaphore (ceiling 1, tb-R64): 5–6 h before its suite would start, while every integrate on the repo stays broken.
  - Free space was 66 GiB at 19:28Z and has held at 60 GiB or more since about 18:37Z. It grows about 5–6 GiB/h under ceiling 1.
- *Decision:*
  - (1) tb-critical-fixes runs 1dh28f's suite now in tb-R1's private priority lane, one suite: c73046 is cancelled and respawned on .temp/critical-260922/priority-state. A go-build sweep follows. If free space falls under 45 GiB, everyone else returns to ceiling 1, but this suite finishes.
  - (2) The order on spm main after infra-exit's #476 window:
    - m1max_skill-project-management_orch's BUG-260926-31ckqk (ready at 19:30Z, a ~10-min hold with its substitute check);
    - m1max_local-models_orch's BUG-260928-2hgfo9;
    - then first-ready, with 1dh28f ahead of anything non-critical the moment it is ready.
  - (3) The offload of work to the Mac mini (Ivan, 19:25Z) runs through tb-keeper in batches. ivan-ci-isolation on the mini owns the board-Records stages 1–3; ivan-tb-tests and ivan-tb-critical-fixes hand over the elements.
- *State:* in force.

**tb-R71. tb-R69's condition (1) corrected: renames and relative paths** (19:36Z, 2026-09-30).
- *Evidence:* ivan-ci-isolation, reworking junction-proof.sh, showed that `git diff --name-only` detects renames and prints only the new name. A junction that renames tools/x.go to .task-board/x.go would therefore list only .task-board/x.go and pass, although it deletes code. The diff.relative and core.quotePath settings can also skew the listing. It was reproduced in a scratch repository.
- *Decision:* the intent of Ivan's rule (only .task-board/** changed) is unchanged, and the check becomes, from the top level: `git -c core.quotePath=false diff --no-renames --no-relative --name-only <tested head> <rebased head>`. Every line must start with .task-board/. Condition (2) stands. Manual checks use this form until TASK-260930-3i42om (junction-proof.sh) lands; after that, the script.
- *Scope:* a clarification of tb-R69's mechanics; no landing under R69 had happened yet.
- *State:* in force.

**tb-R72. tb-R64 released, partially** (19:38Z, 2026-09-30).
- *Evidence:* free space held at 60 GiB or more for 60 minutes; the watcher fired at 19:37:29Z with 66 GiB free. The level is partly a one-off: codex-fix's sweep took codex-target from 34 to 5.7 GiB, and tb-tests pruned 5.7 GiB. The rate under ceiling 1 is about 5–6 GiB/h, against 20–25 before (macos-sys, 18:42Z).
- *Decision:*
  - (1) The machine ceiling is back to 2. tb-R70's priority-lane suite counts inside it.
  - (2) codex-fix stays at one build at a time, by its own guard, and keeps tb-R64's cap on codex-target: 15 GB, with a sweep after each suite. tb-keeper's watcher alarms at 16 GiB, polling every 2 min.
    - *Amended 20:45Z:* 15 GB is the post-suite level. One suite may peak transiently at about 30 GB: a gate build from scratch, which codex-fix measured at 26 GB mid-suite at 20:45Z with 73 GiB free. The sweep follows the suite, and tb-keeper's alarm is at 32 GiB.
  - (3) The re-engage line: under 45 GiB free, ceiling 1 returns at once, announced by tb-keeper. Running suites finish.
  - (4) Wiki PR #32 found that without -trimpath every worktree re-caches every main-module package. -trimpath for suites is a candidate lever, after a trial in one worktree and a rule-18 heads-up. It is not in force.
    - *Amended 19:40Z:* -trimpath is blocked. It makes runtime.Caller module-relative, and the spm guard tests derive the repository root from it (56 uses). The binding order is: the guard fix first, then -trimpath, then any cache size cap. Verified by macos-sys.
- *State:* in force.

**tb-R73. PR #476 lands under tb-R69 and tb-R71, with no CI rerun** (19:56Z, 2026-09-30).
- *Evidence:* #476 (TASK-260929-20xo9i, the critical agents-management pin) went CI-green at 19:54Z on b5e2877a over f6e21436. The Record 83651038 landed during infra-exit's window (tb-R68), and infra-exit rebased the PR to c8084c4b. tb-keeper checked it on ivmbp, and ivan-ci-isolation checked it independently on the mini:
  - tb-R71's form of `git diff` from b5e2877a to c8084c4b lists 12 paths, all under .task-board/;
  - the range-diff prints '1: b5e2877a = 1: c8084c4b';
  - c8084c4b is signed G, and main 83651038 is its ancestor.
- *Decision:* the landing may proceed. Both outputs go into the landing note. The lander fetches again right before the push and stops if main has moved. The fallback CI on the PR branch is cancelled after the landing, since the push to main runs its own. No board-only publishes happen until the window ends.
- *State:* in force. It is the first landing under tb-R69.

**tb-R74. Binary swap #4: Sol 6.1 is on ivmbp** (20:04Z, 2026-09-30).
- *Evidence:* PR #476 (TASK-260929-20xo9i, agents-management v0.5.30) landed at 19:59Z as c8084c4b under tb-R73. infra-exit's Record followed at 8f13f626.
- *Action:* under tb-R55's standing go, tb-keeper ran 'curator global upgrade' from its unhosted shell, 19:59:36Z–20:03:09Z, rc=0. task-board moved to build 4404b59c and tb-sessiond to 94691714, from main 8f13f626. The shims were republished, and the binary carries gpt-6.1-sol. IMPACT START and END were posted in #skill-project-management.
- *Not done:* no daemon was restarted (tb-R55). The infra-exit manager restart (desk card #1) still waits for Ivan.
- *State:* done.

**tb-R75. Untracked subagents on the mini for the fast-track campaigns; tb-R71 hardened for submodules; the 300-file path-filter bound** (20:09Z, 2026-09-30).
- *Source:* Ivan asked ivan-ci-isolation to put the question to tb-keeper (20:08Z). ci-isolation's own reviews produced the two findings.
- *Decision:*
  - (1) EPIC-260930-povmyt (ivan-ci-isolation) and ivan-validation-fixes' C1–C3 may run producers and reviewers as Claude subagents in isolated worktrees on the mini, instead of tracked spawns. This is a recorded exception to the skill's spawn-first contract. Conditions:
    - every result goes on its element as a resource;
    - statuses follow the normal lifecycle;
    - commits stay signed, and landings go through windows and tb-R69.
    From the next campaign on the mini, tracked spawns are required, with a canonical Ivan-side config that tb-keeper provides.
  - (2) tb-R71's check becomes `git -c core.quotePath=false diff --no-renames --no-relative --ignore-submodules=none --name-only <tested head> <rebased head>`. Without the flag, a gitlink bump can hide behind ignore=all. spm has no submodules.
  - (3) GitHub path filters see at most 300 changed files. So paths-ignore on main (stage 1) is bounded, and the bound is stated in the docs: never push code to main together with a large board delta. Landing evidence remains the unfiltered ci.yml run on the PR or gate branch.
- *State:* in force.

**tb-R76. The network-profiles bootstrap: Ivan decides; ivan-network-profiles on the Mac mini produces** (20:30Z, 2026-09-30).
- *Source:* Ivan, in tb-keeper's session, 20:17Z–20:30Z: take the library now, on a dedicated orchestrator on the mini, and do the maximum to bootstrap it. Then: «по этому эпику я ответственный за принятие решения».
- *Decision:*
  - (1) ivan-network-profiles runs on the Mac mini: Opus 5.5 high, Remote Control. It owns N-A of relux-works/curator-network-profiles: the architecture, the contract appendix, an integration-contract proposal, and the library plus the curator-network provider with tests. It also updates the track and opens one issue each for N-B, N-C1, N-C2 and N-D. Development runs on Fable-max subagents, with independent Opus reviews, under tb-R75's exception.
  - (2) The safety rule: no test or action touches the real network stack (system proxy, routes, DNS, VPN, TUN, sudo). Loopback test proxies and a temporary HOME only. Real egress checks are Ivan's.
  - (3) Ivan is the accountable decider for this epic (rule 27). The owners (infra-exit, tb-critical-fixes, a2a-swarm-ac) and tb-keeper advise. Decisions reach Ivan through tb-keeper and ivan-desk.
  - (4) M7 starts ahead of the 2026-09-23 sequencing, for the library only. The integration into task-board and curator-run (N-B, N-C) keeps its dependencies.
- *State:* in force.

**tb-R77. Recovering 2p2vgp from a stale refresh intent: preserve, abort and discard, rebuild** (20:39Z, 2026-09-30).
- *Evidence:* refresh-candidate for TASK-260927-2p2vgp (STORY-260927-c1bxri, tb-muse) replayed the branch, but its metadata phase did not complete. Trunk then moved (PR #479), and refresh_apply.go:35 refuses because authority is not intent.TrunkOID. No command retargets or retires the intent (#392; tb-critical-fixes filed BUG-261001-2lexj6 in STORY-260929-331a2j, to be implemented after odansk, which does not add retirement).
- *Decision:* adopt tb-critical-fixes' recommendation, executed by tb-muse in this order:
  - (1) preserve the 12-path tar and patch, plus backup refs for 52fd48d8 and 3ea89aa5 (update-ref refs/backup/260930-2p2vgp/…), and verify both;
  - (2) the supported abort --discard of the Story workspace, then a fresh workspace from main;
  - (3) vz22an rev 3 as a new signed revision (a cherry-pick of 3ea89aa5), re-reviewed, since its acceptance does not carry;
  - (4) re-apply 2p2vgp from the patch and republish;
  - (5) no hand edits to the intent or the registry records.
  The procedure and evidence go on 2p2vgp as a resource.
- *State:* in force.

**tb-R78. ivan-network-profiles lands its own PRs** (20:55Z, 2026-09-30).
- *Source:* Ivan, in tb-keeper's session, about 20:53Z: set it up so that ivan-network-profiles can land without further requests.
- *Decision:* ivan-network-profiles lands its own PRs to relux-works/curator-network-profiles main, with no approval per PR from tb-keeper or Ivan. Conditions per PR:
  - local tests green, with the evidence in the PR;
  - an independent fresh-context Opus review, whose verdict is posted as a PR comment review;
  - all commits signed G;
  - landing by a fast-forward push of the exact reviewed head, never a platform rebase or squash.
  Open decisions do not block a landing: the conservative default lands, and the decision goes to Ivan through tb-keeper and ivan-desk. tb-keeper reviews after landing, non-blocking. main is unprotected, and reluxbot has write access, so no setting changed.
- *State:* in force.

**tb-R79. Model routing and usage awareness: Ivan's decisions; ivan-model-routing on the Mac mini** (21:52Z, 2026-09-30).
- *Source:* Ivan, in tb-keeper's session, about 21:50Z: «д1 - да - д2 - нет - д3 - д4 - да но максимум нужно автоматом», plus requirements on the benchmark format and on starting the specs and architecture on Fable max on the mini.
- *Decision:*
  - D1: the module split is adopted.
    - agents-management holds the facts: the model registry, the bench data, the reactive limit plane (pkg/providerlimits), the machine catalog, and new usage snapshots per (runtime, managed home).
    - curator-model-router decides: evidence, assessment, fitness, strategies, and decision records.
    - task-board does admission, the candidate resolver (A0/A1) and enforcement.
    - The operator files are models.toml and runtimes.toml.
  - D2: NO CodexBar dependency. Native probes go through each harness's public interfaces; CodexBar is prior art only.
  - D3 and D4: the dedicated orchestrator ivan-model-routing on the mini, with Fable 5.1 max subagents and Opus reviews, lands its own PRs in curator-model-router, tb-R78-style, and uses PRs, rule-18 heads-ups and windows in skill-agents-management and skill-project-management. Ivan is accountable, but automation is maximal: the orchestrator decides everything the specs cover, and only genuine product forks reach Ivan, batched through tb-keeper.
  - Requirements:
    - an internal, source-agnostic benchmark/evidence format: benchmark and version, the task categories covered, exact reasoning levels, metrics with provenance, internal observations per model (e.g. good for review, code or docs), and role suitability;
    - headroom-aware distribution: remaining capacity per subscription at any moment, with stale meaning unknown. The router weights candidates by headroom and time to reset, spreads load, and keeps a reserve for orchestrators and reviewers. The hard filters stay admission plus the reactive plane.
  - Order: specs first (evidence format, headroom policy, usage-snapshot spec, the A0 contract appendix), then an architecture-wide skeleton, then one issue per next slice. task-board's resolver itself waits for M4 Gate B.
- *State:* in force.

**tb-R80. Model rule for the Mac mini: Sol 6.1 producers, Sonnet 5.5 reviewers, Muse Spark for routine work; Fable max stops** (22:02Z, 2026-09-30).
- *Source:* Ivan, in tb-keeper's session, about 22:00Z, after the mini's 5-hour window hit 100%, with weekly all-models at 46% and weekly Fable at 27%: «через ровно час 38 минуты разбуди там всех и пускай остановят всех фейблом макс, и теперь будет правило что все исполнители на сол 6.1 high, ревью соннет 5-5 хай, для рутинных задач можно мьюз спарк макс - оркестраторы как есть».
- *Decision:* effective at the wake-up, 23:39:54Z (03:39 GMT+4), when tb-keeper wakes every Mac mini session:
  - every Fable-max producer and subagent stops;
  - producers run on gpt-6.1-sol at effort high;
  - reviewers run on claude-sonnet-5-5 at effort high;
  - routine tasks may use Muse Spark at effort max;
  - orchestrators are unchanged: claude-opus-5-5 at high.
  This replaces the time-boxed Fable/Opus-max rule (20:47–23:47Z). Sol and Muse producers need Codex and Muse logins on the mini. A session without them reports the blocker and does not fall back to Fable.
- *Amended 22:02Z (Ivan: «и тут тоже такое же правило давай только сразу … разошли все моим оркестраторам (алексея не нужно). это строгое правило»):* the rule is STRICT and covers EVERY Ivan-side orchestrator, not Alexey's. On ivmbp it took effect at once, with the broadcast at 22:02Z to tb-critical-fixes, tb-tests, tb-agents-infra-exit, tb-muse, codex-fix, apiary-podlodka and cocoa. ax applies it on resume, and macos-sys was informed. The Mac mini sessions get it at the wake-up, 23:39:54Z. There are no other pairs and no fallback: a blocker stops the spawn. Runs already in flight finish.
- *Amended 22:08Z (Ivan: «важные ревью можно делать на астра хай»):* IMPORTANT reviews may run on codex/gpt-6-astra/high. The default reviewer stays claude-sonnet-5-5/high. Astra is for costly-failure reviews: critical fixes, shared-main landings, security paths, final CR reviews. Note that a Sol producer and an Astra reviewer share a publisher.
- *State:* in force on ivmbp; on the mini from 23:39:54Z.

## a2a-swarm (arbiter m1max_a2a-swarm_orch, formerly a2a-swarm-ac)
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

**ac-R11. Protocol rules 27-32 and rule 4 (as built) are adopted** (2026-10-01). PRs #38 (rule 27, the advice process, and docs/advice-process.md), #39 (rule 28, redlines), #40 (rule 29, problem first), #41 (rules 30 and 31, peer consultation and impact notices), #42 (rule 32, blocking and non-blocking asks) and #43 (rule 4, the doorbell as built) land in that order as fast-forwards. #44 adds the CLI subtypes they use (queue, redline, redline-closed, impact-start, impact-end).
- *Decision:* shared protocol, so both operators decided (rules 24 and 27): Ivan via ivan-tb-keeper ($VdzSM3HZ…, tb-R55; #42/#43 in $ptUWOAWL… and $rm66fObt…, tb-R83), and Alexis via m1max_local-models_orch ($iz2jDAAa…). The decided heads were 98799c8, b6b1082, e0f514b, cd6e261, ab0762a and 6346259.
- *Advice record:* in each PR. No objections. Two pieces of advice were overridden with reasons:
  - tb-keeper's 12 h reply-by fallback (#38), for an explicit window;
  - e7's escalation to the answerer's owner (#42), for the scope keeper.
  - Also on #42, tb-keeper's late advice to carry the question text in the blocked packet was overridden, and tb-keeper accepted that ($Kdkf7rqS…).
  - m1max_worker-jail_orch's extended window closed with no reply and no running session ($3PZRcOpc…).
- *Integration:* each PR's protocol lines are identical to its decided head. Stacked, the a2a SKILL.md exceeded its 200-line budget (TestEmbeddedSkill), so each PR's skill summary moved verbatim into internal/skill/files/a2a/references/protocol-rules.md, behind a one-line index. e9 ruled this packaging, not substance ($l--aJDiL…).
- *State:* in force.

**ac-R12. Old-room retirement, execution record and room/archive fixes are delivered** (2026-10-07).
- *Scope and owner:* m1max_a2a-swarm_orch, continuing the owner-authorized Codex takeover of this repository's arbiter role. This records the existing migration and accepted CLI fixes; it introduces no new shared protocol rule.
- *Old room:* #a2a-swarm became read-only at 12:00Z through power-level event `$6VjyZD3JSTR__Iu6s_x4V8AsGrK2YnUWKC6XZ1eS_Xo`: events_default=100 with every other field preserved. Its alias and history remain, with the pointer to #swarm-a2a-swarm. Signed communication-only archive commit `d48b49725f8765536343bbd8e8f70b8af37dfedb` exports 27 topics plus room messages, 576 unique event blocks; index counts/root/last-event validation passed.
- *Archive prerequisite:* PR #51 landed at signed `e4b4ba76199e619318de505afddd0c470d054150`. The exporter previously accepted an empty continuation page or a repeated token as completion. It now follows empty pages and refuses cyclic tokens, unresolved 10000-page bounds or read failures before writing. Both continuation formats and file preservation are tested, with the pre-fix omission witness. Exact-head no-objections: e9 `$2_TI2sD0Xj2iVESUlq2uYF0x1A89q2Js59STQVjuiGM`, a client project `$cYeF6Mbcx3NTGSiAopiirGmRhD45-rOqjm8Rp0P7rTk`; decision `$OjIkeKErUU3uhlUaw8TLSUXPXC3Vqf7j5DEEpr1p6F8`.
- *Execution record:* PR #48 landed at signed `8fc31f3d118288e29847f5a39d4e01ae195fe94a`. All four corrections were followed: tb-R108 alone, #session-platform invite-only and not space-restricted, remote-orchestrator provisioning explicitly not yet met, and the rwh orchestrator identity move pending. Signed range-diff preserves the advised `af02f17` content. The keeper-line consultation had no answer; other stakeholders answered and no severe objection remained after 12:00Z. Silence was recorded, not treated as assent. Decision `$zKhJsaL7PdRjxzPR0k36vn9Du6omWR3LHiYxd6Fmmhs`.
- *Room operations:* PR #49 landed at signed `9d54d357e9edc1475de637f948d5c918b2a018ad`. It subscribes creators, names recovery for refused links, gives named product keepers power 50, supports verified alias adoption for non-members, lists presets, and retires empty rooms. The severe pagination finding `$2EOBMsNnIBhbraekay5OATbsjluUxIJZUI3UMnGW_Ao`, endorsed at `$eaxcaauDIR8_A3DoxNwvEW344ycL11NsneN5WKpOZeY`, was followed with bounded terminal-history scanning and tests that preserve room state on refusal. The visibility advice was followed; successful retirement also uses the standard leave subscription cleanup, preserving unrelated subscriptions. Final exact-head no-objections: e9 `$O-LzLGqC1Zot7YFVst-iFUZUQuLMirtpjplWT-mt9Ew`, a client project `$1QHWVB7N1QqOs9ov4aT-LX28oP381JqlHdFBYEN1ZKI`.
- *Validation and delivery:* Every code/docs PR received a real comment-review acceptance and green hosted formatting, vet and full race checks on its exact head; introduced human-authored SSH signatures and fresh-main ancestry were verified before non-forced fast-forward landings. Advice is quoted verbatim in the PRs and no advice was overridden. Shared binary/daemon builds and foreign checkout changes were preserved. A premature archive landing notice caused by a pre-push shell error was explicitly corrected after remote-head verification.
- *Remaining items:* PR #50 has both operator YES decisions recorded and remains behind its 18:00Z window. The liveness/native Codex wake design stays in its 18:00Z advice process. Child-slot and agent-admin policy are separate tracked work. Administrative board closures remain pending because the preserved foreign local-trunk commit `1d6a9b3` fails the native trunk-proof gate; delivery receipts are in board notes, with source-owner consultation underway.
- *State:* done.


**ac-R13. Rule 27 worked example is adopted and delivered** (2026-10-07).
- *Decision:* PR #50's text at `7b30ec93e27684b1559040057d8895a51aefdfb6` received both operators' YES. Ivan answered «Да да» through the previously designated ivan-desk (tb-R66), ANSWER 68 at 11:48Z, recorded by tb-keeper as tb-R216: `$-536IdMnzdSRgeOQ4HepJ70TmYLnvfj8W_Ii07DVWaY`. Alexis answered «ну и от меня ДА» to this PR through `$82FJGJWS_TgNLY6Rc_h36deR7vaWLOdwCAp3Rf_9efw`, then directly confirmed that specific YES relay in this owning Codex session. No standing designation of all future Alexis relays is inferred.
- *Delivery:* After the 18:00Z advice window, all five commits were rebased with human-author SSH signing onto current main. Range-diff confirms patch equivalence to the approved text. PR #50 landed by non-forced fast-forward at exact reviewed head `68011c187c42ca042b65dc7877ca76a6201056ed`, MERGED 18:28:20Z. All nine adviser records and followed dispositions are quoted in PR comment `6044206429`; the real author COMMENT review records ACCEPT at this head. Hosted run `37666712374` passed formatting, vet and full race tests at that exact head. Fresh remote feature/PR equality, ancestry and all introduced signatures were checked before landing.
- *Scope:* The example preserves each operator's rules and per-agent runtime instructions. A channel designation needs the operator's own recorded words; a designation arriving only by relay does not count. The embedded skill source changed; installed shared binary and daemon remain unchanged.
- *State:* in force.

**ac-R14. Liveness and native Codex wake direction is decided; implementation owns the remaining proof** (2026-10-07).
- *Direction:* After the 18:00Z window, adopt revision 3 `$Z2_rLfUgr8DZVOix1KZ2hRDxsSHL4CqNnsMBOx0givk` and retain revision 2's self-keyed global #status, explicit heartbeat/staleness, hook/runtime reachability, unanswered age, correlated host incidents and keeper-routed transition alerts. Implementation owner: m1max_a2a-swarm_orch, STORY-261007-248001 (codex idle wake and liveness). Decision `$eRQi5TmNyy_gy6lq2UWzZQqJAogY4DBEn3btGAt0E3o`.
- *Advice:* macmini-infra has no objection. Follow all tb-keeper additions: Ivan-side migrations need their own launcher owner's go per session and tb-keeper's impact notice; require an explicit per-identity maximum N wake turns/hour with visible rate suppression and host budget gates; any addressee, its recorded un-prefixed alias, or a keeper reply counts as answered, while a wake alone does not. Follow e9's single launcher binding command request. The keeper of a client project and remote-worker-harness-2b did not answer revision 3 by the deadline; their earlier defect/hosting reports and revision 2 constraints remain included. Silence is recorded, not assent.
- *Boundary and proof:* Admit only the exact loaded thread, owning endpoint and current launcher generation; send a fixed metadata-only host doorbell under native toolOutput authority. One supervised consumer, durable ambiguous-write reconciliation, busy coalescing, pause/approval/input/unknown-state/budget/rate gates, and no receipts outside inbox. No terminal typing, user-authority steering, timer-only inference, broad peer-controlled runtime methods or parallel live-thread resume. A disposable runtime pilot and positive/negative controls precede shipping or launcher-owned live migration. Current embedded sessions and installed binaries remain unchanged; schema/transport probes do not establish end-to-end acceptance.
- *Scope:* Internal implementation direction through the advice process, not a new shared protocol rule or authority to migrate another operator's session. The implementation PR must contain the complete advice record and evidence.
- *State:* implementation in progress; native wake is not yet shipped or verified end to end.

**ac-R15. Native Codex doorbell and liveness source is delivered; live activation stays owner-controlled** (2026-10-07).
- *Scope:* This records implementation of ac-R14, not a new shared protocol rule or a grant to migrate another operator's session. STORY-261007-248001 remains the owning delivery record.
- *Delivery:* PR #52 landed by non-forced fast-forward from `19e96de2be0b5cb383c5718b7c098f8de9158228` to exact accepted human-signed `fa2e02a509f6a5ba01208caa25bfae500e742642`, MERGED at 21:03:17Z. Fresh remote main, feature and PR head, default ancestry, all four introduced SSH signatures and current checks were verified. Owning-launcher GitHub COMMENT ACCEPT `PRR_kwDOUxSNCs8AAAABRL40pg` and macmini focus ACCEPT `$O7bM50gn4e9aDpbsqvFYaRP9RTVtmVvHMyr1M2x-Gjs` accept that exact head. Legitimate hosted run `37685651608`, job `113012650546`, passed formatting, vet and full race checks. Intermediate `51e0b1d` legitimately failed compile/CI; its fixture correction and subsequent reviews/checks remain in the history.
- *Proof:* Disposable managed 0.161.0 threads ran the actual private CLI inbox over isolated Matrix fixtures: fixed TOOL-output authority, no peer body in native input, exact mailbox receipts only on inbox, actual busy coalescing and no duplicate inference. Production-client paged reconciliation against an archived owned fixture was read-only. Full local race/vet/build, negative regressions and bounded detector plants are recorded; plants were restored before publication. This does not establish automatic wake of an existing plain TUI or atomic native idle-generation compare-and-submit.
- *Review outcomes:* Follow the narrower pinned pre-submit refusal classifier; internal/unknown/transport errors remain fenced. Reconciliation uses bounded turn/item pages. Owner-only retirement preserves unread mail and rate reservations without inference or replay. Unknown process observations cannot authorize replacement. Status monitoring preserves manual holds, uses power-zero own-key telemetry with protected administration, isolates invalid peer thresholds in watch mode (one-shot retains exit64), retries errors at its explicit poll interval, and uses bounded resumable visible history with stream checkpoints and a required per-room page budget. Unknown history cannot open or clear unanswered incidents; reply-age journals are separate from health. Recorded alias destinations, own-monitor observation exclusion and explicit host-supersession notices are tested. Complete verbatim advice/dispositions remain in PR comments; rwh2b's new-head silence is not approval.
- *Limits and rollout:* One-item native reconciliation and the 100,000-entry retained-history bound remain nonblocking availability limits. Missing stream checkpoints require re-backfill; restarting the same genuinely over-cap time window cannot cure the cap, so any narrower history scope is an explicit keeper choice that preserves outstanding requests. Native status read and submission are separate RPCs, with host-owned scheduling/approvals. Before live binding, every same-identity writer (hooks, inbox and consumer) must use a compatible CLI; old `871cf62` writers can discard new typed-state fields. Preserve old mail/checkpoints, stop only the owner's exact old consumer, never parallel-resume live history, and leave hook trust to the human. Shared CLI/projector deployment, #status initialization and per-session launcher migration require their separate coordinated impact steps; none occurred in the source landing.
- *State:* source delivered and disposable managed inbox path verified; installed shared binaries and existing sessions unchanged. This supersedes ac-R14's implementation-in-progress state without claiming fleet activation.

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

**tb-R81. One-time retirement of the orphaned c1bxri base-refresh intent, as an exception to R77 (5)** (22:33Z, 2026-09-30).
- *Evidence:*
  - After the R77 rebuild, provisioning refuses every producer on STORY-260927-c1bxri with `worktree_stale: preserved candidate intent does not bind this workspace checkpoint`.
  - The base-refresh intent is keyed by Story, and `worktree abort --discard` never retires it (#392, BUG-261001-2lexj6). The workspace id is deterministic per Story, so an id-based orphan test cannot fire.
  - tb-muse ran tb-critical-fixes' revised checks read-only at 22:30Z, and all of them hold:
    - (1') the intent's created_at (20:07:16Z) is earlier than the workspace record's created_at (20:44:10Z);
    - (2') neither 52fd48d8 nor 3ea89aa5 is an ancestor of the branch tip;
    - (3') the branch tip, checkpoint, current base and selected base are all 8f10a12b;
    - (4') refs/backup/260930-2p2vgp/prepared-replay resolves to 52fd48d8, and vz22an-checkpoint to 3ea89aa5.
- *Decision:* approved as a one-time exception to R77 (5), for this one file only. tb-muse executes these steps:
  - (1) Immediately before the move:
    - re-run (1')–(4') and confirm the file's sha256 is still 43ab66dc…;
    - back up the CURRENT dirty candidate as a fresh tar plus a `git diff --binary` patch, with their sha256. The earlier tar holds the 12-path candidate; the rebuild left 14 paths.
  - (2) Run `mv -n` on the file into .temp/TASK-260927-2p2vgp/archive/. Never delete it and never overwrite anything. Then verify the sha256 at the new path, and verify that the old path is gone.
  - (3) Change no other record, ref or worktree. replay-340 stays, because #338's cleanup owns it.
  - (4) Respawn the 2p2vgp r13 producer under R80. If provisioning refuses for any other reason, or would clean or reset the dirty candidate, STOP and report. This ruling authorises no further manual step.
  - (5) Rollback: `mv -n` the file back to its original path; its sha256 must match.
  - (6) Put the evidence on TASK-260927-2p2vgp as a resource beside R77's: the check outputs, every sha256, the move and the respawn result.
  R77 (5) otherwise stays in force. The structural fix remains BUG-261001-2lexj6: abort retires the intent, and provisioning detects an orphan by created_at and OIDs, not by workspace id.
- *State:* in force.

**tb-R82. R80 governs worker model choice; the Gate B live proof on the local Qwen engine continues** (22:51Z, 2026-09-30).
- *Source:* tb-agents-infra-exit, after applying R80. Its Gate B live proof (TASK-260927-1ycuhl, TASK-260929-2zno29) launches a Codex child against the operator's LOCAL Qwen engine through the local-runtime binding. The question: does R80 stop that proof?
- *Decision:* no. The proof may continue.
  - R80 governs the model choice for work: producers, reviewers and routine tasks. A child whose runtime and model ARE the subject of an acceptance proof is not a worker choice. Here it is M4's own criterion, "spawn onto a local engine on this Mac through codex".
  - The child runs on local compute and burns no subscription limit, which is what R80 protects.
- *Conditions:*
  - The exemption covers only these two proof tasks.
  - The local-runtime ceiling is used for nothing else.
  - The child's output is proof evidence, never a work product.
  - Every producer and reviewer around the proof follows R80.
- *State:* in force. Ivan may overrule it at any time; tb-keeper reported it to him.

**tb-R83. Ivan's answers on desk cards #2 and #4: yes to rule 32 and to the rule 4 update; cocoaskills takes only the highest-severity audit stories** (22:57Z, 2026-09-30).
- *Source:* Ivan on ivan-desk at 22:56Z, relayed verbatim in one message: «екомендация tb-keeper: по-прежнему А, да обоим. -- ок -- В) только самые серьёзные истории из него - давай так».
  - The first part quotes the #2 recommendation line.
  - The second part quotes option В (C) of the narrowed #4.
  - The cards are identified by the quoted text; there is no card number.
- *Decision:*
  - (#2) Ivan's side says YES to both a2a-swarm protocol changes: rule 32 (PR #42 at ab0762a, blocking and non-blocking asks) and the rule 4 update (PR #43 at 6346259, the doorbell as built). Posted to m1max_a2a-swarm_orch in both threads. Landing waits for Alexey's answer and then goes in order #38 to #43.
  - (#4) cocoaskills takes ONLY the highest-severity stories of EPIC-260917-3kbco2 (security-audit remediation): secrets in argv or env; reserved shim names and PATH; the shell-hook approval gate. The rest stays in the backlog. Relayed to cocoa.
- *State:* in force.

**tb-R85. Muse as a root session: tb-muse starts R1–R3 now; curator#100 and 3dn5uk are routed to their owners** (23:14Z, 2026-09-30).
- *Evidence:* Ivan asked at 23:08Z, on card #13, whether option A delays the task-board release that launches Muse as a root session.
  - tb-muse's resource muse-root-session-path.md (EPIC-260927-5c4nxe; 23:12Z and 23:13Z) shows that the root session does not depend on c1bxri. It needs:
    - R1: the Muse interactive launch mode (yiw33v);
    - R2: a Muse PTY primary host in tb-sessiond;
    - R3: the `task-board muse` launcher;
    - R4: composition through Curator from day one. agents-infra compose supports only codex, claude and pi.
    - R5 and R6: the Spark 1.4 row and the release;
    - G: the `run.toolset` allowlist.
  - The estimate is about 32–50 agent-hours. The critical path is curator#100 plus 3dn5uk, not option A.
- *Decision:*
  - (3) tb-muse starts R1–R3 now as a new Story under EPIC-260927-5c4nxe, in parallel with c1bxri. Conditions:
    - the R80 pairs, and reviews on the full context profile;
    - a rule-2 heads-up before touching tb-sessiond or the launch code, because those are shared seams;
    - R2 follows the session-host contract of the agent-session-host spec, and deviations are raised;
    - R1's module tag goes through infra-exit's window in skill-agents-management;
    - c1bxri (2p2vgp r13) keeps priority for tb-muse's landing windows.
  - (1) curator#100 (register the muse environment) goes to the Curator owner, ivan-curator, as a priority request. Alexey takes Curator over on 2026-10-01, so if ownership has moved, the request goes on to his side.
  - (2) infra-exit decides whether 3dn5uk can go ahead of 3mf83p, or whether `task-board muse` may call `curator env resolve muse` directly for the first release. tb-keeper rules only if the owners disagree.
- *State:* in force. Reported to Ivan through the #13 thread.

**tb-R84. Ivan's multi-card answers of 23:08Z: #5, #6, #7, #9, #11 and #14** (23:15Z, 2026-09-30).
- *Source:* one message from Ivan on ivan-desk at 23:08Z (03:08 GMT+4), relayed verbatim card by card. Each card is identified by its number or by quoted card text.
- *Decisions:*
  - **#5.** A separate GOCACHE for the ivmbp CI runner, with the stated conditions:
    - an APFS clone as the seed;
    - a cap of about 8 GiB;
    - the switch happens between jobs;
    - revert if the first two runs are slower.
    macos-sys executes it and waits for a job gap. The baseline median is 88.9 min, band 86–102 min.
  - **#6.** Ask Alexey through the swarm to bring back the Alexis-M1-Max runner; posted to m1max_gha-runner_orch. tb-R45's return condition is met: the g7ijg3 fix is on main.
    - macbook-iv is used whenever it is on. Ivan manages its power and says when.
    - tb-keeper added the relux-ci label to the macbook-iv org runner (id 39) just before 23:11Z.
    - tb-keeper resumes its runner service on ivmbam1 when Ivan says it is on.
  - **#7.** No cap on review rounds. Every review runs with the FULL context profile, never lite. An item past 7 review rounds is reported to tb-keeper, who brings it to Ivan without stopping it. Broadcast to every Ivan-side orchestrator.
  - **#9.** The mini takes nothing from AX, and AX stays fully paused. Ivan asked for an OUTSIDE audit of how to speed up development. tb-keeper started the session ax-speed-audit on ivmbp at 23:12:56Z:
    - Opus high, read-only rules, R80 for delegates;
    - Remote Control session_01JSTNXyhBwJWbJmTbvphwiz;
    - it writes collective-mode/audits/ax-speed/AUDIT.md.
  - **#11.** Ivan now says yes to switching AX's spawn config to gpt-6.1-sol. That reverses his 20:13Z answer. It is relayed to ax, which confirms with Ivan in its own session first and then applies the full R80 pairs.
  - **#14.** Adopt curator-spec Decisions 0019 and 0021. Relayed to ivan-curator, which confirms in its session, where the question sits.
- *State:* in force.

**tb-R86. The fate of plhkjh and 3l276k: split; the lane contract stays with tb-tests, and selection and scheduling move to the validation owners** (23:42Z, 2026-09-30).
- *Source:*
  - Ivan, in tb-tests' session on 2026-10-01: a detailed review of two backlog tasks by gpt-6-astra at effort max, with tb-keeper deciding their fate from those reviews.
  - Ivan's standing direction for EPIC-260910-vbuq8g (Go test performance): items with no real speedup leave the epic.
  - The reviews were read-only inline analyses, attached to the elements. They are not CR reviews, and astra at max was Ivan's explicit instruction.
- *Evidence:*
  - **TASK-260910-plhkjh** (select finalizer suites from changed inputs) is partly superseded but still valid for doc-only CRs. Its September contract is unsafe to implement literally: it lists 6 modules instead of 5, takes the first matching row instead of the union, omits the server under remoteconfig, and treats docs/ as safe although spawn guards read it.
  - **TASK-260910-3l276k** (unify the test-lane entrypoints) is partly superseded and still valuable. It is mostly correctness and maintenance, with some savings from duplicate runs.
- *Decision:* both are SPLIT along one interface. tb-tests owns WHAT the lanes are and that they cover everything. The validation owners own WHICH lanes run for a CR and how the evidence binds.
  - **Kept with tb-tests** in EPIC-260910-vbuq8g:
    - plhkjh's lane-coverage inventory;
    - 3l276k's canonical lane definitions (the current 5 modules plus build tags) and completeness enforcement, after #481.
    - These stay because they are the precondition of the only real speedups in both items: selective suites and no duplicate runs.
    - Condition: tb-tests states the expected saving and measures the duplicate-run saving before and after. If it proves to be nil, the remaining correctness work moves out, per Ivan's direction.
  - **Moved to the validation owners.** tb-critical-fixes decides whether it or ivan-validation-fixes takes each piece:
    - plhkjh's selection and evidence binding. The first slice covers research and doc-only CRs, with full fallback, after #481, #478 and BUG-261001-33ave8. Silent coverage loss is never allowed.
    - 3l276k's CR scheduling, reuse and config consumption, alongside #478.
  - **A prerequisite for any full-green claim:** BUG-261001-33ave8, repaired and actually executed.
- *State:* in force.

**tb-R87. Ivan's decisions on the AX speed audit (card #15), and a public-access audit** (23:47Z, 2026-09-30).
- *Source:* Ivan on ivan-desk.
  - 23:42Z, verbatim: «по ax почти все пункты да кроме ревью - если реально гитхаб бесплатный для публичных проектов, то надо этим пользоваться и еще заодно проверить что мы еще можем перевести в публичный доступ (кроме ядра доски - это переносим к 16-му октября.»
  - The clarification at 23:47Z, verbatim: «(оставить «нет» без границ)». It is the first alternative of the desk's question «Только (c) (оставить «нет» без границ), а (a), (b) и (d) принять?».
- *Decisions for AX, applied at resume. AX stays paused.*
  - Lever 1, the suite diet, is YES. It is AX's first config commit at resume:
    - one combined `go test ./... -race -cover -json` with an explicit -timeout;
    - the producer runs targeted packages only;
    - the reviewer reuses the construction result.
  - Lever 2 is YES for (a), (b) and (d):
    - (a) a pre-spawn surface table;
    - (b) the whole class of a finding, with every site, in one verdict;
    - (d) no acceptance criteria that demand a checker complete against adversarial code shapes.
    (c) is REJECTED: evidence-only findings get no bound, and #7's «нет» stands with no bounds.
  - Lever 3A is YES: AX's heavy lanes move to GitHub-hosted runners. Standard hosted runners are free for public repositories. This reverses the 09-08 local-only CI rule for AX. The task-board side is skill-project-management#487; until it lands, the local gate stays.
- *Instruction:* audit which other repositories can go public, excluding the board core (skill-project-management), which moves on 2026-10-16. ax-speed-audit is running it read-only. Ivan decides per repository, and no visibility changes without his answer.
- *Done since:* AX main carries R80 as 5712474 (PR #67), which Ivan confirmed in ax's session.
- *State:* in force.

**tb-R88. A standing rule for session managers: tb-keeper restarts or stops only the safe ones** (23:51Z, 2026-09-30).
- *Source:* Ivan on ivan-desk at 23:48Z, card #1 (outdated tb-sessiond managers), verbatim: «пускай кипер возьмет за правило перезапускать только те которые можно без проблем перезапустить и которые не делют ничего критического».
- *The rule, standing:*
  - A manager counts as safe to restart or stop only when all of these hold:
    - it hosts no live session and no running run;
    - it has no connected clients and no child processes;
    - no process of its build depends on it;
    - it is doing nothing critical.
  - How to act on a safe manager:
    - a lock holder is restarted with `task-board session restart` from an outside shell;
    - a stale non-holder is stopped with SIGTERM.
  - The board owner gets a heads-up under rule 2.
  - A manager with live sessions waits until its owner reports a quiet moment. It is never restarted to pick up a build.
  - This supersedes "a per-event go for each daemon restart" for the safe cases only.
- *Executed at 23:50Z:*
  - Stopped five stale managers that hosted nothing: 10911, 30676, 96815 and 97313 (duplicates on infra-exit's board), and 68107 (an orphan on the curator board).
  - Restarted tgfs's manager, which had no live sessions: 49738 became 35640, on the current build 94691714.
  - Waiting for a quiet moment:
    - 60087, infra-exit's live primary and two runs;
    - 22921, «Task-board корневые цели и запуски»;
    - 55355, tb-muse's live primary.
- *State:* in force.

**tb-R89. Claude Code prompt suggestions are off in every session started through our launchers** (23:58Z, 2026-09-30).
- *Source:* Ivan on ivan-desk at 23:57Z, card #16, verbatim: «Переменная окружения CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false. Она важнее настройки, и её удобно ставить в лаунчерах: tmux, task-board claude, curator run. -- и нужно взять в работу задачи что бы через task-board claude, curator run всегда прикладывались эти параметры (и потом через keeper up)».
- *Why:* tonight tb-keeper misread Claude Code's grey prompt suggestions in the Mac mini panes as unsent operator text. Nothing was sent in Ivan's name, but a suggestion was relayed once as his instruction before the correction.
- *Decision:* `CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false` is set as a literal in every Claude Code launch.
  - task-board: skill-agents-management#46, the claude plugin's childEnv for every mode. Offered to infra-exit, with ivan-model-routing as the fallback owner.
  - curator run: curator#102, routed to ivan-curator after curator#100.
  - keeper up: curator-keeper#6, a line in the spec.
  - Until the launchers carry it:
    - tb-keeper's own tmux launchers (ax-speed-audit, the ivan-desk recipe) export it now;
    - macmini-infra adds it to every NEW session it starts on the mini.
  Running sessions are not restarted for this.
- *State:* in force.

**tb-R90. CI queue reordered so that the R61 critical fix #485 gets the next relux-ci runner** (00:05Z, 2026-10-01).
- *Evidence:*
  - #485's CI (36790030838, BUG-260930-1dh28f) had been queued since 23:14Z with no runner: the M1 Max and macbook-iv are off.
  - rose-air was running a push CI on main for the board-only Record 640b6017 (36779099396). That is not landing evidence for anything; R60 calls it the waste class, and Stage 1 removes it.
  - #480's run (36780419548, Alexey's branch) was queued ahead. It lands after 06:00Z, so it had hours of slack.
  - GitHub does not hand out queued jobs FIFO: when rose-air was freed, it picked #483's rerun (36792710809, queued 23:45Z) instead of #485.
- *Decision (tb-keeper as spm main arbiter), at the request of tb-critical-fixes:*
  - cancel the board-only main CI 36779099396;
  - cancel the queued #480 run 36780419548 and #483's just-started rerun 36792710809, so that #485 is the only queued job when rose-air frees;
  - re-run both cancelled runs as soon as #485 has a runner.
  - The cancelled #483 rerun does NOT count against ci-isolation's single tb-R26 rerun.
- *State:* executed; #485 in_progress on rose-air at 00:05:49Z; #483 and #480 runs requeued.

**tb-R91. Model aliases: the registry is the one source of "latest", and delegation goes only through task-board spawn** (00:07Z, 2026-10-01).
- *Source:* Ivan, directly in tb-keeper's session at about 00:08Z, answering tb-keeper's options A and B. Verbatim: «да оба сразу и а и б», and to the alias research: «ок».
- *Background:*
  - task-board resolves its registry aliases to exact ids in argv. In agents-management, `sonnet` has AliasOf claude-sonnet-5-5 and `opus` has AliasOf claude-opus-5-5.
  - Claude Code's OWN table (2.1.286) maps `sonnet` to claude-sonnet-5 by default, and to 4-6 through AWS or a gateway. Agent-tool subagents and a raw `claude --model sonnet` therefore drift.
- *Decision:*
  - **A.** The harnesses' default-model overrides are generated from the agents-management registry:
    - for task-board launches, by the claude plugin's child env: skill-agents-management#47, beside #46;
    - for Curator-managed homes and `curator run`: curator#103;
    - for keeper up, later, the same rule.
    The variables are ANTHROPIC_DEFAULT_{SONNET,OPUS,FABLE,HAIKU}_MODEL. Claude Code 2.1.286 reads all four.
  - **B.** All delegated work goes through `task-board spawn`. Harness-native subagents and harness aliases are not used for delegated work.
    - Interim for sessions whose control root has no task-board config yet (the R75 exception on the Mac mini): direct launches with EXACT ids only (`codex exec -m gpt-6.1-sol`, `claude -p --model claude-sonnet-5-5`), never the Agent tool and never an alias, until the root gets a config.
    - Enforcement, meaning hard switches that turn native subagents off, comes from research L2 part A.
  - **Research:** L2 part B (macmini-infra) maps every harness's alias table, and how the registry pins it, for Claude, Codex, Muse and agy.
- *State:* in force. Broadcast to every Ivan-side orchestrator.

**tb-R92. Recovering yiw33v (STORY-260930-1lfkbh) from an "already combined" candidate: R77's procedure, conditionally** (00:11Z, 2026-10-01).
- *Evidence (tb-muse, 00:11Z):*
  - CR rev 2 is `ready`, but its review was refused with revision_base_superseded: main moved to e53de69 and overlaps on README.md.
  - The r3 producer could not refresh. It combined e53de69's 20 paths into the uncommitted candidate, and all checks are green with 4 mutants killed.
  - Operator converge fails in two ways. First as conflicted. Then, after the two overlapping files were set to trunk, with worktree_git_command_failed, because merged_tree_oid == dirty_tree_oid (44f5f267): converge does not handle a delta that already contains the incoming authority.
  - The candidate was restored and verified, with 0 mismatches across 30 paths.
  - No base-refresh intent exists for this Story.
- *Decision:*
  - (a) goes to tb-critical-fixes: is there a supported command for the "already combined" case?
  - (b) If tb-critical-fixes answers that there is none, tb-muse may apply R77's procedure to 1lfkbh without asking again. The steps:
    1. Verified backups: a tar plus a `git diff --binary` patch of the 30-path candidate, each with its sha256, and refs/backup/… for every commit of rev 2 on the Story branch.
    2. The supported `abort --discard`.
    3. A fresh workspace from current main.
    4. Re-apply the candidate, then producer re-validation.
    5. Hand off as rev 3, with a full re-review: Sonnet 5.5 high, and Astra high for the final review (R80a). Acceptance does not carry over.
    6. R77 (5) stays in force: no hand edits to intent or registry records.
    7. The evidence goes on yiw33v as a resource.
  - tb-critical-fixes files the converge gap (merged == dirty) as a bug in the #392 family.
- *State:* in force.
- *Amended 00:13Z:* tb-critical-fixes answered (a) with a supported operator path that needs no abort: the same path it used on odansk, minus the 3-way step. It becomes the PRIMARY path, and R77's procedure is the fallback only if a step refuses. The steps, in tb-muse's own Story worktree:
  1. Record the snapshot's full-tree OID with a temporary index; it must equal 44f5f267.
  2. Restore the dirty tracked paths to the current base, BY NAME (never repo-wide), and move the untracked candidate files aside.
  3. Run `worktree converge` from the control root. It is a clean fast-forward to e53de69, and rev 2 goes stale as expected.
  4. Overlay the snapshot exactly, deletions included.
  5. Verify that the temporary-index write-tree equals 44f5f267, and that the diff against e53de69 holds only the producer's paths.
  6. Do the r3 handoff under the R80 pairs, with the full re-review of R92.
  The workspace record and the CR history stay intact. The case is added to #443.
- *Outcome 00:13Z:* the primary path worked with no abort. The snapshot tree is 3658ff1d (44f5f267 was a temporary state, verified by recomputing). converge moved 2dff98f to e53de69, and the delta is the producer's 13 paths only. The r3 handoff is RUN-261001-c7eb97 under R80. The evidence is converge-r3-evidence.md on TASK-260930-yiw33v.

**tb-R93. Ivan's in-session answers on the public-repo audit (card #17)** (01:04Z, 2026-10-01).
- *Source:* Ivan, directly in tb-keeper's session, between 01:01Z and 01:03Z.
- *Decisions:*
  - **The codex fork's heavy CI lanes move to GitHub-hosted runners: YES** («давай», «да»).
    - Routed to codex-fix, whose plan is: a hosted relux-ci.yml; the CR candidate pushed to ci/<story>/rev<N>, so that the hosted result on the exact head is the evidence; a local fast lane only.
    - It reverses an earlier "no CI" instruction in codex-fix's own session, so codex-fix confirms with Ivan there first.
  - **The duplicate ivanopcode/skill-git-filter-repo is archived: YES.** Done at 01:03:34Z; it remains private and archived.
  - **The curator specification family's legal gate:** «отправь в опус 55 макс на решение - он в роли юриста. другого у нас нет».
    - Session relux-legal started at 01:04Z (claude-opus-5-5, effort max, Remote Control session_01HMG5o9FDhopwAknSmzdtHb, read-only).
    - It decides the open RCL 1.0 §1.2 question and the per-repo terms for the five repos (model-router, network-profiles, inference-manager, playbook, trust).
    - It also checks the Adoption Notice, and authorship and consent.
    - The memo goes to collective-mode/audits/legal/LEGAL-MEMO.md, and the publication itself still goes through Ivan.
  - **Asked, not yet decided:**
    - Which of the 7 "public now" repos help CI? Answered: none of them helps the local CI load. Six have no workflows, and swift-realtime-openai-local runs only on hosted runners with no recent runs.
    - More detail on the history risk. Answered in session.
- *State:* in force.
**tb-R94. Binary swap #5 on ivmbp: the 1dh28f integrate fix** (01:22Z, 2026-10-01).
- *Trigger:* PR #485 (BUG-260930-1dh28f, the R61 critical fix) landed at c5ff3d19 at about 01:19Z. The standing go of R55 applies.
- *Done:* `curator global upgrade` from tb-keeper's outside, unhosted shell, 01:20:05Z–01:21:59Z, rc=0.
  - task-board is build 1361260b and tb-sessiond is build 8457d455, both from main 10c4b582.
  - The old builds were 4404b59c and 94691714.
  - Probe: the new binary carries validationPolicyFilePresentAt, and the old one does not (negative control).
  - IMPACT START was posted at 01:19:57Z and IMPACT END after the run.
  - Daemons were not restarted (R55, R88).
- *Open:* the R61 redline closes after the first real `worktree integrate` on the new build succeeds.
- *State:* done.

**tb-R95. BUG-260927-3ib9pa gets the next free landing slot once it is READY; the README restale loop** (01:29Z, 2026-10-01).
- *Evidence (m1max_skill-project-management_orch, 01:29Z):* BUG-260927-3ib9pa (close-landed records use wall-clock time, not the configured commit-time policy) went stale four times today, each time on README.md. Each lap costs a CR suite, a review and a landing suite: about 2 h and three Opus runs.
- *Decision:*
  - When its refreshed revision is accepted and posted READY, 3ib9pa takes the NEXT free window. It goes ahead of every non-critical item that is not yet in a window, and after any window already open and any R61-class critical item.
  - Its window post lists its README hunks per R56.
  - Until it lands, other landers that carry README.md hunks post a one-line heads-up before opening their window, so 3ib9pa's owner can rebase BEFORE its review, not after acceptance.
  - The tool fix of R56 (re-parent and revalidate clean doc-only junctions instead of marking stale) goes back to tb-critical-fixes for a status and a date. It shares machinery with ci-isolation's stage 3 (absorbing board-only trunk advances), so tb-critical-fixes and ci-isolation decide together whether stage 3's fail-closed opt-in can extend to doc-class paths.
- *State:* in force.
- *Amended 01:31Z:* the shape of the tool fix is agreed with tb-critical-fixes. One opt-in key carries two classes:
  - board-only: absorb, with no suite (ci-isolation's stage 3);
  - doc-class: re-parent AND revalidate, keeping the acceptance and attaching the junction diff (#472). Doc-class never skips the suite, because the docs guards read README.md and references/**.
  ci-isolation keeps the key shape open for the second class. #472 is not started; it comes after odansk, #408 and #392, and the target for a reviewed CR is 2026-10-03.
- *Settled 01:32Z:* TWO sibling booleans instead of one key with classes, as ci-isolation proposed: `absorb_board_only_advances` (board-only, no suite) and `reparent_doc_only_advances` (doc-class, re-parent plus revalidate). A reader then sees which key skips the suite. #472 refactors the stage-3 predicate to take a path class.

**tb-R96. Muse keeps auto-updating; the tooling adapts to new versions instead of pinning them** (02:21Z, 2026-10-01).
- *Source:* Ivan, directly in tb-keeper's session at about 02:00Z, verbatim: «Нет пускай обновляется - надо адаптироваться под изменения». He answered tb-keeper's question whether to disable Muse auto-update host-wide on ivmbp, as had been done on the mini.
- *Decision:*
  - Muse auto-update stays ON on ivmbp. macmini-infra reverts its 01:55Z host-wide disable on the Mac mini: the MUSE_NO_AUTO_UPDATE line in ~/.zshenv, and launchctl unsetenv.
  - The agents-management muse plugin keeps setting the variable for task-board CHILDREN, so a running child does not change binaries mid-run. New launches pick up the updated launcher binary.
  - tb-muse's MSP client and pin qualification (vz22an) ADAPT: a new version or schema fingerprint triggers a fast re-qualification run, not a standing block. 1.4.2 is the current target, and ivmbp gets it at the launcher's next hourly check.
  - tb-keeper's own read-only probes still pass the variable, so that they never start an update at an arbitrary moment. Updates come through the normal calls.
- *State:* in force.

**tb-R97. KV-cache orchestration research: Fable max, then Astra max, then tb-keeper aggregates (an explicit exception to R80)** (02:27Z, 2026-10-01).
- *Source:* Ivan, directly in tb-keeper's session at about 02:20Z.
  - He sent a prior conversation and a draft spec for a multi-backend KV-cache branching and model-swapping gateway (llama.cpp, antirez/ds4).
  - Verbatim: «нам это нужно что бы мы могли через нашу системы оркестрации управлять движками инференса для эффективного переиспользования kv-cache. Передай ресерч в фейбл макс, одновременно передав ему еще вводные на тему нашего роадмапа и модулей куда это должно быть встроено. Результат от фейбла передай в астру Макс, и потом с агрегирую все в финальную архитектуру и как оно вписывается а нашу инфраструктуру и роадмап её построения».
- *Decision and execution:*
  - **Fable max.** An explicit operator exception to R80's "no Fable max", for this research only. Session kv-research on ivmbp, started 02:27Z: claude-fable-5-1, effort max, read-only rules, Remote Control session_01NgjQK3e8VRMP84bqKNXUzg.
    - Inputs: Ivan's two documents, saved verbatim under collective-mode/research/inference-kv/inputs/.
    - Context: CONTEXT.md, covering LP-D7, D9 and D10; task-board shared contexts and context-builder; tb-sessiond; curator-inference-manager; the model router; the agents-management launch plane; network profiles; roadmap M4, M5b, M8 and AR.
    - Deliverable: RESEARCH.md, covering claim verification with citations, the architecture, placement per module, the measurements to run, and a roadmap.
  - **Astra max.** Its result goes to gpt-6-astra at effort max, for an adversarial review. This is the same operator exception, since R80a otherwise allows astra at high only.
  - **Aggregation.** tb-keeper turns both into the final architecture, its fit with the infrastructure, and the build roadmap.
- *State:* done (see the outcome).
- *Outcome 04:09Z:* aggregated. ARCHITECTURE.md was published with the research, the review and the diagrams in wiki research/kv-cache-orchestration/ (main 972e151). The roadmap gained track 4c, K0–K5. The operator decisions Q1–Q10 went to the desk. K0 and K1 need no decision.
**tb-R98. C1 (#478) goes first-ready after #488; private-repo CI stays manual until publication** (04:52Z, 2026-10-01).
- (1) validation-fixes' C1 (#478): BUG-261001-21mxun and BUG-261001-358zsz are accepted. 3ib9pa and 6qyhbh have posted no READY for about 8 h. C1 relieves ceiling-1 slot starvation.
  - It goes first-ready after #488 (which landed at 04:49Z, 279e570e). R95's priority for 3ib9pa stands if 3ib9pa is READY first.
  - The last review is a final astra-high review on the rebased head.
  - m1max is offered validation-fixes' clean 3-way resolution of 6qyhbh over C1.
- (2) CI for curator-model-router (and curator-network-profiles) stays on manual dispatch with local verification, per the 09-24 decision that private repos use only our own runners. It switches to hosted CI on publication (card #19).
- *State:* in force.
**tb-R99. yiw33v rev 4 takes a one-off priority lane for its short suite** (05:03Z, 2026-10-01).
- *Evidence (tb-muse, 05:02Z):* TASK-260930-yiw33v rev 4 (skill-agents-management, RUN-261001-c8b9f0) adds the interactive Muse launch mode, and it is now the blocker for interactive `curator run muse` (curator TASK-261001-3fgu9f). Its suite (make vet, make regress, the agents-management go tests) takes minutes, but it was queued behind tb-muse's own 2p2vgp spm suite (8/14, hours) at ceiling 1. Free space was 61 GiB and load1 about 19.
- *Decision:* it runs now in a private priority-state dir, by R70 (1)'s mechanism, as one suite only.
  - It does not start if free space is under 45 GiB.
  - The dir is deleted after the suite.
  - The review (astra high) and the window in #skill-agents-management follow, ordered with infra-exit's 2vfg79 (v0.5.34).
- *State:* in force until the suite ends.
**tb-R100. Order on skill-agents-management main: #49, then yiw33v, then 2vfg79** (05:28Z, 2026-10-01).
- *Evidence:* ivan-model-routing's PR #49 (W3, pkg/providerquota, head 83d4bd9, accepted after 5 astra-high rounds) is ready.
  - yiw33v (tb-muse) is still in its R99 suite, with its review to follow; the ETA for a tagged plugin is 10:00–12:00Z. 2vfg79 (infra-exit) is not reviewed yet.
  - #49's overlap with yiw33v is new files only in pkg/agentic/systems/muse, plus CHANGELOG.md.
- *Decision:*
  1. #49 lands first-ready now, in a window that infra-exit (the area owner) names, as an exact-head plain ff.
  2. yiw33v rebases onto it before its astra review. R99's lane is amended to allow one more short re-validation after that rebase.
  3. 2vfg79 follows yiw33v. infra-exit tags once yiw33v is in.
- *State:* in force.
**tb-R101. odansk rev3 takes R1's private priority lane** (05:29Z, 2026-10-01).
- *Evidence:* TASK-260922-odansk rev3 (F2+F4 safe refresh; it reworks the 4 refresh defects that the astra final review reproduced) is the last of the seven R61-class critical fixes. Its CR validation (RUN-261001-adf817) was at queue position 7, roughly 8–12 h away. Free space was 66 GiB and load5 about 27.
- *Decision:* it runs now in .temp/critical-260922/priority-state, one suite only (R70's mechanism). adf817's queued validation is cancelled and respawned there.
  - The lane counts inside the machine ceiling: when ceiling 2 returns, the shared queue runs one suite until odansk ends.
  - Under 45 GiB everything else holds, and this suite finishes.
  - A go-build trim at the 2 h floor follows.
- *State:* in force until the suite ends.
**tb-R102. R30's load trigger reads CPU idle, not load average alone** (07:06Z, 2026-10-01).
- *Evidence (macos-sys, 07:05Z, several samples):*
  - At load1 35–46, the CPU was 20–47% idle with 9–16 runnable threads, and only one process was in uninterruptible wait.
  - macOS load counts threads. Parallel `go test` (-p 2, t.Parallel) raises it without matching CPU demand. tb-keeper's own sample just before this ruling read 59% idle.
  - Last night's R22 event (load 112–168 at 90 °C) was real. A pure thread-count rise can still cross load5 96 while the machine has headroom.
- *Decision:* R30 fires when, for 10 consecutive minutes, either:
  - CPU idle is under 10%; or
  - load5 is above 96 and CPU idle is under 25%.
  - thermalState serious or critical still fires at once.
  - Its action is unchanged: R22 in full, plus pausing the ivmbp runner, which is the operator's standing decision for load peaks (R28).
- *State:* in force. The watcher samples `top` CPU idle and load5 every minute.
**tb-R103. odansk rev4 takes one more lane suite; a narrow hold on overlapping doc hunks** (07:59Z, 2026-10-01).
- *Evidence (tb-critical-fixes, 07:54Z):* odansk rev3's lane suite was green. Then #483/#481 landed on README.md and references/change-request-lifecycle.md, which odansk also edits, so the reviewer freshness preflight would refuse. After the doc rebase (patch-id 683b9186 unchanged), the tree is new and needs one more suite.
- *Decision:*
  1. One more R101-lane suite for rev4, started only with free space at 45 GiB or more.
  2. Landers touching the same files as odansk hold until its review starts, at the latest 10:15Z. Non-overlapping hunks, references/** included, land normally. critical-fixes posts odansk's full doc file list.
  3. After 10:15Z the hold lapses.
- *State:* ended at 10:01Z, when odansk's final review started (RUN-261001-a92965, astra high). The rev4 suite was green and the lane is closed.
**tb-R104. R1b (2xr7e6) takes a one-off lane for its short suite, under the disk hold** (10:03Z, 2026-10-01).
- *Evidence (tb-muse, 10:03Z):* TASK-261001-2xr7e6 makes the Muse plugin able to launch the offline echo provider. v0.5.34 always emits --model, and Muse 1.4.1 refuses --model with echo. It blocks R2 (the Muse PTY primary host) and so Ivan's Muse root-session goal. Its suite is the minutes-long agents-management set. Free space was 45 GiB and CPU about 61% idle.
- *Decision:* it runs now in a private lane by R99's mechanism. The disk hold targets spm-sized go-build growth, which this is not.
  - It starts only with free space at 43 GiB or more.
  - One suite only, and the dir is deleted after.
- *State:* in force until the suite ends.
**tb-R105. odansk's alias-preflight rework takes one more R1 lane suite** (11:27Z, 2026-10-01; sent by SendMessage because Matrix was down).
- *Evidence (tb-critical-fixes):* odansk's round-2 final review (astra) confirmed all four rev2 findings fixed and all 10 mutants killed. One blocker remained: a pairwise physical-alias variant of the destination preflight, where incoming paths that alias by case-fold or NFC/NFD must refuse before any mutation. The rework RUN-261001-c502c1 had handed off and was at queue position 10. Free space was 49 GiB.
- *Decision:* one more R1 lane suite (R101/R103 mechanism), started only with free space at 45 GiB or more.
  - A paired go-build trim follows.
  - The lane closes after the suite.
- *State:* in force until the suite ends.
**tb-R106. cocoa's three option-C security suites take a one-off sequential lane** (11:52Z, 2026-10-01; sent by SendMessage).
- *Evidence (cocoa):* the three option-C security CRs (Ivan's card #4: K3 1ax76u, K4 39mn8b, K24 2u0otf) lost their queued tickets twice, after 5h50m and 6h40m, to the #446 idle-deadline expiry under ceiling 1. Each suite is 15–20 min of Python with no Go build cache. Free space was 42 GiB and CPU about 52% idle.
- *Decision:* a private lane runs them ONE AT A TIME, in the order K3, K4, K24.
  - Each starts only with free space at 40 GiB or more and CPU idle at 25% or more.
  - cocoa pauses and reports if free space falls under 40 GiB.
  - The lane dir is deleted after the third suite.
- *State:* COMPLETE (reported 13:33Z): all three suites green in the lane (1ax76u 11,698 passed; 39mn8b 12,129; 2u0otf 11,617); lane dir deleted.

**tb-R107. The ivmbp CI runner pauses for the disk incident** (12:53Z, 2026-10-01).
- *Evidence:* free space was 34 GiB at 12:51Z and falling about 4 GiB/h. The swap leak (fseventsd at 16.3 GiB, 55 swapfiles) is waiting on Ivan's sudo restart; he has had pushes since 08:10Z. rose-air and the M1 Max were online.
- *Decision:* the ivmbp runner service (actions.runner.relux-works.ivmbp) is booted out as soon as its current job finishes. The basis is R28/R30, Ivan's standing decision to pause ivmbp for load peaks, applied to this host-health incident.
  - CI continues on rose-air and the M1 Max.
  - Rollback: `launchctl bootstrap gui/<uid> ~/Library/GitHubActions/ivmbp/manual-service.plist`.
  - Resume when free space holds at 50 GiB for 10 minutes, or after the fseventsd restart.
  - Announced as IMPACT START. IMPACT END follows.
- *State:* CANCELLED at 12:54Z before it took effect. codex-fix removed codex-target (14 GiB) and macos-sys trimmed, so free space was back at 53.8 GiB, above the 50 GiB resume line, while the runner was still on its first job. The pause re-arms if free space falls under 40 GiB again. RE-ARMED at 15:07Z (39 GiB); runner booted out idle at 15:08:11Z. CLOSED: Ivan restarted fseventsd at 15:11:48Z (swap 66 to 10 files, 96 GiB free); runner bootstrapped back at 15:12Z.

**tb-R108. Ivan-side chat structure in a2a product spaces, Phase A** (13:01Z, 2026-10-01; decided after the rule-27 advice process, 11:31–13:00Z).
- *Basis:* Alexis's side asked tb-keeper to decide as Ivan's meta-keeper (rules 14 and 27: reversible chat structure is an applied question). Ivan is informed via desk #24. The one item left to the operators is the ownership of skill-agents-management.
- *Advice:* no objections from critical-fixes, validation-fixes, ci-isolation, tb-muse, infra-exit or m1max_local-models_orch. Taken:
  - keep the old aliases;
  - announce Phase B 24 h ahead;
  - subs sync after the move;
  - invite @ivan and @alexis;
  - one room for skill-agents-management windows.
- *Decision:*
  - **swarm-platform** (meta-keepers tb-keeper and m1max_local-models_orch), with children:
    - swarm (Alexis's);
    - spm (home #skill-project-management);
    - curator (home #curator);
    - model-routing;
    - network-profiles.
  - **Top-level spaces:** apiary-talk and cocoaskills.
  - Phase A only links rooms, with no renames or archives.
  - #skill-agents-management stays the single room for that module's windows. #swarm-skill-agents-management carries no landing traffic until ownership is settled.
  - Phase B is per product: announced 24 h ahead, old alias kept, never during windows or disk holds, spm last.
- *State:* Phase A in progress.

**tb-R109. odansk rev6 lane suite; a doc hold until odansk lands; R2 next in the lane** (14:01Z, 2026-10-01; late, because tb-keeper's doorbell was down from 12:01Z to 14:00Z).
- *Evidence:*
  - odansk rev5 ran green in the R105 lane, but #480 (README.md) and C1 (references/tracked-background-spawn.md) landed during its suite. That made the third doc-junction loop.
  - The doc rebase kept patch-id c8140f8a, which makes rev6.
  - tb-muse asked for a lane for R2 (RUN-261001-ce9e65), the last code leaf on Ivan's Muse root-session path.
  - Free space was 42 GiB, with swap growing.
- *Decision:*
  1. One rev6 suite in R105's lane, from 45 GiB.
  2. A doc hold on odansk's four files (README.md, references/change-request-lifecycle.md, references/file-formats.md, references/tracked-background-spawn.md) until odansk lands, capped at 2026-10-02 06:00Z.
  3. R2 takes the next lane suite after odansk's, from 45 GiB. Only one spm-sized lane suite runs at a time.
- *State:* in force for the doc hold. The lane part is done: odansk rev6 was green at 16:08Z, and R2 was green at 18:06Z (RUN-261001-28370c, 14/14); the lane is closed.
**tb-R110. A provider content-policy refusal during an R80 review** (16:40Z, 2026-10-01).
- *Evidence:* tb-critical-fixes' codex gpt-6-astra review of odansk's filesystem-race code (RUN-261001-26559b) ended at 16:35Z with OpenAI's "flagged for possible cybersecurity risk" filter, after its verdict was written. The transcript-substring classifier mislabelled the exit as provider_capability_unavailable (spm#493). codex-fix diagnosed the false positive; critical-fixes found the real line.
- *Decision:*
  1. Briefs for security-adjacent defensive work use neutral, accurate regression-test language.
  2. If astra is refused by policy again on such code, that review goes to claude-sonnet-5-5 high, R80's standard reviewer pair. It counts as an allowed pair, not a fallback, and the refusal line is recorded.
  3. A refused producer rephrases and retries once, then reports.
  4. OpenAI "Daybreak" access is an org-level act and goes to Ivan's desk, not urgent.
- *State:* in force.
**tb-R111. The standalone CI guard steps are removed; the broad step runs every guard uncached once** (17:05Z, 2026-10-01).
- *Evidence (tb-tests, TASK-260910-19eu3m_guard-duplicate-wall-261001.md):* since the prelude runs go clean -testcache, the broad 'go test -p 2 -timeout 90m ./...' step already runs ciguard, testsupport, mirrorgate and spawn uncached. The standalone -count=1 steps ran them a second time, serially: 315 s of wall on the M1 Max and 142 s on rose-air. ci-isolation, owner of the steps, agrees.
- *Decision:* option (a), with ci-isolation's conditions:
  - ciguard pins the exact broad command after the prelude;
  - CLAUDE.md is updated;
  - the local Makefile prerequisites stay.
  - It lands after l2vgx1, and tb-tests owns it.
  - Alexis's side may object by 2026-10-02 09:00Z.
- *State:* in force.

**tb-R112. odansk rev7 takes one R1 lane suite, launched only when CPU idle is 25% or more** (18:07Z, 2026-10-01).
- *Evidence:* the round-3 rework (RUN-261001-1f8476: temp-file ownership proof and per-install parent-namespace re-verification) handed off. Its CR validation was at queue position 8. Disk was 67 GiB. CPU idle was about 19%, with ceiling 2 back and the shared slots busy.
- *Decision:* one lane suite by the R101–R109 mechanism. It starts only with free space at 45 GiB or more AND CPU idle at 25% or more at launch, because the lane counts inside ceiling 2. A paired trim follows. The R110 routing and the R109 doc hold apply.
- *State:* done. The rev7 suite was green at 20:36Z (RUN-261001-f95be9), and the lane passed to R2 under tb-R114. The tb-R109 doc hold stays until odansk lands.
**tb-R113. A standing short-suite lane for module repositories (trial until Gate A)** (18:54Z, 2026-10-01).
- *Evidence (infra-exit; Ivan asked it to cut the infra-exit timeline):* CR suites for skill-agents-management and curator-inference-manager take minutes but waited 3–5 h behind 60–90-min spm suites. They went stale and paid a converge, a republish and a second queue lap: 1ycuhl, 3txv44, 2vr6t8 and 2vfg79.
- *Decision:* one private lane per module binding, open to any campaign landing there.
  - Only suites expected under 15 min run there; anything past 20 min goes back to the shared queue.
  - At most one module-lane suite at a time machine-wide, counted inside the ceiling.
  - Start only with free space at 45 GiB or more and CPU idle at 25% or more.
  - spm suites are excluded.
  - At the trial's end, a report: the number of lane suites, their wall time and the stale laps avoided.
- *State:* trial, in force until Gate A lands.

**tb-R114. R2 rev3 takes the next spm lane suite after odansk rev7's** (19:53Z, 2026-10-01).
- *Evidence:* tb-muse asked at 19:41Z. R2 rev3 (RUN-261001-f3425e, TASK-261001-3hz2af, the Muse PTY primary host) fixes the round-2 review findings and sat 8th in the shared queue. R2 is the last code leaf before the Muse orchestrator (R3). curator#100 is complete on the curator side, and R1c lands with v0.5.37. odansk rev7's lane suite (RUN-261001-f95be9, tb-R112) was still running. Free space was 55 GiB and CPU idle about 50%.
- *Decision:* one lane suite by the tb-R101–tb-R109 mechanism, launched after odansk rev7's suite ends.
  - It starts only with free space at 45 GiB or more AND CPU idle at 25% or more (tb-R112). It counts inside the machine ceiling (tb-R72), and only one spm-sized lane suite runs at a time.
  - If R2's shared-queue turn comes first, it takes that turn and the grant lapses.
  - A paired go-build trim follows. The tb-R109 doc hold still applies.
- *State:* in force until the suite ends.

**tb-R115. The tb-R113 module lane also covers curator-agent-launcher** (21:01Z, 2026-10-01).
- *Evidence (infra-exit, which owns the launcher):* 5 lane suites under tb-R113 ran 2.4–12.8 min each, against 3–5 h waits in the shared queue earlier the same day. TASK-260927-2gp7vz (curator run through the agents-management construction API, Gate A) waited 1h50m in the shared queue for a minutes-long Go module suite.
- *Decision:* the curator-agent-launcher binding joins the tb-R113 lane for the rest of the trial.
  - tb-R113's limits are unchanged: one module-lane suite at a time machine-wide, counted inside the ceiling; suites under 15 min only, and past 20 min back to the shared queue; launch at 45 GiB or more free AND CPU idle at 25% or more; never an spm suite.
  - The Gate A trial report covers all three bindings.
- *State:* trial, in force until Gate A lands.

**tb-R116. The spm main landing order for the night of 2026-10-01** (21:25Z, 2026-10-01).
- *Evidence:* three code candidates were heading for spm main at once, and each landing makes the others' tested heads stale:
  - odansk: rev7 lane suite green at 20:36Z, round-4 review running;
  - R2 rev3: tb-R114 lane suite running since about 21:02Z;
  - #496 (BUG-261001-fb662e, the validationsuite flake fix): reviewed and accepted, hosted CI queued at 21:24Z.
- *Decision:*
  1. Whichever of odansk and R2 is READY first lands first. The other rebases, re-validates once on top of it, then lands. tb-R114's grant carries over to that re-run.
  2. #496 lands after both, with one CI run on top of both. Cancelling its queued CI now is validation-fixes' call.
  3. Until odansk and R2 have both landed, no other code or doc landing on spm main. Board-only gap publishes stay allowed under tb-R69, and tb-R109's doc hold continues.
  - Cap: point 3 lapses at 2026-10-02 06:00Z if neither has landed.
- *Grounds:* in a serial queue every candidate after the first re-tests once. This order costs no more suites than any other, and puts the critical fix and Ivan's Muse path first.
- *State:* in force until odansk and R2 have landed, or the cap.

**tb-R117. A narrow exception to tb-R109's doc hold for R2's two README hunks** (21:27Z, 2026-10-01; in force from 21:28Z).
- *Evidence:* R2 rev3 touches README.md, a file tb-R109 holds, in exactly two single-line hunks: the skill-agents-management pin line near line 62 (v0.5.30 to v0.5.35) and one new row in the Tools table near line 1541. critical-fixes checked odansk's README hunks: about lines 408–414 and 3442–3510. The two sets are line-disjoint, with more than 300 lines of margin. Under tb-R116, odansk re-validates once on top of R2 in any case.
- *Decision:* R2 may land with exactly those two README.md hunks. The 3-way merge check (git merge-tree) moves to critical-fixes' rebase onto R2, as part of its trunk-line verification. If the merge had conflicted, R2 would have waited for odansk or the tb-R116 cap, without stripping the hunks. The other three held files stay held.
- *State:* in force until odansk lands.

**tb-R118. odansk rev8 takes one R1 lane suite after R2 lands** (21:28Z, 2026-10-01).
- *Evidence:* odansk's round-4 review asked for one narrow fix: the temp-file ownership gate did not cover deletions. The rev8 rework is done. critical-fixes cancelled the queued shared validation, because R2 lands first under tb-R116 and the validation would have gone stale.
- *Decision:* one lane suite by the tb-R101–tb-R112 mechanism, launched after R2 lands, as tb-R116(1)'s single re-validation on top. It starts only at 45 GiB or more free AND CPU idle at 25% or more, and counts inside the ceiling. Only one spm-sized lane suite runs at a time. A paired trim follows. tb-R110's routing applies to round 5.
- *State:* granted in advance; it starts after R2 lands.

**tb-R119. No Daybreak application; models rotate on a policy refusal** (21:52Z, 2026-10-01; Ivan's answer to desk card #26 at 21:51Z).
- *Ivan, verbatim:* «нет. делаем ротацию моделей, если сол отказывает используем соннет или астру или наоборот и тд. в нашей юрисдикции не выдают daybreak и под наш масштаб».
- *Decision (extends tb-R110, for every Ivan-side campaign):*
  1. OpenAI Daybreak access is not pursued; it is not granted in our jurisdiction or at our scale.
  2. A provider policy refusal is routed to another model, not escalated.
     - A refused gpt-6.1-sol producer rephrases and retries once (tb-R110(3)), then re-runs on claude-sonnet-5-5 high or gpt-6-astra high.
     - A refused astra reviewer goes to sonnet (tb-R110(2)). A refused sonnet reviewer goes to astra or sol, and so on.
     - Rotation pairs count as allowed pairs. The refusal line and the finishing model are recorded in the run notes.
  3. Rotation covers refusals only. Capacity limits keep their own handling, and quality escalation stays a workflow decision.
  4. A rotated review stays independent of its producer.
- *State:* in force.

**tb-R120. Fork PRs locked down; public repositories leave our self-hosted runners** (22:05Z, 2026-10-01; Ivan's go in tb-keeper's session: «короче да, давай все что ты предлагаешь … пр вообще не мог делать никто кроме членов организации … переводим все на публичные. на личных маках оставляем пока только то что строго приватное»).
- *Evidence:* public repos (curator, curator-agent-launcher, curator-spec, runner-control) could use the runner groups on Ivan's Macs. tgfs and relux-proxy have repo-level self-hosted runners. The org's fork-PR approval policy was first-time contributors only. A fork PR runs its own workflow, so code from outside could reach ivmbp, macbook-iv or rose-air.
- *Applied by tb-keeper and verified:*
  1. Fork-PR workflow approval is now all_external_contributors, org-wide; repos inherit it.
  2. An org interaction limit makes public repos collaborators-only (issues, PRs, comments) until 2027-04-01T22:05:33Z. It must be renewed before then.
  3. Runner groups ivmbp-manual, macbook-iv-manual, rose-air-projects and runner-control-release-1-2-0 have allows_public_repositories=false and serve only private repos (skill-project-management; licenses on macbook-iv). Default already refused public repos. Fork-PR workflows in private repos stay disabled, and private forking stays off.
  4. curator's ROSE_AIR_RUNNER variable is now false, so its main-push self-hosted job is skipped.
  5. Migration issues: tgfs#65, relux-proxy#2, runner-control#3, curator#104. Until relux-proxy and tgfs migrate, their repo-level runners stay behind points 1 and 2.
- *Rule from now:* public repositories use GitHub-hosted runners only. A private repo leaves self-hosted runners before it goes public, and the groups enforce this.
- *State:* in force. Renew the interaction limit before 2027-04-01.

**tb-R121. Work never stops for lack of another vendor; same-provider review as a last resort** (22:07Z, 2026-10-01; Ivan's rule, relayed verbatim by tb-muse: «в крайнем случае ревью может быть той же провайдера что и продьюсер, останавливать работу нельзя просто из за отсутствия другого вендора»).
- *Decision (extends tb-R80, tb-R110 and tb-R119):* work never stops only because no other vendor is available: a refusal, capacity, an outage or a missing login. After tb-R119's rotation, a review may run on the producer's own provider as a last resort. It stays a separate run and session with a fresh context, and the reason is recorded in the run notes. The next round returns to the normal tb-R80 pairing once another vendor is available.
- *State:* in force.

**tb-R122. The short-suite lane is standing for every Ivan-side root** (23:03Z, 2026-10-01; a quick win of the process-efficiency analysis, approved by Ivan).
- *Evidence:* the analysis (collective-mode/research/process-efficiency) found that suites running for minutes waited 2–5 h in the shared queue, and each needed a ruling of its own (tb-R99, tb-R104, tb-R106, tb-R113, tb-R115). Under tb-R113, nine lane suites ran in 2.4–21 min.
- *Decision:* any root may run a validation suite in the shared short lane when the last run of that suite took under 15 min. The lane is infra-exit's lane root, `skill-project-management-infra-exit/.temp/module-lane`, with machine_max_concurrent_suites=1, and preflight is computed under the same state home. tb-R113's limits are unchanged: one at a time machine-wide, inside the ceiling; over 20 min back to the shared queue; launch at 45 GiB or more free AND CPU idle at 25% or more; never spm-sized. No ruling per suite.
- *State:* in force; the Gate A trial report covers it.

**tb-R123. Capacity work goes first** (23:03Z, 2026-10-01; approved quick win).
- *Decision:* after the critical fix, the next spm-sized lane suites go to work that shortens the suite or CI or removes stale laps: l2vgx1, #472, #495, 19eu3m, and ranqpb after its ruling. The grant is standing, under tb-R112's launch conditions. The ivmbp swap carrying C1 runs in the morning gap between lane suites and does not wait for odansk.
- *State:* in force.

**tb-R124. Train 1: R2 rev3, #496, odansk rev8** (23:03Z, 2026-10-01; approved quick win; amends tb-R116 and tb-R118).
- *Decision:*
  - **Stacking.** Staleness is per file. #496 overlaps neither R2 nor odansk and stacks without re-validation. odansk's tb-R118 lane suite runs on the stacked base (R2 plus #496) as soon as R2's lane suite ends.
  - **Landing.** When all three are accepted: one hosted CI run on the exact top head, one window, and one plain fast-forward push, which lands all three. critical-fixes pushes.
  - **Fallback.** If odansk is not READY within 1 h of the other two, R2 and #496 go as a two-car train.
  - **Red top CI.** Drop the failing member and re-run once.
- *State:* in force until Train 1 lands.

**tb-R125. #496 lands first and alone; Train 1 becomes R2 plus odansk** (23:30Z, 2026-10-01; amends tb-R124).
- *Evidence:* R2's lane suite ended red at 23:29Z, on the flake TestRemotePhaseHeartbeatCannotReReleaseAfterReacquire. That is the flake #496 fixes, not R2's code. #496's earlier hosted CI was cancelled under tb-R116. Its head 01df3e24 is a fast-forward of main b4dd25c8 apart from board records. validation-fixes, tb-muse and critical-fixes all proposed the same order.
- *Decision:*
  1. odansk rev8 takes the lane now. A failure only on the known flake re-runs once on main with #496.
  2. #496 lands alone on a fresh hosted CI of 01df3e24, under tb-R69.
  3. R2 converges onto main with #496 and re-runs its suite after odansk's lane suite. A free shared slot may take it earlier.
  4. Train 1 is R2 plus odansk: one CI on the top head; critical-fixes pushes.
  5. Lane order: odansk, then R2's re-run, then l2vgx1.
- *State:* in force until Train 1 lands.

**tb-R126. The desk contract: a digest every 6 hours, with optimistic defaults** (23:33Z, 2026-10-01; Ivan: «пускай просто каждые 6 часа копит не отвеченные вопросы + сводка по оптимистично принятым решениям»).
- *Decision:*
  - The desk sends Ivan one digest at 00:00, 06:00, 12:00 and 18:00Z: unanswered cards, plus decisions taken optimistically since the last digest.
  - Reversible cards carry a default that acts at reply-by. Security, legal, money, sudo or reboot, and irreversible cards wait for Ivan.
  - Urgent items still go out at once.
- *State:* in force on the orchestrator side. The desk switches its own delivery once Ivan confirms in the desk session, as its brief requires. Confirmed by Ivan in the desk session at 05:49Z, 2026-10-02 («дайджест раз в 6 часов — да»); the desk runs digests at 00/06/12/18Z, and urgent cards go out at once.

**tb-R127. Soft work-in-progress limits for two weeks** (23:33Z, 2026-10-01; Ivan: «правило берем но правило не строгое, если будет срочно нужно запустить что-то я дам знать»).
- *Decision:*
  - Code campaigns producing spm CRs on ivmbp:
    - tb-critical-fixes, until odansk lands;
    - tb-muse, until R3 lands;
    - tb-agents-infra-exit.
  - Test and capacity work (tb-tests, ci-isolation, validation-fixes) rides the tb-R123 lane. Ivan wants the tests in shape first.
  - No suites on the Mac mini until its reboot.
  - At most 8 orchestrators awake on ivmbp.
  - No new campaign until the median queue wait stays under 30 min for two days.
  - Ivan overrides case by case.
- *State:* in force until 2026-10-16.

**tb-R128. Host hygiene is approved once** (23:33Z, 2026-10-01; Ivan: «давай, берем»).
- *Decision:*
  - Desk #22: the go-build auto-trim under 50 GiB goes live (macos-sys).
  - An fseventsd memory-threshold restart daemon on ivmbp and the Mac mini, prepared by macos-sys and installed by Ivan.
  - A standing maintenance slot for reboots and sudo installs; its time is to be confirmed with Ivan.
  - The Mac mini reboot (#28) goes into the first slot.
  - tb-R55 swaps need no slot.
- *State:* in force.

**tb-R129. Narrow targeted builds on the Mac mini after its reboot** (23:35Z, 2026-10-01; amends tb-R127).
- *Decision:*
  - No builds on the mini until its reboot.
  - After IMPACT END, with syspolicyd stable: `go test -p 1` on selected packages, one run at a time across validation-fixes and ci-isolation, and at most two worker runs at once.
  - "One at a time" is the mkdir lock `~/.mini-build-lock` (agreed 23:36Z): owner, run id and start time in `owner`, released by a trap, and breakable after 2 h with a dead PID, with a one-line notice.
  - No full spm or CR validation suites on the mini.
  - Stop and report if load goes above 2x the cores or swap grows.
- *State:* in force until tb-R127 ends.

**tb-R130. Tiered validation** (01:22Z, 2026-10-02; analysis item T1; Ivan, in the dispatcher's session at about 01:10Z: «берем!»).
- *Decision:*
  - A CR's validation becomes two tiers:
    - the fast tier, about 10 min, runs before review: vet, the guards, the short packages, and the tests of the packages the CR touches, selected with `go list`;
    - the full run happens once, as hosted CI on the landing head.
  - The exact-head rule (tb-R69) is unchanged.
  - Owner: validation-fixes, which owns validationsuite. Design, config and scripts start now, ahead of the C2 and C3 builds that wait for the mini reboot. Builds and tests on the mini only after the reboot, under tb-R135.
  - Before any root switches, validation-fixes posts the design as a rule-27 proposal. A switch that changes the SuiteSHA happens only when the root has no CR ready, in validation, or accepted and unlanded (critical-fixes' constraint of 20:10Z). Otherwise it uses a key outside the SuiteSHA, as hmt6xa (a) does.
- *State:* in force.

**tb-R131. CI sharding and the test-optimization epic go first** (01:22Z, 2026-10-02; analysis item T12; Ivan: «надо начинать максимально быстро это один из первых приоритетов», «эпик по оптимизации тестов нужно завершить как можно скорее вместе с выходом из инфры»).
- *Decision:*
  - The test-optimization epic and the infra exit are the two top priorities on the Ivan side.
  - When they compete with other work for a lane or the ceiling, they go first. Train 1 and the critical fix still come before them.
  - T12, CI sharding for the hosted runners, starts now, not by 10-06. Owner: tb-tests. Producer codex gpt-6.1-sol high.
  - tb-tests declares its rule-18 files. It agrees its ci.yml hunks with ci-isolation, whose Train 2 items touch CI, in the orchestrator-channel thread.
  - Landing waits for tb-R116 point 3 to end: after Train 1 lands, or at the 06:00Z cap.
  - l2vgx1 rev3 keeps its lane place after R2's re-run (tb-R125).
- *State:* in force.

**tb-R132. The review policy** (01:22Z, 2026-10-02; analysis item T9, Ivan's version: «дефолт два», «ревью для нас очень важная часть - нужно её проходить полностью»).
- *Decision:*
  - Every review round is two parallel full fresh reviews: claude-sonnet-5-5 high and gpt-6-astra high. Up to three reviewers may be configured when needed.
  - From round 2, add one separate delta reviewer, in its own fresh run (default claude-sonnet-5-5 high). It checks:
    1. that the previous round's findings were fixed correctly;
    2. whether the same class of problem appears anywhere else in the code;
    3. whether the rework broke anything.
  - The orchestrator merges the verdicts. Identical findings (same file, same class) collapse into one; everything else is unioned. Any changes_requested sends the CR back to rework.
  - No cap on rounds. The suite result still gates acceptance. Reviews may start in parallel with validation.
  - tb-R119 (rotate the model on a refusal) and tb-R121 (same-provider review as a last resort) still apply.
  - Rounds already running finish under the old policy; every round started after this ruling follows it.
- *State:* in force.

**tb-R133. Standing rules instead of per-case rulings, as a trial** (01:22Z, 2026-10-02; analysis item T6; Ivan: «ок давай попробуем»).
- *Decision:*
  - Routine grants become condition → action standing rules that orchestrators apply themselves, as tb-R122 and tb-R123 do.
  - When a standing rule covers a case, the orchestrator does not ask. It posts what it does and under which rule.
  - tb-keeper rules only on disputes and exceptions.
  - tb-keeper posts the first set of conversions with the -trimpath ruling at 12:00Z: lane queue order, board-only gap publishes, next-in-order windows.
  - The trial is reviewed against the daily flow report (T11, owned by the dispatcher).
- *State:* trial, until 2026-10-16.

**tb-R134. The spm README becomes a storefront and the CHANGELOG becomes fragments** (01:22Z, 2026-10-02; Ivan decided alone, as routine: «это рутинный вопрос я могу принять решение без согласия. ответ да»).
- *Decision:*
  - The spm README becomes short: what it is, install, links. Its sections move to docs/, one topic per file.
  - The CHANGELOG moves to fragments in changelog.d/<id>.md. The aggregate is generated at release and not committed between releases. Any shared logbook does the same.
  - The doc guards that read README follow in the same CR.
  - Owner: ci-isolation, as a capacity item. It lands after Train 1, once tb-R109's hold has ended, and rebases onto odansk's and R2's doc hunks.
  - Alexey's side gets this as information, not as a question.
- *Clarified* (01:30Z, ci-isolation's fact check): spm commits no CHANGELOG. goreleaser builds its release notes at release, which already meets the rule, so spm gets no changelog.d. The fragment rule applies where several campaigns append to a committed CHANGELOG or logbook; skill-agents-management comes first (9 CHANGELOG commits in 7 days), routed separately.
- *State:* in force.

**tb-R135. tb-R129 covers every Mac mini campaign; idle mini campaigns resume** (01:22Z, 2026-10-02; extends tb-R129; Ivan: «нужно будить оркестраторов»).
- *Decision:*
  - tb-R129's rules cover every campaign on the mini, not only validation-fixes and ci-isolation: network-profiles, model-routing and any other.
    - No builds or tests on the mini until its reboot.
    - After IMPACT END, one build or test at a time across the whole mini, through the mkdir lock ~/.mini-build-lock. At most two worker runs at once.
  - Design, spec and doc work may go ahead now.
  - network-profiles resumes with 11b (confirm takes the edit lock), 15 (the RU translations), the integration-contract card C and N-B planning. Desk #20 stays open; its landed defaults stand.
  - model-routing resumes with curator-model-router issues #13–#18. W4 and W5 follow after M4 Gate B.
- *State:* in force until tb-R127 ends.

**tb-R136. Narrow builds on the Mac mini are allowed now; the reboot is deferred** (02:19Z, 2026-10-02; amends tb-R127, tb-R129 and tb-R135; Ivan, in the dispatcher's session at about 02:10Z: «стремно перезагружать - это прям критично? есть риск вообще потерять его - я бы подождал первой недели октября - возьму в аренду еще одну тачку»).
- *Evidence:* the mini's syspolicyd crash loop slows exec but does not stop the host. Ivan defers the reboot until a second rented machine is in place, around the first week of October. tb-R129 and tb-R135 tied every mini build to the reboot, so the four mini campaigns (validation-fixes, ci-isolation, network-profiles, model-routing) would stall for days.
- *Decision:*
  - Narrow targeted builds on the mini are allowed now: `go test -p 1` on selected packages, one build or test at a time across all mini campaigns, through the mkdir lock ~/.mini-build-lock, with at most two worker runs at once.
  - Before each build, check that syspolicyd is up: `launchctl print system/com.apple.security.syspolicy | grep -E 'state|successive'`. If it is down or crash-looping, wait rather than retry, and release the lock while waiting.
  - Full spm suites and CR validation suites stay on ivmbp (queue or lanes) and hosted CI.
  - Stop and report if load goes above 2x the cores or swap grows.
  - Desk #28 is deferred by Ivan until the new machine. Before any mini reboot or retirement, Matrix and the critical services move to the new machine or are freshly backed up. Backup status is asked of macmini-infra now, read-only, with no reboot and nothing destructive.
- *State:* in force until the mini is rebooted or retired; tb-R127's end date still applies to the WIP limits.

**tb-R137. GPT-only subagents on the Mac mini today** (02:57Z, 2026-10-02; Ivan, in macmini-infra's session, relayed verbatim at 02:57Z: «передай всем оркестраторам тут на машине что сегодня используем только gpt субагентов / остальных только как резерв в случае отказа»; applies tb-R132 on the mini for the day).
- *Decision:* for every Ivan-side orchestrator on the Mac mini, for 2026-10-02:
  - Producers: codex gpt-6.1-sol high.
  - Reviews keep tb-R132's structure, with GPT models:
    - two parallel full fresh reviews, each a separate codex gpt-6-astra high session;
    - from round 2, a delta reviewer, also gpt-6-astra high in its own session;
    - each is noted as 'same-provider review (operator rule, tb-R137)'.
  - Claude (sonnet, opus) and Muse subagents are fallback only: for a GPT refusal, failure, capacity limit, outage or rate limit. The reason goes in the run notes.
  - Orchestrator sessions are unchanged. In-flight runs finish undisturbed; new spawns follow this ruling.
  - Off the mini (ivmbp), tb-R132 stands as written unless Ivan extends this ruling.
- *State:* in force for 2026-10-02 (UTC).

**tb-R138. GPT models first for every Ivan-side orchestrator; astra low for reviews** (03:00Z, 2026-10-02; Ivan, directly in tb-arbiter's session: «передай всем оркестраторам что сейчас используем активно и в первую очередь chatgpt модели - код на сол 6-1 ревью на астра лоу - клод и мьюз только как разервные в случае отказа»; extends tb-R137 beyond the mini and amends tb-R132 and tb-R121).
- *Decision:* for every Ivan-side orchestrator, on every host:
  - Producers: codex gpt-6.1-sol, effort high.
  - Reviewers: codex gpt-6-astra, effort LOW. tb-R132's structure stays:
    - two parallel full fresh reviews, each a separate astra session;
    - from round 2, a delta reviewer, also astra low in its own session;
    - the verdicts merged as tb-R132 says.
  - Same-provider review is now the default, not tb-R121's last resort. Note each review as 'same-provider review (operator rule, tb-R138)'.
  - Claude (sonnet, opus) and Muse subagents are fallback only: for a GPT refusal, failure, capacity limit, outage or rate limit. The reason goes in the run notes. tb-R119's rotation on a refusal stays.
  - Orchestrator sessions are unchanged. In-flight runs finish undisturbed; new spawns follow this ruling.
- *State:* in force from 03:00Z for the next few days, until Ivan's next instruction (Ivan, 03:01Z: «это будет правило на несколько ближайших дней до моего следующего указания»). It supersedes tb-R137, including tb-R137's reviewer effort.

**tb-R139. Research on gpt-6-astra max on the Mac mini, to use its weekly limit** (03:05Z, 2026-10-02; Ivan, directly in tb-arbiter's session: «на мини тачке 70% недельного лимита - можем нагрузить её какими нибудь ресерчами или большой работой через астру макс на ресерчь», then «по всем можно, и можно еще взять что то про нетворк профили например - в обшем все что там в работе»).
- *Decision:*
  - Mini campaigns may run research tasks on codex gpt-6-astra, effort max, on the mini's own account, for anything in their current work. tb-R138 still governs producers and reviewers.
  - Assigned now:
    1. validation-fixes: test-suite profiling for T1 and T12 (tb-R130, tb-R131). Per-package times from recent validation records and CI logs, the fast-tier package set by `go list`, and a shard plan sized for an 8 GB runner. Hand it to tb-tests.
    2. ivan-curator: a read-only pre-release audit of every repository planned for the open-source release, git history included:
       - secrets;
       - references to the employer and clients;
       - personal addresses;
       - LICENSE, NOTICE and SPDX consistency.
       The report goes to oss-release, Ivan and relux-legal.
    3. ivan-curator: the curator-spec owner-review triage (#101, #106–#112). Ivan delegates it (desk #27 item 3). Only the flagged issues go to Ivan, through the desk. #106 and #108 are not implemented before his decision.
    4. model-routing: design research for curator-model-router #13–#18.
    5. network-profiles: research for N-B planning and the integration-contract card C.
    6. ci-isolation: the tb-R134 README inventory, a topic map of the 3535-line README into docs/ with the guards that read it.
  - Rules:
    - Research is read-only: no commits beyond the task's own research artifacts, no pushes, and no builds outside ~/.mini-build-lock (tb-R136).
    - Sensitive findings (secrets, the employer, clients, personal data) never go into shared rooms. They stay in the local report file. Rooms see only counts and the file path. Details go to Ivan through the dispatcher, and to relux-legal.
    - Budget guard: stop starting new research when the mini's weekly limit falls to 25% remaining, so that producers and reviews keep their capacity. macmini-infra posts the remaining limit in #macmini-infra every 6 h while research runs.
- *State:* in force until the mini's weekly limit resets, or until Ivan changes it.

**tb-R140. One explicit-complete integration run closes codex P1** (03:27Z, 2026-10-02; an exception under tb-R133; codex-fix's request, via the dispatcher).
- *Evidence:* STORY-260929-2urftk (p1-integer-tool-arguments) landed on relux-works/codex as 35af013 (signed, valid; authored by Ivan). Its tree, ece69834, equals CR-TASK-260929-17t419-6's candidate_tree_oid; checked on GitHub at 03:27Z. The board is a separate-owner board. The integration run RUN-260930-e0c3b9 was told that "the runner lands synchronously", so the model never ran `worktree complete`. The runner's `integrate` was then refused with board_owner_separate. task-board has no admitted path that routes a separate-owner landing to `complete --landed-commit`.
- *Decision:*
  - codex-fix spawns ONE developer/implementer integration run on TASK-260929-17t419 revision 6, under tb-R138's models.
  - Its brief does only this: re-check that 35af013's tree equals ece69834 and that it is signed; run `task-board worktree complete STORY-260929-2urftk --cr TASK-260929-17t419 --revision 6 --landed-commit 35af013b901ed4be1b88416068f140e9ca5cc85d`; then exit. No code edits.
  - Cancel any recovery successor first. Record the reason in the run notes: "tb-R140 workaround for the separate-owner complete gap".
  - codex-fix files a BUG on the spm board: on a separate-owner board, the runner must route a verified external landing to `complete --landed-commit`, and the integration brief must not say that the runner lands. Attach the two logs.
- *State:* a one-time exception; it does not repeat without a new ruling.

**tb-R141. tb-R132 reviews record nothing until the verdicts are merged** (03:33Z, 2026-10-02; a standing rule under tb-R133; replaces the interim practice posted at 02:00Z).
- *Evidence:* task-board records each reviewer's verdict as it arrives, and a lone recorded accept is irreversible: withdraw_cr refuses an accepted CR (change_request_state_conflict).
  - On 2w6op9 round 2, a sonnet accept was recorded while two astra panels requested changes. A developer rework spawn then tried a runner landing of the accepted revision. Only the write-boundary gate stopped it (infra-exit, 03:33Z).
  - The same tb-R132 panels found a real data-loss bug in the already-landed 28odl0. A re-import drops kv_context_size, prefill_chunk_size and reasoning_effort. It is being fixed in TASK-261002-3va9hn before 3hrwzn uses the importer.
- *Decision,* until task-board supports parallel reviewers:
  1. All of a round's tb-R132 reviewers (both full reviews, and the delta reviewer from round 2) run as NON-RECORDING research tasks. They replay the CR patch, check the write-tree, and write their verdicts as outcome resources. Start them at the same moment.
  2. The orchestrator merges the verdicts as tb-R132 says.
  3. Only then, ONE recording CR reviewer run records the merged verdict, citing the outcome resources. It records accept only when every merged verdict is accept.
  4. A CR already holding a lone accept that the merge contradicts is superseded by a successor CR, and closes with close-landed --superseded-by, as infra-exit did with 2xj2l5. It is never landed.
  5. critical-fixes' issue on the tool gap becomes a BUG on the spm board, with capacity priority under tb-R123.
- *Clarified* (04:00Z, codex-fix's check): a research panel with review=none publishes an evidence-only CR (kind story_final, empty repository delta, state ready), and its task waits at to-review. Leave those CRs as they are:
  - nobody accepts, completes or lands them;
  - they carry 'R141 panel' in the title;
  - flow and WIP counts exclude them;
  - one sweep closes them all after #499 / BUG-261002-whijfm lands.
  This avoids a recording reviewer and a complete per panel.
- *State:* standing, until the task-board fix lands.

**tb-R142. codex P1: complete against the exact rev-6 suite, then restore** (03:35Z, 2026-10-02; extends tb-R140; codex-fix's request R140b).
- *Evidence:* tb-R140's run RUN-261002-e91c74 re-checked 35af013 (an ancestor of relux/main, tree ece69834, signature good). Its `worktree complete` then failed with validation_suite_changed. The separate-owner path in spm's selectReviewedValidationSuite accepts evidence only when the evidence suite digest equals the current one. codex-fix added two app-server websocket flake quarantines to its untracked control-root config at 00:41Z on 2026-10-01, after rev 6 was validated. The current digest is 582bc3d0…; the evidence digest is 4bf6b859…. The evidence suite is the stricter one: it ran without those quarantines. Nothing was written.
- *Decision:*
  1. Before the run, codex-fix sets the config's validation commands to the exact rev-6 suite (the current one minus the two quarantine terms) and verifies that the SuiteSHA256 is 4bf6b85935c1…. No other codex-fix run may be active, and none starts until step 3.
  2. ONE developer/implementer integration run with tb-R140's brief: re-check, `worktree complete … --landed-commit 35af013…`, exit.
  3. Immediately afterwards, codex-fix restores the current suite and verifies its digest is 582bc3d0… again.
  4. It posts both digest checks and the board result (done or board_published; any successor cancelled) to tb-keeper. No other config change goes into the window.
  5. A repeat needs a new ruling. BUG-261002-whijfm should also cover suite drift that only quarantines after the evidence (the evidence suite is a strict superset).
- *State:* a one-time exception.

**tb-R143. The tb-R116 cap moves to 07:00Z, for Train 1 only** (03:40Z, 2026-10-02; amends tb-R116 point 3).
- *Evidence:* tb-muse, 03:40Z: R2's lane suite (RUN-261002-2d72a7) ran slower under the double load from 02:00Z to 03:00Z. Its LANE ENDED is now expected at about 04:25Z. The R141 non-recording panels (3r9tem, 3l6y11, delta 1ry8f7) and the one recording reviewer then make R2 READY at about 05:10Z. With about 70 min of train CI on ivmbp's runner (rose-air is down), Train 1 lands at about 06:20Z, past the 06:00Z cap. If other code or doc landings resumed at 06:00Z, they would stale Train 1's tested top head in mid-CI and waste the run.
- *Decision:*
  - tb-R116 point 3 holds until 07:00Z, and only for Train 1 (R2 plus odansk). It lapses at once when Train 1 lands.
  - If Train 1's top-head CI is not green by 07:00Z, the hold lapses anyway and normal first-READY order resumes. Train 1 then re-tests once, as tb-R125 provides.
  - Board-only gap publishes stay allowed under tb-R69. tb-R109's doc hold continues until odansk lands.
  - The ivmbp swap still waits for the first gap between lane suites after 05:00Z. Train 1's CI builds from source on the runner, so it does not collide with the swap.
- *State:* in force until Train 1 lands, or until 07:00Z.

**tb-R144. Explicit-complete integration runs become standing for codex-fix stories** (03:55Z, 2026-10-02; a standing rule under tb-R133; generalizes tb-R140; codex-fix's request).
- *Evidence:* every codex-fix story closes through `worktree complete` on a separate-owner board, and each close hits the runner gap (BUG-261002-whijfm) until it is fixed. tb-R140 and tb-R142 showed that the explicit-complete run is safe when it checks the landed commit first.
- *Decision:* until BUG-261002-whijfm lands, codex-fix may close each landed story with ONE developer/implementer integration run under tb-R138's models. Its brief:
  1. re-check that the landed commit is an ancestor of the target main, that its tree equals the accepted revision's candidate_tree_oid, and that its signature is good;
  2. run `task-board worktree complete <story> --cr <cr> --revision <n> --landed-commit <sha>` once;
  3. exit.
  - Cancel any recovery successor first, and afterwards report one line per close to tb-keeper.
  - If complete fails on suite drift, or on anything other than the known #429 failure after a successful complete, codex-fix stops and asks. It does NOT apply an tb-R142-style suite window without a new ruling.
  - Per codex-fix's own rule, the validation suite stays frozen while any CR revision is ready, reviewing or accepted.
- *State:* standing, until BUG-261002-whijfm lands.

**tb-R145. Binary swap #6 on ivmbp: C1 and everything landed since 10c4b582** (04:16Z, 2026-10-02; under tb-R55 and tb-R123).
- *Done:* `curator global upgrade` from tb-arbiter's unhosted shell, 04:13:29Z–04:15:18Z, rc=0, from spm main 44d672a8.
  - task-board 1361260b → 68193c03; tb-sessiond 8457d455 → 13e5cb00.
  - It carries C1 (#491, remote_only_commands), #496, #494, #492, #488, #483 and #481, and #480.
  - Probe: the new task-board has remote_only_commands (7 hits). The old one had 0, checked at 04:12Z before the swap.
  - Daemons were not restarted (tb-R55, tb-R88).
- *Deviations:*
  1. The IMPACT START post failed, because the a2a CLI rejects the "impact-start" subtype that rule 31 names. The swap ran anyway, because the commands were chained with ';'. Disclosed in a combined incident post at 04:16Z.
  2. curator's build-cache sweep deleted the old task-board build 1361260b while 24 live processes still executed it. Re-execs of that absolute path (successor runner launches, printed observation commands) now fail. Rollback needs a rebuild at 10c4b582.
- *Follow-ups:*
  - The sweep bug goes to ivan-curator: do not sweep a build that a live process executes.
  - The rule-31 subtype gap goes to a2a-swarm.
  - tb-tests may set remote_only_commands.
  - Swaps now post the impact notice and run the upgrade only if the post succeeded (&&, never ';').
- *State:* done.

**tb-R146. ivmbp returns to the old model rule; tb-R138 stays on the Mac mini only** (04:18Z, 2026-10-02; Ivan, directly in tb-arbiter's session: «я поспешил с переходом на кодекс на машине ivmbp - нужно вернуть старое правило - ресет будет только через 13 часов с этого момента»; amends tb-R138).
- *Evidence:* the codex limit of ivmbp's account resets only at about 17:18Z (13 h from 04:18Z). tb-R138 moved every ivmbp review onto codex as well. The Mac mini has its own account (67% left, tb-R139).
- *Decision:*
  - On ivmbp, from now: the model rule before tb-R138 is back.
    - Producers: codex gpt-6.1-sol high.
    - Reviews follow tb-R132 as written: two parallel full fresh reviews, claude-sonnet-5-5 high and gpt-6-astra high, plus a delta reviewer from round 2 (default claude-sonnet-5-5 high), run with tb-R141's non-recording mechanics.
    - tb-R121 applies again: same-provider review is the last resort.
  - When ivmbp's codex limit runs out before the reset, codex roles rotate to Claude under tb-R119 and tb-R121 (work never stops), with the reason in the run notes.
  - On the Mac mini, tb-R138 and tb-R139 stand unchanged.
  - Runs already spawned under tb-R138 on ivmbp finish undisturbed; new spawns follow this ruling.
- *State:* in force on ivmbp until Ivan changes it. Review it after ivmbp's codex reset at about 17:18Z.

**tb-R147. ivmbp trial: Muse max producers, sol 6.1 medium reviews** (04:19Z, 2026-10-02; Ivan, directly in tb-arbiter's session: «давай попробуем код на muse max, ревью на sol 6.1 medium, если закончатся лимиты на кодексе то ревью включаем на соннет 5-5 хай»; supersedes tb-R146 on ivmbp).
- *Decision:* on ivmbp, from now, as a trial:
  - Producers: Muse, effort max.
  - Reviews keep tb-R132's structure and tb-R141's mechanics, on codex gpt-6.1-sol, effort MEDIUM:
    - two parallel full fresh reviews, each a separate session;
    - from round 2, a delta reviewer, also sol medium;
    - then one recording reviewer, also sol medium.
    Producer and reviewers are now different providers, so tb-R121's concern does not arise.
  - Before each review round, check that ivmbp's codex account still has quota. If it does not, every review role of that round goes to claude-sonnet-5-5, effort high, until the codex reset at about 17:18Z; after the reset, sol medium again. The reason goes in the run notes. (Wording clarified at 04:20Z: 17:18Z is the reset, not the moment the quota runs out.)
  - When Muse refuses or fails, the producer rotates under tb-R119 (sol high first), with the reason noted.
  - Runs already spawned finish as they are; new spawns follow this ruling.
  - On the Mac mini, tb-R138 and tb-R139 stand unchanged.
- *State:* a trial on ivmbp until Ivan changes it. tb-keeper reports the first results (refusals, round counts, findings) to Ivan.

**tb-R148. Card C, the network-profiles integration contract rev 2: Ivan's decisions** (05:17Z, 2026-10-02; Ivan, directly in tb-arbiter's session; rule 27, the accountable decider).
- *Decided:*
  - **D1 adopt** rev 2 as the contract for N-B and N-C1 (it stays DRAFT until implemented). Ivan adds: «неплохо бы уже иметь задачи на связку библиотеки и потребителей». network-profiles creates the wiring tasks now (N-B, N-C1, N-C2 and the spec amendments) and files module asks with each consumer owner.
  - **D2 (a)** «ок берем а и правим спеки». A new `inherited` origin, with precedence explicit → inherited → runtime → project → operator, and the reference persisted at reservation. N3/N4 are amended before N-C1.
  - **D4** a typed `Network{Patch, Record}` field on LaunchRequest/SpawnRequest, applied after ChildEnv («точно не колбеки»).
  - **D5** record the route in suite evidence (profile ref and digest, or "ambient"), never the value.
  - **D7** role-based ownership, unchanged.
  - **D8** Muse is `network_scope_unsupported` until verified. Ivan wants the verification started as early as possible, and the decision taken on its result.
  - **D9** fix the spec erratum.
  - **D10** sign tag v0.1.0.
- *Decided later the same morning:*
  - **D3 (b) NOW** (05:26Z, «давай сейчас да»). A named `kind=direct` profile ships in v0.2.0 together with D2 (issue #17). Estimate: about 400–600 lines, 3–4 h, low risk.
  - **D6 deferred** (05:36Z, «да»). The carrier for tracked launches through ax is chosen when ax implements session launch; until then, tracked mode with `--network` refuses (N9). The context is recorded in relux-works/agent-session-manager#68. The direct `curator run --network` path (N-B) and the task-board / tb-sessiond paths (N-C1) do not depend on it.
  - **MCP:** Ivan wants the MCP env behaviour verified in the other harnesses too («про mcp нужно убедиться про поведение и в других средах»).
  - **D8 (A)** (05:44Z, «ок … да»). Muse 1.4.1-R4503.1 and 1.4.2-R4684.1 are SUPPORTED with adapter `muse-env-v1`: the generic patch, plus the set half injected into each MCP server's env block, plus hook-command wrapping. Evidence:
    - R0–R3, R5 and R6 pass;
    - R6 with an unreachable proxy was closed by a calibrated SIGKILL sandbox detector: no direct fallback;
    - R4 and R7a (scrubbed child env) are fixed by the adapter.
    Shell and tool children (R7b) are a documented gap until a PTY row after R2 lands. Ivan: «не забыть».
- *State:* in force.

**tb-R149. skill-agents-management is developed jointly by both operators** (06:08Z, 2026-10-02; desk #24, Ivan via ivan-desk at 06:06Z, verbatim: «мы разрабатываем его совместно с алексеем - друг с другом согласуем видение, критические вопросы дизайна решаем вместе, рутинные и баги - по отдельности, много текущих решений принято и должно быть записано в вики.»).
- *Decision (Ivan's position; ownership needs both operators, rule 24):*
  - relux-works/skill-agents-management is JOINT: the two operators agree its vision with each other and decide critical design questions together. Each side handles routine work and bugs on its own.
  - Its placement under Alexis's "swarm" space (tb-R108) is placement, not ownership. Its windows and landings stay in shared #skill-agents-management.
  - This matches Ivan's general scope of 2026-10-01: routine repository and process changes are his alone, with Alexey informed; significant architecture needs both.
  - The decisions already taken go into a decision record in relux-works/wiki (the dispatcher compiles it; Alexis's side adds or corrects entries there).
- *State:* Ivan's side in force. Confirmation from Alexis's side requested.

**tb-R150. The whole Matrix server is backed up to ivmbp every 6 h** (07:02Z, 2026-10-02; desk #32 area; Ivan via his dispatcher at about 07:00Z, verbatim: «настроить надо бекапы матрикс - всех сообщений аккаунт и вот этого всего на мою машину (и потом на машину алексея тоже). досутп по ssh и бекап куда нибудь в директорию бекапов»).
- *Decision (Ivan's):*
  - The scope is the tuwunel database (all messages, accounts, rooms and keys), the media store and the server config.
  - Mini side (macmini-infra): an online RocksDB backup if tuwunel supports one, else a short pause. A restart or pause is B4. It needs Alexis's side's AGREEMENT in #a2a-swarm › matrix-backup (macmini-infra's 02:24Z proposal, re-pinged 07:03Z), plus a 30-minute notice in #a2a-swarm and #lobby. Notice alone is not enough. A launchd job every 6 h writes ~/backups/matrix/matrix-<UTC ts>.tar.zst with a .sha256 and a MANIFEST, in a 0700 directory, keeping 8.
  - Access: read-only pull by ivmbp's own key, restricted to rrsync -ro or an equivalent forced read command, limited by from= to ivmbp's tailnet address. The authorized_keys line went to the mini as a file, not through a room.
  - ivmbp side (the dispatcher): a LaunchAgent pull every 6 h, sha256 verification, keeping 28 in ~/Backups/matrix-a2a (0700), with a last-ok file for freshness checks.
  - The archives are secret-bearing and never pass through rooms.
  - Alexey's machine gets the same pull model with his own key, after his go.
- *Not decided:* desk #32's restic/R2 offsite finding (one age recipient, on the mini only) stays open with Ivan.
- *State:* requested of macmini-infra at 07:02Z, reply by 09:00Z.
- *Amended 07:05Z:* step 3 (the read-only pull) is installed. rrsync is unavailable (openrsync only), so a forced-command guard admits only an rsync --server --sender read of backups/matrix; fingerprint SHA256:vO949/bf…. Step 2 (the backup script, keep 8) is ready but not scheduled. Step 1 waits for Alexis's side to choose (a) a one-time container recreate with the backup path set, about 1 min down, after which every backup is online (preferred), or (b) a pause of a few seconds on every run.

**tb-R151. Network-profile consumers get priority, right after tb-R131** (07:21Z, 2026-10-02; Ivan via ivan-network-profiles' session, 07:xxZ, verbatim: «узнай у моего кипера можем ли приоритезировать»; Ivan wants a root session started on a profile, and subagents spawned on a profile, soon).
- *Evidence:* N-C1 (TASK-261002-re5ew4, critical-fixes, in backlog) waits on network-profiles' v0.2.0 (ETA 2.5–4 h) and on M2/M3, whose timing is unknown. Every task-board child is already built through vendorplugin.BuildLaunch (launch_plan.go:475 on main, verified), so subagent spawn needs only D4's typed carrier plus v0.2.0. The tb-sessiond hosts build env from os.Environ and do need M3. D4 (TASK-261002-143e84) is in development and N-B (TASK-261002-1g5dyh) in backlog, both on infra-exit's board.
- *Decision:*
  1. N-C1 splits into N-C1a, subagent spawn (D4 + v0.2.0), and N-C1b, the tb-sessiond hosts (after M3 / LP Phase 0). critical-fixes owns the split.
  2. D4, N-B, N-C1a and v0.2.0 rank right after the two tb-R131 top priorities and the critical fix, ahead of other non-critical work, for ivmbp lanes, the ceiling and reviews. Ranking them above tb-R131 is Ivan's call.
  3. infra-exit puts N-B next after D4 and posts ETAs by 08:30Z; any displaced tb-R131 item goes to Ivan with evidence.
  4. critical-fixes starts N-C1a once D4 lands and v0.2.0 is tagged; Story-branch pre-work may start now.
- *State:* in force unless objected to with evidence by 08:30Z.

**tb-R152. Short Matrix downtime is tb-keeper's call; notice, not agreement** (07:25Z, 2026-10-02; Ivan directly in tb-arbiter's session, verbatim: «если речь об одной минуте простоя матрикса то может брать это решение без доп согласования»).
- *Decision (Ivan's):*
  - A planned Matrix server downtime of about one minute (a container recreate, restart or pause for maintenance or backups) needs no further agreement from either operator's side. tb-keeper decides it.
  - Each one still gets a notice in #a2a-swarm and #lobby at least 30 minutes ahead, so that doorbells and orchestrators expect the gap. The notice gates the action (&&).
  - Anything longer than a few minutes, or anything that changes the server's image, data or federation, is not covered and still needs both operators.
- *First use:* tb-R150 step 1, option (a): recreate a2a-matrix once with TUWUNEL_DATABASE_BACKUP_PATH (+ BACKUPS_TO_KEEP=2) set; image, env and ports otherwise identical; about 1 min down. This supersedes tb-R150's 07:05Z wording that B4 needs Alexis's side's agreement.
- *State:* standing.
- *Amended 07:26Z (tb-R150 + tb-R152):* option (a) is dropped. tuwunel 1.9.3's online backup runs only through the admin-room command, which needs a human server admin; agents never get admin. Instead: a one-time clean stop of under 1 min at 08:00–08:10Z for the baseline archive, then a 6-hourly 'docker pause' of seconds (capped at 60 s) for crash-consistent archives. A fixed, published UTC schedule counts as tb-R152's notice for every recurring run. A restore drill of one pause archive is owed.
- *Amended 09:05Z:* the codex-env-v1 adapter joins tb-R151's priority set. network-profiles' harness check (08:50–08:56Z): codex-cli 0.159.0's MCP stdio children do not inherit the proxy variables, while an explicit env block works. codex is the primary producer harness (tb-R138), so subagents on a profile need it. claude-code 2.1.287 passes as generic-env-v1, and its allowlist tuple may land now.

**tb-R153. The shared validation queue on ivmbp is unwedged; the empty-lock fix goes to infra-exit** (11:27Z, 2026-10-02).
- *Evidence:* no shared-queue suite had started since 05:38Z. ~/Library/Application Support/task-board/provider-limits/validation-suites/state.lock was 0 bytes with mtime 05:38:23Z, and every state.json heartbeat was frozen at 05:38:23Z. Both recorded holders were dead PIDs (c8c69f/75132, 3644eb/99309). In semaphore.go on main, updateUntil creates the lock with O_CREATE|O_EXCL and writes the owner JSON afterwards; breakStaleLock fails json.Unmarshal on an empty file and never breaks it. A process that died between those two steps leaves a permanent machine-wide deadlock. Diagnosed by infra-exit, confirmed by validation-fixes, critical-fixes and tb-keeper.
- *Action:* tb-keeper posted an IMPACT notice and removed the empty lock at 11:26:44Z (the notice gated the removal, with a check that the file was still 0 bytes and unchanged). At 11:27:09Z the queue was live: the dead holders swept, 6ce3fc holding, fresh heartbeats. The state.json snapshot is kept in keeper/state/evidence/ (sha256 dd34a2aa…).
- *Decision:*
  1. The queue keeps its recorded order under ceiling 1. A waiter whose run fails on re-exec of the deleted 1361260b build re-spawns through the shim and keeps its turn.
  2. #408 runs in critical-fixes' private tb-R1 lane and drops its shared ticket at handoff.
  3. The fix is owned by infra-exit and reviewed by validation-fixes: an empty or unparseable lock older than lockBreakAge counts as stale; lock creation becomes atomic with its content (a temp file plus os.Link); regression tests for an old empty lock, a simulated crash between create and write, and a fresh empty lock that must NOT be broken. It is critical-class and takes the tb-R1 lane right after #408.
- *State:* the unwedge is done; the fix is in progress.

**tb-R154. -trimpath for validation suites: option (a), the narrow key outside the SuiteSHA** (12:00Z, 2026-10-02; TASK-261001-hmt6xa; advice converged at 20:11Z, 2026-10-01: critical-fixes, validation-fixes, tb-tests, ci-isolation, with ci-isolation's 20:12Z amendment; no objection by the reply-by).
- *Evidence:* ivmbp's go-build cache held 715 compiled-package archives in 6 sizes, 20.7 GiB or 72% of the cache (macos-sys, 08:45Z): the same packages re-cached per worktree. Ceiling 1 came back four times on 2026-10-02, with refill bursts of 13–50 GiB/h. The precondition is met: #495 (hmt6xa) landed in Train 2 at 07:01Z, main has zero first-party runtime.Caller call sites, and the ciguard sweep forbids new ones. #497 / TASK-261002-ranqpb: head 0b6c0996, CI 36986316352 green, review round 3 accepted, its base behind main only by board Records.
- *Decision:*
  1. A narrow opt-in key, spawn.validation.trimpath: true, outside the SuiteSHA. The runner appends GOFLAGS=-trimpath and sets GOROOT per command dir via SuiteStateHome.Environ. The flag is recorded in each validation record as provenance: outside identity, inside evidence. The command-prefix form (b) is a fallback only.
  2. Author ci-isolation, reviewer validation-fixes (its optional seam review does not block). #497 lands in the next free window via junction-proof.
  3. Opt-in comes only after the tb-R55 ivmbp swap carrying #497, because a pre-swap binary rejects the key and live runs re-read the frozen config. Then each root sets the key before its next spawn and announces it. No early opt-in. Switching strands no CR.
  4. First opt-ins, by cache impact: tb-muse's clone, infra-exit, the spm main root, tb-tests' go-tests roots, and codex-fix where Go applies.
  5. Hosted CI does not switch now; if later, after l2vgx1 and 19eu3m, keeping the ordering pins. tb-R69's tested-head evidence stays the plain hosted CI run.
  6. The cache cap remains the step after (tb-R72(4)). macos-sys measures growth for 24 h after the first opt-ins.
- *State:* in force.

**tb-R155. The first tb-R133 standing rules** (12:00Z, 2026-10-02; trial until 2026-10-16). When one of these covers a case, the orchestrator does not ask: it posts what it does and cites the rule. tb-keeper rules only on disputes and exceptions.
- *S1, lane order:* the next lane suite (module lane, tb-R122 short lane or shared queue) starts after the previous LANE ENDED, in queue-claim order, with no grant. It launches at 45 GiB or more free and CPU idle at 25% or more (tb-R112), inside the ceiling (tb-R72); a critical fix may go first (tb-R131). LANE START and LANE ENDED are posted with the run id, free space and duration. A slot that looks stuck is checked in the queue state before anything is cancelled (tb-R153).
- *S2, board-only gap publishes:* a signed, fast-forward publish that touches only .task-board/ may go to spm main between landing windows under tb-R69, with no grant. Post the SHA and the git diff --name-only proof. Never during an open window, unless the window holder agrees.
- *S3, next-in-order windows:* with CI green on the exact head to land, and being next in first-READY order or the only READY, the lander opens its own window (READY + WINDOW OPEN, base, until, 30 min by default). No grant. A base moved only by board Records lands via junction-proof (tb-R69/tb-R71). The window closes with LANDED or ABORTED.
- *S4, review panels and post-landing CI:* tb-R141's mechanics are standing. Panel CRs stay untouched until the post-#499 sweep, and done panel workspaces may be released after a canary check. A changes_requested is resolved by rework or by a dispute to tb-keeper, never waived by the CR's owner (tb-R132). After a landing, cancel the land/* branch's push CI when 'CI main' covers the same SHA.
- *State:* trial, reviewed against the daily flow report (T11).

**tb-R156. Push hold on relux-works/curator** (16:09Z, 2026-10-02; amended 16:10Z; a protective hold pending Ivan's decision).
- *Decision:*
  - No pushes to any ref of relux-works/curator (main, board-state commits, .task-board resources, .research, remote-gate gate/STORY-* snapshot branches, candidate-check/*, tags), from 16:09Z until Ivan decides. A content review of the public repository is in progress.
  - Local work and local commits may continue. Nothing is deleted, rewritten or forced on any ref; a gate's automatic cleanup of its own snapshot counts as a push and is paused too.
  - It applies to every writer: ivan-curator, any run or bot pushing as Relux Bot (landing runs, remote-gate, close-landed publication from the curator-spec and launcher roots) and the second-operator migration work.
  - ivan-curator acked at 16:11Z (applied 16:13Z): 0 unpushed local commits; the landing queue, remote-gate runs and manual candidate-check pushes are paused. Consequence: the curator v0.15.0-rc.3 chain, and with it the next ivmbp tb-R55 swap, waits for the lift.
- *State:* in force until Ivan decides.

**tb-R157. #408 scope: what blocks the critical fast track** (18:31Z, 2026-10-02; a dispute under tb-R132, raised by critical-fixes before round 4).
- *Evidence:* #408's defect (a stale snapshot after converge makes a CR silently revert trunk) has been closed since round 1. Rounds 2–3 extended the revert guard to repeated converges, every lifecycle edge, rollback on converge failure and an earliest-base anchor. Round-3 panel b (TASK-261002-3b2957) found 3 highs: F1, an external HEAD.lock between the branch CAS and re-attach leaves HEAD detached (panel b says the window exists in base; a real fix is a durable converge intent plus recovery); F2, a later converge re-derives the anchor from a truncated proof; F3, an absent proof with a retained anchor is treated as ordinary publication. F2 and F3 need a tampered registry behind the write boundary.
- *Decision:* blocking for #408 = the revert guard holds on every legitimate lifecycle path, no regression against base, and no pre-existing defect made worse.
  1. F2 and F3 are fixed in round 4 (keep an existing anchor, backfill only an empty one; an anchor without a proof becomes the typed refusal), with regression tests.
  2. F1 is non-blocking ON CONDITION that round 4's fresh delta reviewer confirms against base that (a) the same window exists in base and (b) base's outcome there is the same or worse (the new rollback must not turn a recoverable state into a worse one). If (a) or (b) fails, F1 blocks. Before landing, F1 is filed as its own BUG, linked from #408's CR, and the residual is stated in a code comment at the CAS/re-attach site.
  3. Further tamper-resistance beyond the anchor becomes a documented bound plus a follow-up BUG. Reviewers still report such findings; the merge classifies one as non-blocking, citing this point, only if it needs a write behind the boundary and is not a regression.
  4. Round-3 panels a and delta are merged before round 4; their findings on legitimate paths block as usual.
  5. The lane is unchanged: #408 is next in the R1 lane after m8mzsw r3.
- *Scope:* #408 only; a standing rule may follow if the pattern recurs. Precedent: the 11:46Z C3 TOCTOU dispute (a documented residual verified by the next reviewer).
- *Posted:* $8uLmKKHHX4RzaMGiiL4BSgT-ylpZPhJ8g4xmn-RxQhQ.
- *State:* in force.

**tb-R158. Micro-suites skip the shared queue** (18:50Z, 2026-10-02; standing; amends tb-R122; from cocoa's report).
- *Evidence:* cocoa's CR suite is a ~1-min smoke gate with no compile and under 1 GiB of disk. RUN-261002-78fbaf (C1 rev4) and RUN-261002-64f5e5 (K25 rev2) finished their work at about 05:30–07:00Z, then waited over 12 h at positions 4–5 in the shared validation queue behind multi-hour spm suites, and were cancelled at idle_deadline 720 ("validation suite cancelled while queued"). Their candidates are intact. tb-R122's short lane exists, but tb-R112's launch gates (45 GiB, idle 25%) also blocked such suites today.
- *Decision:*
  - A micro-suite = a validation suite with no compile step, a measured disk footprint under 1 GiB and a last run under 5 min. The owner posts the measured numbers once, at first use.
  - Micro-suites never use the shared validation queue. They run in tb-R122's short lane (skill-project-management-infra-exit/.temp/module-lane, max 1) without tb-R112's launch gates and outside the tb-R72 ceiling.
  - Limits: one at a time in the short lane; no start under 25 GiB free or at thermal state 2 or more; a run over 10 min falls back to tb-R122's normal rules.
  - Other short-lane suites keep tb-R122's gates.
  - First user: cocoa (republish 78fbaf and 64f5e5 into the short lane, cancel their shared tickets).
  - Follow-up, non-blocking: a task-board suite-class field so micro-suites route themselves, filed with the tb-R153 note that the queue should check the disk floor before granting.
- *Posted:* $gRYmvA4SXaGTv26uKc5Yi5XkOHEzBeuCKfWzHFLjtcA.
- *State:* in force (tb-R133 trial).

**tb-R159. Done review workspaces are released promptly** (23:28Z, 2026-10-02; standing; tb-R72, extends tb-R155 S4).
- *Evidence:* macos-sys's sampler (16:23–18:23Z) showed ivmbp's disk loss is a slow monotone grind, not the self-reversing spikes: -33.8 GiB unlogged, including -25.9 GiB in 57 min; go-build explains about a third. The rest matches new Story worktrees: 11 since 20:00Z at about 3 GiB each (the .task-board checkout alone is 0.9–2.3 GiB), mostly tb-R141 review-panel Stories. After tonight's go-build pass (56.8 GiB) free space fell to 48 GiB in 12 min. tb-muse's .temp holds 38 October Story dirs; infra-exit's .temp is 32 GiB.
- *Decision:*
  - Within 30 min of a panel's merged verdict, the owner releases each finished panel and delta Story workspace via task-board worktree abort --discard, after the usual check (clean worktree, no lease, empty-delta CR). Panel CRs stay untouched until the post-#499 sweep (S4). The GiB freed are posted.
  - Any root whose .temp exceeds 25 GiB runs a release pass of done or superseded workspaces without asking and posts the result. Live, open-CR and lease work is never touched.
  - Follow-up after the tb-R154 opt-ins: a sparse or partial .task-board in Story worktrees (infra-exit, tb-tests).
- *Posted:* $VxRmfK0cOC_p6RmN9DO-Y2g9LBElwl3NKTvCko6ZxQQ.
- *Correction (23:34Z, macos-sys measurement):* the evidence's "about 3 GiB each" was wrong; measured Story dirs range from 20 KB to 3.7 GiB (infra-exit average 1.07 GiB; 27 dirs = 28.8 of 33 GiB). tb-muse's 35 released Stories gave back only 1.85 GiB. The rule stands because it triggers on measured .temp size; reclaim is never estimated by dir count. (23:41Z: the "about 16 GiB/h" was a single burst window, withdrawn by macos-sys; infra-exit's tb-R159 pass then took .temp 33.44 → 20.40 GiB and free 49.3 → 59.9 GiB at 23:32–23:38Z. 22 of 33 Story dirs are empty husks left by releases; the space sits in young workspaces, so prompt release after the verdict matters and oldest-first sweeps reclaim little.)
- *State:* in force (tb-R133 trial).

**tb-R160. A skill-agents-management maintenance prerelease as a bridge for 1ycuhl's S1** (23:56Z, 2026-10-02; infra-exit's request; an exception inside the 13:53Z pin hold).
- *Evidence:* Gate B (TASK-260927-1ycuhl) round-2 finding S1: provider config sampled more than once, so A/B/A launches B while the manifest says A. spm cannot fix it cleanly: skill-agents-management's codex plugin (v0.5.31–v0.5.42) takes only a provider ID and parses config.toml itself; the clean fix is a module API taking one parsed provider snapshot + digest. spm pins v0.5.30, and every release after v0.5.40 requires the private curator-network-profiles (13:53Z hold: no consumer past v0.5.40; desk #34 open).
- *Decision:*
  1. The snapshot API lands on skill-agents-management main as usual (next minor; tb-R132, tb-R155 S3, module window).
  2. v0.5.41-0.maint.1 is cut from the exact v0.5.40 tree plus only that change. Evidence: diff --stat v0.5.40..maint; the same change as main (range-diff, or a delta review if the backport differs); full module CI green on the maint head; a signed tag; no curator-network-profiles requirement (go list -m all).
  3. A bridge, not a maintained branch: no further maint.N tags without tb-keeper. It ends when spm can pin a mainline release after Ivan's #34; the end condition goes in the tag message and in 1ycuhl's notes.
  4. 1ycuhl pins the maint tag in its own CR; its hosted CI must resolve it publicly (no private fetch).
  5. S2 (lease loss) proceeds now.
  - Fallback if 2–4 fail: S1 parks on #34 and 1ycuhl lands S2 and the rest, with S1 filed as its own BUG.
- *Posted:* $plBZj8SSOLdo67j33aRfa62TU9LNcDieBpCjwCd66aY.
- *State:* in force.


**tb-R161. A review carries over across a no-code base refresh** (05:15Z, 2026-10-03; standing; first case #408; critical-fixes' request).
- *Evidence:* #408's round-6 merged tb-R141 ACCEPT (3/3 panels) was on CR rev4 over base ae40f13d. The recording reviewer was refused at spawn (revision_base_superseded): main had changed 4 of the CR's paths (README, spawnruntime/changerequest.go, worktree/presence_shape_test.go, worktree/registry.go) during six rounds. The rev4 delta 3-way-applies onto main 27841c01 with 0 conflicts; each of the 4 files has a per-file delta with a patch-id identical to the accepted one; the other 29 files are byte-identical blobs.
- *Decision:* a tb-R141 merged ACCEPT on rev N carries over to rev N+1 (rev N refreshed onto a newer main, no code change) when all hold:
  1. Identity: the recording reviewer proves per-file patch-id identity of the CR delta and byte-identical blobs elsewhere, with the commands and output attached to the CR.
  2. Interaction: for every path main itself changed between the two bases, the recording reviewer reads main's base delta there and checks it touches nothing the CR relies on or changes. An interaction finding sends it to a delta-only round on rev N+1.
  3. Evidence: rev N+1's CR suite is green, and the refresh uses the safe refresh-candidate, not converge.
  4. The recording reviewer records ACCEPT citing this rule, or names the failed condition.
  - Any code change beyond the 3-way refresh means a normal round. This is the review-side twin of tb-R69's board-only junction for CI.
- *Posted:* $yJN0WcoXyuLaVikQW10NXr_RA6WLZBfCGOkUy6EXrPE.
- *Amendment (07:15Z, 2026-10-03; $KArDfDatkSl7OujF1UabtCDzTEo2f3UOofCP8B1tOKk):* condition 3 is also met when the refresh used converge (refresh-candidate does not admit a 'ready' revision; a tooling gap critical-fixes files) AND the resulting Story tree is proven identical to an independent 3-way apply of the accepted delta onto the new main; the recording reviewer recomputes that proof itself. #408 rev6 (tree 3dd3a6d5 on 27841c01) proceeds under it.
- *State:* in force (tb-R133 trial).

*Notes added to earlier tb- rulings (2026-10-03):*
- tb-R159, correction (08:36Z, addendum 08:41Z): "the rest matches new Story worktrees" was an inference and is withdrawn. macos-sys's 23:27–01:27Z run measured go-build as the largest consumer among the components it measured (+12.29 GiB). A worktree costs disk when it is created (about 2.28 GiB per new panel workspace). The tracker missed the spm main root, so the worktree total for that window is unknown, and about 30% of the drop stays unattributed. The rule stands: it fires on measured .temp size and on release after the verdict.
- tb-R160, clarification (10:27Z): skill-agents-management has no GitHub Actions, so condition 2's "full module CI" means the module's full local suite (make vet + make test) on a clean checkout of exactly the maint head, with the log and SHA attached to the consumer's CR. The hosted gate is condition 4.

**tb-R162. curator stays public: its history is cleaned and guarded** (11:06Z, 2026-10-03; Ivan's decision, without a GitHub Support request; supersedes tb-R156's end condition).
- *Decision:* the second operator's product documents, which had entered curator's public task board, were archived privately. curator main was rewritten once, from the first commit that carried them (base 5ce440c2, 119 commits) to the then tip, and every commit was re-signed. The remaining mentions in board files were neutralised. The two candidate-check branches that held old commits were deleted. Repo-local pre-commit and pre-push hooks guard against a repeat, and tooling follows in spm#511.
- *Verification:* ivan-curator's local check, then an independent check by relux-legal from a fresh mirror: 0 hits in HEAD, in main's history, and in every branch, tag and PR ref.
- *Result:* force-push dfaa557f→f17ea733 at 11:55:59Z, announced with IMPACT START/END. tb-R156 was lifted after the independent pass. Accepted residual: old commits stay fetchable by SHA until GitHub's own cleanup.
- *State:* done.

**tb-R163. Another operator's product material never becomes a resource in a public board** (11:06Z, 2026-10-03; standing).
- *Decision:* agents never put another operator's product material (briefs, requirements, reports, evidence, answers, or quotes of them) into a board resource, .research, README or progress file of a public repository. They store it privately and link to it with a neutral note; titles stay neutral. When unsure whether a repo is public, treat it as public. The tooling to enforce it is spm#511.
- *Scope (11:09Z):* "another operator" means a party outside Ivan's own fleet (a second operator, a client, a company). References between Ivan's own orchestrators are not covered.
- *State:* in force.

**tb-R164. Mac mini model rule: codex first, astra medium reviews, Claude and Muse as reserve** (12:04Z, 2026-10-03; Ivan, in macmini-infra's session).
- *Decision (Ivan-side orchestrators on the Mac mini; new spawns):*
  - Producers: codex gpt-6.1-sol, effort high.
  - Reviewers: codex gpt-6-astra, effort medium (amends tb-R138's low on the mini). Parallel fresh reviews are allowed, each noted "same-provider review (operator rule)".
  - Claude and Muse are reserve only, used on a codex refusal, failure, limit or outage, with the reason recorded.
  - Scope: the Mac mini. Whether it also applies on ivmbp is with Ivan.
- *State:* in force on the mini.

**tb-R165. Unpublished derived activity events that collide on sequence are dropped, not renumbered** (12:29Z, 2026-10-03; standing).
- *Decision:* an unpublished activity event that only a rollup produced, whose sequence number another root has meanwhile published, is dropped. It is never renumbered by hand and never edited. It is kept in a tagged stash until the source item's next publication re-derives it, after which the parent's rollup must match (a mismatch is a BUG). Source events are never dropped under this rule; a colliding source event is a BUG to its owner.
- *State:* in force (tb-R133 trial).

**tb-R166. Host-known reds** (12:30Z, 2026-10-03; standing).
- *Decision:* a CR's local suite counts as green on a host where clean main fails, when all four hold:
  1. A clean-main baseline at the CR's base SHA, on the same host and the same day, shows the same red tests with the same messages.
  2. The CR's run shows exactly that set of reds.
  3. The CR touches neither the red packages nor anything those tests exercise (fixtures, vendor tables, pins, registries).
  4. The host-known reds are a filed BUG with an owner.
  A red package that fails condition 3 must go green on another host before the merged verdict.
- *State:* in force (tb-R133 trial).

**tb-R167. Producer fan-out on a shared workstation** (16:55Z, 2026-10-03; standing).
- *Decision:* one root may run up to 3 concurrent producers when all of these hold:
  1. Free disk is at least 50 GiB at launch.
  2. Producers run only targeted tests and builds. CR suites stay in the shared queue.
  3. Under 40 GiB, no new producer launches until free disk is back at 45 GiB, and the youngest producer runs no local tests.
  4. One review-panel set at a time per root.
  5. Landings keep their standing order.
  The heavy-suite ceiling stays at 2 at most.
- *State:* in force (tb-R133 trial).

**tb-R168. A vendor integrity field that cannot be verified is non-blocking, under conditions** (16:57Z, 2026-10-03; standing).
- *Decision:* a review finding that a vendor-supplied integrity field (such as a content digest) is not verified is non-blocking when all of these hold:
  1. There is evidence that the algorithm cannot be derived from the pinned artifacts.
  2. The field is format-checked fail-closed and never treated as verified downstream.
  3. The covered content is validated independently and carries no state that the guarded decision relies on.
  4. A code comment names the residual risk. A follow-up BUG exists. An upstream question goes to the vendor's public tracker in neutral wording.
- *State:* in force (tb-R133 trial).

**tb-R169. A hosted red is diagnosed only from the attempt-specific complete job log** (22:01Z, 2026-10-03; standing).
- *Decision:*
  1. Any claim about why a hosted CI job failed must cite the complete log of that specific attempt (for GitHub: `gh api --allow-escape-sequences repos/<o>/<r>/actions/jobs/<job id>/logs`), with the failing test and its message. This covers claims used for a re-run, a queue position, a BUG link or a "waits for X" plan. A summary view or a copied log is not evidence; truncated views have misattributed failures.
  2. A decision that already rested on a truncated view is re-checked against the complete log. If the real cause lies outside the change, the one re-run spent on the wrong diagnosis is restored, on a named runner. If the cause lies in the change, it is a real red.
  3. Queue tickets and their order are unchanged.
- *State:* in force (tb-R133 trial).

**tb-R170. Review panels carry a review contract, not a research-plan contract** (23:34Z, 2026-10-03; standing; amends tb-R141 point 1).
- *Decision:* a non-recording review panel is a review run. A research contract's "one decision, exit when answered" does not apply to it.
  - The brief carries a surface table: entry points × positions × forms, or the catalog rows that apply. The panel marks every row as held, finding or not walked.
  - After a confirmed blocking reproduction, the panel records it and continues to the remaining rows.
  - The budget is per row. At the marker the panel writes rows_not_walked. A verdict with unwalked rows is never accept.
  - Briefs state each invariant's intent and its allowed cases, not only its literal form.
- *Measured:* on a frozen candidate with four known defects, two runs per arm: the research shape found 2/4 and 4/4; the review contract found 4/4 in both runs.
- *State:* in force (tb-R133 trial).

**tb-R171. A CI runner pause on a loaded workstation** (23:44Z, 2026-10-03). The runner was paused after three cumulative-timeout deaths of one head under local load. The pause is CLOSED: the operator re-enabled the runner. Lesson: tell the operator plainly at the moment of any runner change.

**tb-R172. A union of disjoint appended rows in a test-only table counts as a mechanical refresh** (01:07Z, 2026-10-04; standing; amends tb-R161 condition 1).
- *Decision:* this applies when all of these hold:
  1. The conflicted file is test-only, and both sides only append disjoint entries to the same table.
  2. The resolved file's diff against the new base equals exactly the change's own added lines, and every other file keeps per-file patch-id identity.
  3. The full suite passes on the exact landing head, and the recorder cites tb-R161 and tb-R172.
- *State:* in force (tb-R133 trial).

**tb-R173. A publication guard's blocking scope is every publication path** (01:07Z, 2026-10-04; scope ruling for one change).
- *Decision:*
  - Blocking: any push of any ref to any remote, plus every landing on a protected main.
  - Each publication point scans the whole unpublished range.
  - Internal local commits are a documented bound with a follow-up.
  - "No commit object" reads as no reachable object and no ref move.

**tb-R174. No spawn uses a reduced ("lite") agent-context profile** (02:36Z, 2026-10-04; standing). The reduced profile drops the role's normative sections, including the reviewer's no-early-exit contract. Configs use the full profile; a lite review is invalid evidence. Code removal is tracked separately.

**tb-R176. Tools install from a release tree that excludes the board; no NUL-bearing files in tracked board resources** (03:26Z, 2026-10-04; standing).
- *Decision:*
  - A first-party tool whose repository carries a board is installed from a release ref or artifact without the board directory. The board is data, not product.
  - New board evidence is plain text. An archive stays outside git, with its path, sha256 and size recorded in a text resource.
  - A validate check refuses new NUL files in board resources. There is no mass move of historical archives.
- *State:* in force (tb-R133 trial).

**tb-R177. Both founders' explicit OK before any OSS publication** (06:02Z, 2026-10-04; standing).
- *Decision:*
  - No repository is made public, and no first public tag is pushed, without both founders' explicit OK, asked before the switch.
  - The arbiter's publication gate re-reads the original prerequisites before any PASS.
- *State:* in force.

**tb-R178. Dependency bumps with unrelated behaviour changes ride in their own bump change; no context-budget raise inside a security fix** (10:23Z, 2026-10-04; standing).
- *Decision:*
  - A dependent change rebases on the bump, which lands as a documented migration.
  - A gate or budget raise is always its own reviewed change.
- *State:* in force (tb-R133 trial).

**tb-R179. A model-budget throttle until the weekly reset** (12:02Z, 2026-10-04). The burn was projected to hit the stop line six days early. Until the reset:
- research and new implementation run at medium effort;
- high effort only for security or critical reviews and critical-fix producers;
- no new research fan-out;
- no producers on a host with exec stalls;
- the reserve providers are used where the rotation rules allow.

**tb-R180. A docs-only relocation refresh keeps the suite evidence** (12:25Z, 2026-10-04; standing).
- *Decision:* a change made stale only because the base relocated documentation it touches keeps its queued suite when all of these hold:
  1. Its doc hunks are moved verbatim.
  2. Outside the relocated doc files, the trees are byte-identical.
  3. Every test that reads the changed doc files re-runs green on the landing revision, as a micro-suite.
  4. The recorder cites the rule with the proof.
- *State:* in force (tb-R133 trial).

**tb-R181. Codex budgets per subscription** (14:03Z, 2026-10-04; operator decision).
- *Decision:* each Codex account follows its own rules. The ivmbp account keeps its prior pace and stop line. The Mac mini account was used at full pace for a bounded window (it closed at 22:30Z), within the mini's run cap.
- *State:* the mini window closed; the per-account principle stays.

**tb-R182. A stale-tree check after every refresh** (19:45Z, 2026-10-04; standing).
- *Decision:* after any refresh-candidate or converge, and before any handoff, recorder run or CR snapshot, `git diff --name-only HEAD` must list only the candidate's own paths, and check-no-revert must be clean against the current trunk. A failure stops the handoff. A refresh that silently leaves stale tracked paths is a critical bug (a silent trunk revert).
- *State:* in force.

**tb-R184. No `spawn wait` / `spawn --wait` until the busy-spin bug is fixed** (20:40Z, 2026-10-04; standing interim).
- *Decision:* waiters busy-spun at up to 90% of a core and tripped the CPU guard. Poll `spawn status` every few minutes, or wait for the completion notice. The rule lifts when the fix lands with a regression test.
- *State:* in force.

**tb-R185. Storage hygiene by design** (20:49Z, 2026-10-04; operator decision).
- *Decision:* automatic retention for Story workspaces and spawn-run logs, a content-addressed evidence store outside git, and research on reviving the remote board as cloud storage.
- *State:* in force.

**tb-R186. Disk recovery: archive before deletion, Story worktrees without the board, stale Stories closed through the board** (01:03Z, 2026-10-05; operator decision).
- *Decision:*
  1. Logs are archived before any deletion.
  2. Managed Story worktrees exclude the git-tracked `.task-board/`, which is most of their roughly 0.9 GiB.
  3. Stale live Stories are closed with `worktree abort`, finished, or closed-landed, never removed with `rm`.
- *State:* in force.

**tb-R187. Models after a budget reset: Muse writes code, sol medium reviews** (02:05Z, 2026-10-05; operator rule for the Mac mini).
- *Decision:* producers use muse-spark-1.3-contributor at max effort; reviewers use gpt-6.1-sol at medium. If Muse fails, the fallback producer is sol medium or claude-sonnet medium/high; when the fallback producer is codex, the reviews move to Claude (cross-provider rule).
- *State:* in force on the Mac mini.

**tb-R188. The session-host extraction is built in a private fork** (02:24Z, 2026-10-05; operator direction).
- *Decision:* the hybrid extraction happens in a PRIVATE relux-works fork that syncs main at least daily. Accepted, inert foundations land on live main first. Merging back is its own fully gated change. Module map: agent-session-host (supervisor, registry, park and restart, harness adapters), agent-session-bridge (providers plus a trust-gate front), and trust primitives in curator-trust plus mandates.
- *State:* in force.

**tb-R189. A test-only CR crosses a code junction with a package-scoped exact-head run** (02:42Z, 2026-10-05; standing).
- *Decision:* a CR whose delta is only `_test.go` files and fixtures may land on a newer main without a fresh full hosted CI when all of these hold:
  1. A full exact-head CI was green on the previous base.
  2. Its patch-ids are identical after the rebase.
  3. Main's interacting delta has been read.
  4. If main changed a package the CR touches, `go vet` plus `go test` of those packages ran at the exact new head.
- *State:* in force.

**tb-R190. Only new module repos may go public** (02:43Z, 2026-10-05; operator direction, amended).
- *Decision:* the private fork never goes public. A new module repo may go public only when all of these hold:
  - it has a clean history (no session links, personal data, credentials or NUL resources);
  - `.task-board/` is in `.gitignore` from the first commit;
  - its CI runs on GitHub-hosted runners, or the repo joins the self-hosted groups only together with mandatory approval of external fork-PR workflows and no secrets for fork runs;
  - both founders gave their OK (tb-R177).
- *State:* in force. The first such repo (agent-session-bridge) went public on 2026-10-05. The operator's proposal that one founder's OK should suffice is PENDING the other operator's agreement, so tb-R177 stands.

**tb-R191. An architecture review package comes before any extraction code** (02:49Z, 2026-10-05; operator direction).
- *Decision:* the module repos are prepared as an architecture review package: overview, module map, trust-gate pipeline, injection and output paths, key lifecycle, open questions and diagrams, for both founders. No extraction code lands before the operator's separate go. The session-core study of comparable systems goes to the wiki, with its adopted ideas traced into the architecture.
- *State:* in force.

**tb-R192. A CR crosses a test-only advance of main with a package-scoped exact-head run** (04:14Z, 2026-10-05; standing; the mirror of tb-R189).
- *Decision:* a CR may land on a main that advanced only by test-only changes (`_test.go` files and fixtures) when all of these hold:
  1. its full exact-head CI was green on the previous base;
  2. its per-file patch-ids are identical after the rebase;
  3. the advance's files are disjoint from the CR's files;
  4. `go vet` plus `go test -count=1` of every package the advance touched ran at the exact rebased head.
- Any non-test advance means a fresh full CI.
- *State:* in force.

**tb-R193. Every Mac mini build goes through the FIFO build lock** (04:33Z, 2026-10-05; standing).
- *Decision:* every go build or test on the Mac mini runs through one FIFO ticket lock with dead-owner detection (a pid plus its start time). Hand-rolled spinner wrappers are retired.
- *State:* in force.

**tb-R194. The mini's syspolicyd trigger, part 2: test-created executables** (05:02Z, 2026-10-05).
- *Decision:* suites that create and delete fake executables run on hosted CI or on the operator's workstation, not on the Mac mini. A default-off test-support switch keeps such executables until a reaper clears them. Success measure: syspolicyd's 24 h crash rate. It went from 2.4/h to 0 after this rule.
- *State:* in force.

**tb-R195. Review difficulty beats the reviewer pin; model routing runs in shadow for a week** (05:10Z, 2026-10-05; operator decisions).
- *Decision:* hard, critical and delicate reviews get a qualified strong reviewer whatever the pinned reviewer is. The model router wraps spawns in shadow mode for one week, launching exactly the caller's arguments, before any switch to select mode.
- *State:* in force (amended on the operator's workstation by tb-R209).

**tb-R196. One machine-scoped validation ceiling** (10:25Z, 2026-10-05).
- *Decision:* the validation queue's machine ceiling is read from one machine-scoped file that the arbiter writes. A per-root value may only lower it for that root's own tickets.
- *State:* in force.

**tb-R197. A CR crosses a disjoint code advance with a guard-and-vet run at the exact head** (10:50Z, 2026-10-05; standing).
- *Decision:* a CR may land on a main that advanced by production code when all of these hold:
  1. its full exact-head CI was green on the previous base;
  2. its patch-ids are identical after the rebase;
  3. `go list -deps` shows the two sides are disjoint in both directions;
  4. at the exact rebased head, every repo-wide guard step runs, plus `go vet` and `go test -count=1` of each package the CR changes;
  5. a signed exact-head fast-forward.
- *State:* in force.

**tb-R198. No new test debt in skill-project-management** (14:02Z, 2026-10-05; the second operator's request, the operator's nod).
- *Decision:* new or changed tests use no waits (fake clocks and deterministic seams instead), no literal mutant tables in the PR suite, and isolation by default (TempDir only, no fixed ports, no shared paths). There are no new slow serial tests without a stated reason. A mechanical ciguard check follows. A fixed port means one that is bound, listened on or dialled; an inert URL literal is allowed.
- *State:* in force.

**tb-R199. Mutation tests leave PR CI for a trunk lane with a mechanical redline** (14:05Z, 2026-10-05; both operators).
- *Decision:* fixed mutant tables leave the default PR suite. Security guards, diff-scoped meta guards and diff-triggered mutants stay per PR. A trunk mutant lane blocks landings while it is red (except the fix and board-only records) and gates tags. The CI speed-up W1–W7 runs as one epic.
- *State:* in force.

**tb-R200. Our own session core; herdr's vendor contract is supported, not implemented from herdr** (14:21Z, 2026-10-05; the operator's decision).
- *Decision:* no herdr implementation is adopted. The session host supports herdr's agent-to-runtime state-report contract. Three invariants hold: verified input, no process writes into a session through state reports, and swarm scale. For the joint platform it is the operator's position, not a joint decision.
- *State:* in force.

**tb-R201. When Codex refuses a review, another provider reviews; review prompts get recalibrated** (15:42Z, 2026-10-05; both operators' take).
- *Decision:* a refused or derailed Codex review is re-run on another provider. Each refusal is logged. Review prompts are reframed defensively with the same substance (owners: the second operator's spm keeper plus tb-tests, per tb-R206).
- *State:* in force; on the operator's workstation the fallback is Muse (tb-R209).

**tb-R202. A time-boxed bridge: CLI test timeout 90m → 135m until sharding is wired** (17:39Z, 2026-10-05).
- *Decision:* one workflow change raises the CLI test timeout so the sharding fix can pass its own CI. The bridge expires when the sharding wiring lands.
- *State:* expired (W1 rev2 landed 22:09Z, 2026-10-06).

**tb-R203. Test redline on skill-project-management main until the sharded CI is wired** (20:13Z, 2026-10-05; the second operator's proposal, imposed as an arbiter landing hold).
- *Decision:* only the timeout bridge, the sharding CRs, test-only hygiene, critical fixes and board-only records land. Everything else parks, with no re-runs. The release condition is simplified by tb-R208.
- *State:* RELEASED (22:08Z, 2026-10-06; W1 rev2 green and landed).

**tb-R204. Generated maps are derived files** (04:34Z, 2026-10-06; standing; a sibling of tb-R197).
- *Decision:* a CR crosses an advance whose only overlap is a generated owner or shard map when:
  - the map is regenerated by its tool at the exact head, and its diff touches only the CR's entries;
  - all other patch-ids are identical;
  - tb-R197's checks pass, plus the maps' own guards.
- *Amendments:* an edge that exists only through test imports passes when both ends' tests run at the exact head. A same-package overlap passes only with disjoint files, a leaf package and its full tests at the exact head.
- *State:* in force.

**tb-R205. No deliberate CPU saturation on shared hosts** (07:33Z, 2026-10-06; standing).
- *Decision:* no load generator may take a host's CPU idle below 25%. A stress test runs at most 4 workers, attached to the test's own process (never disowned), at most 10 min per pass, and is named in the brief. A full-saturation repro goes to a hosted runner. The arbiter stops violators behind an impact notice.
- *State:* in force.

**tb-R206. Codex unblocking** (08:18Z, 2026-10-06; the second operator's request).
- *Decision:*
  - the prompt recalibration of tb-R201 is led by the second operator's spm keeper, with tb-tests as co-owner;
  - the Codex goal-recovery 409 bug is P1 in the critical lane;
  - every new guard states its per-PR cost, and a repo-wide guard over about 30 s needs a stated reason.
- *State:* in force.

**tb-R207. The operator workstation's task-board install is unblocked from two sides** (10:19Z, 2026-10-06).
- *Decision:* curator rc.3 refuses any snapshot with a NUL byte.
  1. skill-agents-management stores its zone data as NUL-free text with a pure loader (v0.5.54 and v0.5.41-0.maint.2), and spm pins it.
  2. curator makes the NUL gate v1-only.
  No install with a pre-rc3 binary.
- *State:* in force.

**tb-R208. The sharding fix W1 rev2 gets a private lane, a reserved window and a simpler release condition** (10:42Z, 2026-10-06).
- *Decision:* W1 rev2 runs one suite in a private lane under the host gates. Main is reserved for it from green validation to landing; critical landings inside the window need its owner's agreement. Its own green exact-head full CI plus an exact-head landing releases tb-R203.
- *State:* RELEASED (22:08Z, 2026-10-06).

**tb-R209. Models on the operator's workstation: parallel reviews on gpt-6.1-sol high plus Muse max; producers on sol medium or Muse max** (11:37Z, 2026-10-06; the operator's rule, to save the Claude Code subscription's limits).
- *Decision:* for new spawns on that host only:
  - every review round has two parallel reviewers (sol high and Muse max); delta and recording reviewers are cross-provider from the producer;
  - producers run sol medium or Muse max;
  - Claude leaves the producer and reviewer roles there.
  Running runs continue. The Mac mini keeps its own rules.
- *State:* in force.

**tb-R210. The model-router module may go public: one founder's OK given; the other founder's OK and the creation go pending** (23:47Z, 2026-10-06; the operator, as the module's maintainer).
- *Decision:* the operator gives his founder OK. tb-R177 still needs the other founder's explicit OK before any flip. The tb-R190 preflight (clean history in every commit, a licence, no board directory, hosted-only CI, no self-hosted runner group) passed on the candidate. The operator confirmed Apache-2.0 plus a NOTICE, and no session trailers in future public commits. The flip itself needs his go for the creation steps.
- *State:* in force; publication pending the other founder's OK.

**tb-R211. One spm CI and test owner** (00:27Z, 2026-10-07; the operator's decision).
- *Decision:* tb-tests hands its work to validation-fixes through a handover packet. On the ack, every ruling role naming tb-tests (CI owner, shard manifests, the CI workflow, test fixes, recalibration co-owner, stress-run policy, the critical-lane fixes in flight) passes to validation-fixes. Ruling texts are not rewritten.
- *State:* in force.

**tb-R212. Operator workstation binary swap: curator rc.4 and a task-board build with the nested-repository refresh fix** (01:03Z, 2026-10-07; under the standing swap go; completes tb-R207 path 2).
- *Decision:* curator v0.15.0-rc.4 (checksums and attestation verified) and a new task-board build were installed behind impact notices. A symbol probe showed the fix present in the new build and absent in the old one. Nothing was restarted; the old build stays on disk. Rollback: the kept rc.3 binary and an APFS clone of the state.
- *State:* done.

**tb-R213. A generated map does not make a CR's review or suite evidence stale** (09:24Z, 2026-10-07; standing; a sibling of tb-R204 for the review stage).
- *Decision:* when a CR is re-materialised onto a newer main and its only path overlap with the advance is a generated map (owner or shard map), the map is regenerated on the new base. If every other per-file patch-id is identical, the previous revision's local suite and reviews carry to the new one; the next reviewer may start at once. Landing evidence is unchanged (a step 5 class, or a fresh full hosted exact-head CI). Tool fix: the base-superseded check and converge treat generated maps as derived.
- *State:* in force.

**tb-R214. The mutant cut executes tb-R199 point 1 now** (09:49Z, 2026-10-07; the second operator's owner decision, within tb-R199).
- *Decision:* mutant tests move behind the `mutants` build tag, except the tb-R199 point 2 carve-out (security and trust guard mutants, diff-scoped meta guards), which the cut lists by name. No-loss evidence at the exact head: a test-name inventory (every test runs exactly once across untagged plus tagged), one full tagged run green, and a fresh full hosted CI. Until the remote-gate wiring lands, a red trunk mutant run blocks landing windows by ruling, except the fix and board-only records. The CI owner reviews.
- *State:* in force.

**tb-R215. The test-roadmap queue: other CRs yield to the speed-up lane; one integration lands as one fast-forward** (10:00Z, 2026-10-07; on the second operator's goal to land the reworked test roadmap fast).
- *Decision:* window order on that main is critical fixes, then the mutant cut and the shard soft threshold (one writer at a time on the CI suite workflow), then the speed-up integration, then everything else. A ready non-critical CR lands only in a gap; it is not frozen. The integration keeps each CR's accepted review with identical patch-ids, re-reviews any conflict resolution, proves its tip with one fresh full hosted CI plus 3 stabilisation runs on the same head, and lands as ONE signed exact-head fast-forward in a reserved window (4 h cap).
- *State:* in force until the speed-up integration lands.

**tb-R216. The operator approves PR #50 and designates his decision channels** (11:48Z, 2026-10-07; the operator, verbatim «Да да», through his desk channel).
- *Decision:* YES to PR #50 at 7b30ec9. The operator's designated decision channels are his desk cards (tb-R66) and the dispatcher session. A decision relayed through either counts as his; this ruling is the record of the designation from the operator.
- *State:* in force.
