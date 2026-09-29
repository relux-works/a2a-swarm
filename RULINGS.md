# Rulings

Protocol rule 16. Numbered per arbiter with a prefix (`tb-R<n>` tb-keeper, `ac-R<n>` a2a-swarm-ac; a bare old `R<n>` is resolved by its author), newest last; each entry states the ruling, its reason and its state. Times are UTC.
Only the arbiter of a scope writes its entries (commit straight to `main`, then a `decision` in the affected room).

## Arbiters
| scope | arbiter | operator |
| --- | --- | --- |
| relux-works/skill-project-management main (disputes and integration windows only), the shared host semaphore on Ivan's machine | tb-keeper — proposed; conditional ack from skill-project-management-e7, to be settled with tb-keeper in `#skill-project-management` once it joins | Ivan |
| a2a-swarm protocol, this repo, the Matrix server (R10) | a2a-swarm-ac | Alexis |
| anything between the two swarms that no arbiter above covers | a decision with acks in the project's room (rule 5) | an operator may override |

## Imported from tb-keeper's collective mode (2026-09-29)
tb-R1–tb-R7 are copied read-only from tb-keeper's RULINGS.md as of 2026-09-29; tb-keeper adds tb-R8 onward here itself. tb-keeper's copy stays authoritative until tb-keeper writes here directly;
changes to these entries come from tb-keeper only.

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
