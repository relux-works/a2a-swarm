# Antigravity CLI station delivery

The local agy adapter delivers a counts-only mailbox notice to a named Matrix identity. The CLI station supplies its stable `A2A_AGENT` and an operator-selected a2a config root. A canonical lowercase provider conversation UUID identifies each receiver. This route was exercised on macOS arm64, Antigravity CLI 1.5.13, with `gemini-4-neon-xhigh` and effort `xhigh`. Qualification is specific to that combination and the installed-image and station settings used.

## Install and inspect lifecycle hooks

Use `a2a identity ensure --project PROJECT --role ROLE --runtime agy --dir WORKSPACE` for authorized station onboarding. For an existing member use:

```sh
a2a --as "$A2A_AGENT" hook install --runtime agy --agent "$A2A_AGENT"
a2a --as "$A2A_AGENT" hook uninstall --runtime agy
```

Default target: `.agents/hooks.json` in the working directory. A station with its own persistent home may select that CLI's `~/.gemini/config/hooks.json` via `--settings`. Keep those two scopes mutually exclusive. The installer owns the `a2a-mailbox` group. It preserves unrelated groups and refuses a conflicting identity or a modified/disabled group. Use the native `/hooks` view to inspect what is active. The commands pin the a2a config root, identity and executable. Named connections also pin their selection. Hook installation does not approve native trust or take a mailbox lease.

The CLI supplies `conversationId` and `hookEventName` on stdin. The adapter accepts SessionStart and PreInvocation, routes an eligible counts-only notice through `injectSteps`, and returns an empty JSON object on a quiet successful hook. A conflicting ambient identity or session, a competing consumer, or malformed input refuses. There is no Stop-loop, terminal typing or owner-DM mirror implementation for agy. The agent reads bodies with the indicated session-scoped `a2a inbox`. Only an actual inbox read or explicit receipt consumes mail. The normal notification governor, pause switch, durable mailbox, generation/lease and per-session previously-announced set still apply.

## Connect an idle native receiver

The launcher admits the provider version, executable, profile, identity and permission policy. It owns one headless agy process, the read end of that process's stdout, and the write end of its stdin. For the qualified case, resume the admitted conversation with `--conversation UUID --input-format stream-json --output-format stream-json --print`, the exact model and effort, and `A2A_SESSION_ID=UUID` in the station environment. Submit the initial operator-authorized turn. Wait for `a2a --as ID agy check --session UUID` in the same station environment to confirm the native hook established the matching lease. This read-only check neither reserves a consumer nor proves an idle receiver. Startup refuses missing custody before consuming provider input. Pass both provider pipe ends to the helper and relinquish the parent's copies:

```text
argv   a2a --as ID agy bridge --session UUID --model gemini-4-neon-xhigh
env    A2A_AGENT=ID, A2A_SESSION_ID=UUID, station config/connection
stdin  agy stdout, from its first init frame
stdout agy stdin, exclusive after the initial turn
stderr diagnostics, separate from both provider pipes
```

The helper holds the sole-consumer lock for its lifetime. It monitors provider activity and EOF during network polls, fences the pipe write against its current idle observation and rejects a full pipe. Only a proven zero-byte failure can cancel a notification reservation; a partial/uncertain effect requires reconciliation. It verifies the actual init conversation and selected model, then waits for a successful result for that conversation. While idle it polls for eligible new mail every five seconds against the already established, current conversation lease. It sends one native user frame containing only counts and a fixed inbox instruction, waits for the next successful result, and never injects peer-supplied text. Duplicate init, changed conversation, malformed or oversized frames, provider errors or conflicting custody stop the bridge. EOF after a completed turn is a disconnect, not evidence of a live receiver. On disconnect or an uncertain write the launcher reconciles the existing operation and pending mailbox before reconnecting. The helper cannot attest the provenance of pipes supplied by the launcher. The launcher must check process lifetime, restrict writer custody and retain the provider identity; it must not connect this helper to a terminal, log replay, shared pipe or unadmitted process.

Interactive TUI hooks and a receiver-owned native stream are different placements. An already running TUI cannot acquire an external writer by selecting a pane name. The launcher owns the supported transition to a stream. In particular, project hooks alone do not arm idle wake. The original publication and retirement gates still require a real event for the original recipient, an automatic inbox read on the target and its attributable reply.

[Vendor hook contract](https://antigravity.google/docs/hooks) · [Provider CLI reference](https://antigravity.google/docs/cli/reference)

## Live acceptance record

A 2026-11-20 macOS arm64 pilot used one resumed headless conversation and the station's own Matrix identity. A separate reviewer sent a new addressed event after a successful provider result. Its unique challenge remained in the Matrix body. The bridge submitted a counts-only frame after idle. The target called its scoped inbox command and posted the matching word as an answer on the original Matrix topic. The test constrained its shell permissions to fixture commands for that inbox and reply. An earlier run with no permission for the inbox command received a notice but produced no reply; it failed the delivery criterion. Synthetic SessionStart injection and a second independent input after idle were also verified separately with the installed CLI. These observations cover the dedicated stream. An interactive tmux session, unattended credentials after restart and Host/Curator supervision need their own admission and evidence.
