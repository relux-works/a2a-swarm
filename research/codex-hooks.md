# Codex CLI hooks: what `a2a hook install --runtime codex` must write

Source: openai/codex at tag `rust-v0.155.1` (matches the installed codex-cli 0.155.1). Paths are under `codex-rs/`.

## (a) Where hooks are declared
- Per config layer, Codex reads `hooks.json` in the layer's folder and the `[hooks]` table of that layer's `config.toml`; both load, with a
  warning when a layer uses both (hooks/src/engine/discovery.rs:129-186, 339-380, 382-400).
- Layer folders (config/src/state.rs:219-243): user = `$CODEX_HOME` (`~/.codex/hooks.json`); project = every `.codex/` from the project
  root down to the cwd (`<dir>/.codex/hooks.json`, config/src/loader/mod.rs:1645-1760). A project layer is loaded **disabled** when the
  project is not trusted (loader/mod.rs:121-123, 1685-1686), so its hooks do not run. In a linked git worktree the hooks of a project layer
  come from the main checkout's matching `.codex/` (loader/mod.rs:1131-1141).
- `hooks.json` schema (config/src/hook_config.rs:10-17, 35-61, 153-201): top level `{"description"?, "hooks": {...}}` with
  `deny_unknown_fields` (any other top-level key makes Codex reject the whole file, discovery.rs:359-367). `hooks` maps
  `SessionStart|UserPromptSubmit|Stop|PreToolUse|...` to arrays of matcher groups `{"matcher"?, "hooks": [handler]}`; a handler is
  `{"type":"command","command":S,"commandWindows"?,"timeout"? (s, default 600),"async"?,"statusMessage"?,"additionalContextLimit"?}`.
- There is **no env field** for hooks: only plugin hooks get extra env (discovery.rs:567-576). So the identity travels as `--as NAME`.
- The command runs through `$SHELL -lc` (fallback `/bin/sh`), cwd = session cwd, JSON on stdin (engine/command_runner.rs:217-220, 435-461).

## (b) Events and stdin payload (hooks/src/schema.rs)
- `Stop` (588-601): `session_id, turn_id, transcript_path, cwd, hook_event_name:"Stop", model, permission_mode, stop_hook_active,
  last_assistant_message`.
- `SessionStart` (499-510): `session_id, transcript_path, cwd, hook_event_name, model, permission_mode, source`.
- `UserPromptSubmit` (567-583): `session_id, turn_id, [agent_id, agent_type], transcript_path, cwd, hook_event_name, model, permission_mode, prompt`.
- The fields a2a uses (`session_id`, `stop_hook_active`) have the Claude Code names and meaning. `stop_hook_active` starts false per turn
  and becomes true after a blocking Stop continued it (core/src/session/turn.rs:407, 644-678).
- Other events: PreToolUse, PermissionRequest, PostToolUse, Pre/PostCompact, SessionEnd, SubagentStart/Stop, Interrupt (hook_config.rs:35-61).

## (c) Output contract
- Stop (schema.rs:451-464, engine/output_parser.rs:290-330, events/stop.rs:277-342): exit 0 with
  `{"decision":"block","reason":R}` blocks the stop; R (trimmed, non-empty) is recorded as a hook prompt and the turn continues
  (turn.rs:661-678). Alternative: exit 2 with R on stderr (stop.rs:343-369). Empty stdout = no effect.
- SessionStart (schema.rs:385-403, output_parser.rs:93-100) and UserPromptSubmit (schema.rs:427-449, output_parser.rs:266-288):
  `{"hookSpecificOutput":{"hookEventName":"SessionStart"|"UserPromptSubmit","additionalContext":R}}`; R is recorded in the conversation as hook
  context (core/src/hook_runtime.rs:833-857). Plain non-JSON stdout also becomes context (events/session_start.rs:222, 300-311).
- All output structs are `deny_unknown_fields` (plus universal `continue, stopReason, suppressOutput, systemMessage`, schema.rs:87-99):
  JSON with an unknown field fails the hook. These shapes are byte-for-byte the ones a2a already emits for Claude Code.
- additionalContext above ~2,500 tokens is spilled to a file with a preview (hooks/src/output_spill.rs:12); a2a's 20-record bound stays below.

## (d) Trust
- A non-managed hook runs only when `enabled` and its trust status is `Trusted` (discovery.rs:664-734): the user layer must hold
  `[hooks.state."<key>"] trusted_hash = "<hash>"` (hook_config.rs:19-33; read only from the user config.toml and session `-c` flags,
  hooks/src/config_rules.rs:15-66). `<key>` = `<abs path of hooks.json>:<stop|session_start|user_prompt_submit>:<group index>:<handler index>`
  (hooks/src/lib.rs:95-123); `<hash>` hashes the normalized handler incl. the command (discovery.rs:766-792), so a changed command or a
  moved group is `Modified`/`Untrusted` again (discovery.rs:794-811).
- The trust is written by Codex's TUI: at startup "Hooks need review" offers "Trust all and continue" (tui/src/startup_hooks_review.rs:228-251),
  which writes `hooks.state.<key>.trusted_hash` via config/batchWrite (tui/src/hooks_rpc.rs:58-92); `/hooks` reviews them later
  (tui/src/slash_command.rs:112). The screen says "Hooks can run outside the sandbox after you trust them."
- The only non-interactive switch is the launch flag `--dangerously-bypass-hook-trust` ("DANGEROUS. Intended only for automation that
  already vets hook sources", utils/cli/src/shared_options.rs:61-64), chosen by whoever starts Codex.
- Conclusion: trusting is a human approval. a2a does not forge it (computing the hash and writing `trusted_hash` would be the agent approving
  its own out-of-sandbox commands). `install` prints the human step: start `codex` in the project, trust the project if asked, and choose
  "Trust all and continue" (or `/hooks`); repeat after the command changes (a moved binary). A project `.codex/` also needs the project trusted.

## (e) Feature flag
- `hooks` (Feature::CodexHooks, "Claude-style lifecycle hooks loaded from hooks.json files") is `Stage::Stable`, `default_enabled: true`
  (features/src/lib.rs:105-106, 1170-1175). Nothing to enable unless a config sets `[features] hooks = false`.

## What a2a writes
Default `./.codex/hooks.json` (project layer, like Claude Code's project-local settings), merged idempotently, one group per event:
`{"hooks":[{"type":"command","command":"<abs a2a> --as NAME hook <session-start|prompt|stop> --runtime codex"}]}`; no `env`, no other
top-level keys. Output of `a2a hook ... --runtime codex` follows (c).
