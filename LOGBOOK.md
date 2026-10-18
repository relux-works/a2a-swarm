# Flight Logbook

> Institutional memory. Concise, factual, high-signal.
> Newest entries first. One block per insight.

## 2026-10-19

### 0209 — Synthetic process startup consumes fixture deadlines
- FINDING: Clean main `6f3c8749` fails the uncached race suite at the positive cross-process cooldown control. The failure predates PR #66; targeted repeats alone can pass.
- FINDING: Eight fresh private shell scripts take 380–463 ms on first direct exec; repeat exec and explicit `/bin/sh script` take 4–6 ms on this macOS host. This establishes a fixture startup cost, not its OS-internal cause or a proof of every historical timeout.
- FIX: `internal/cli/keychain_probe_test.go`, `keychain_process_test.go` and `keychain_test.go` run valid synthetic scripts through `/bin/sh`; missing/non-executable controls retain direct exec. Production deadlines, process groups, pipe checks and cooldown behavior are unchanged.
- STATUS: BUG-261019-enfz95 — stabilize-synthetic-process-startup-witnesses. Narrow repeated controls, deliberate cleanup/pipe violations, full race and hosted checks must pass before acceptance. No real Keychain/helper operation is part of this change.
