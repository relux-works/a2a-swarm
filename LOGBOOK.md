# Flight Logbook

> Institutional memory. Concise, factual, high-signal.
> Newest entries first. One block per insight.

## 2026-10-19

### 0845 — Old executable images can erase a newer binding schema
- FINDING: The first Claude proof lost `State.terminal` and its consumer exited silently. The owner identified its pre-upgrade Monitor waiter: bind ran before that old waiter stopped. An old schema rewrites state without fields it cannot decode; a later process snapshot cannot exclude an already-exited writer.
- FIX: Bind and SessionStart reject a same-identity/config-root a2a process with a different loaded executable vnode, and both directions reject parallel terminal binding plus wait/watch. Handled consumer exits report fixed reasons; raw argv/env and screens never enter the log. The admission snapshot cannot prevent a subsequently started old binary; rollout must stop old consumers first.
- CONTRACT: SessionStart discovers the own-UID runtime ancestor rather than the hook shell, binds automatically with an explicit bounded policy, and preserves an existing live generation through resume/compact/clear. An explicit unbind stays stopped; absence of a qualified tmux/runtime target diagnoses unavailable without changing trust or the mailbox.
- EVIDENCE: TASK-261019-srqhz0 — finish-automatic-session-wake; private terminal controls cover both runtimes, repeat hooks, typed old-image refusal, reverse waiter refusal, exit reasons and the Typed/empty hourly-limit regression. Real Claude proof03 passed separately at installed predecessor895abc6; candidate auto-bind requires its own reviewed delivery and separately coordinated adoption.

### 0209 — Synthetic process startup consumes fixture deadlines
- FINDING: Clean main `6f3c8749` fails the uncached race suite at the positive cross-process cooldown control. The failure predates PR #66; targeted repeats alone can pass.
- FINDING: Eight fresh private shell scripts take 380–463 ms on first direct exec; repeat exec and explicit `/bin/sh script` take 4–6 ms on this macOS host. This establishes a fixture startup cost, not its OS-internal cause or a proof of every historical timeout.
- FIX: `internal/cli/keychain_probe_test.go`, `keychain_process_test.go` and `keychain_test.go` run valid synthetic scripts through `/bin/sh`; missing/non-executable controls retain direct exec. Production deadlines, process groups, pipe checks and cooldown behavior are unchanged.
- STATUS: BUG-261019-enfz95 — stabilize-synthetic-process-startup-witnesses. Narrow repeated controls, deliberate cleanup/pipe violations, full race and hosted checks must pass before acceptance. No real Keychain/helper operation is part of this change.
