# F1 gate (1): Stop payload fixtures, provenance

Captured on Linux on 2026-10-23. Each capture was one single-turn, trivial prompt with no tool calls, run in a fresh temp directory under the lane's scratchpad. That directory is shown below as `<capdir>` and was deleted after capture. No live hook, settings file, config, trust state or credential was created or changed. The Codex capture had host-keeper approval.

## Fixture 1: Claude Code
- File: `claude-code-2.1.309-stop.json`
- sha256: `13788893ec64890b9912576bd379e59d7876aada83092e8bbb199916b14a735b`
- Runtime: `claude --version` = `2.1.309 (Claude Code)`.
- Invocation, from cwd `<capdir>/cwd`, with A2A_AGENT unset:
  `claude -p "Reply with exactly the word: ok" --setting-sources user --settings <capdir>/settings.json --model claude-haiku-5-5`
  - The user-level settings in that environment define no hooks.
  - `--setting-sources user` excludes project and local settings, so the lane's own `.claude/settings.local.json` hooks did not load.
- Hook surface that fired: the `Stop` hook from the `--settings` file: `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"cat > <capdir>/claude-stop-payload.json"}]}]}}`. Its stdin was written verbatim.
- Output: the assistant replied `ok`.

## Fixture 2: Codex CLI
- File: `codex-cli-0.163.0-stop.json`
- sha256: `ec6f7f738b7ad3b9e2da43d043da0b31a0a528985e70805681da073c1daa6a6b`
- Runtime: `codex --version` = `codex-cli 0.163.0`. CODEX_HOME was a Curator environment, used read-only (its own session and rollout files were written by Codex as usual).
- Three runs were made, as approved; only the third produced a payload:
  1. `<capdir>/proj/.codex/hooks.json` with a Stop command hook, plus `-c 'projects."<capdir>/proj".trust_level="trusted"'` and `--dangerously-bypass-hook-trust`; no git repo. Exit 0, reply `ok`; the Stop hook did not run.
  2. As run 1, plus `git init` in `<capdir>/proj`. Exit 0, reply `ok`; the Stop hook did not run.
  3. No project layer. The hook was passed as a session override:
     `codex exec --skip-git-repo-check -m gpt-6.1-sol -c model_reasoning_effort=low -c 'hooks.Stop=[{hooks=[{type="command",command="cat > <capdir>/codex-stop-payload.json",timeout=30}]}]' --dangerously-bypass-hook-trust --sandbox read-only "Reply with exactly the word: ok"`
     Exit 0, reply `ok`. The log shows `hook: Stop` and `hook: Stop Completed`.
- Hook surface that fired: the session-flags `[hooks]` table (the `-c hooks.Stop=...` override). It was not a project or user `hooks.json`.
- Explicit limit: runs 1 and 2 show that in this setup a project `.codex/hooks.json` did not load. The cause was not established (the trust override key, project root detection, or exec-mode layer loading). Run 3 proves only that the Stop payload shape is delivered to a command hook. It does not prove how `a2a hook install` wiring for Codex will load. That must be verified separately in the S1 implementation, in a real trusted project.

## Sanitization (both files)
Only these values were replaced; every field name, field order, type and every other value is as captured.

| field | replaced with | why |
| --- | --- | --- |
| `session_id` | `00000000-0000-4000-8000-000000000001` | session identifier |
| `prompt_id` (Claude) | `00000000-0000-4000-8000-000000000002` | prompt identifier |
| `turn_id` (Codex) | `00000000-0000-4000-8000-000000000003` | turn identifier |
| `transcript_path` | `/home/user/.claude/projects/example/<session>.jsonl` (Claude), `/home/user/.codex/sessions/2026/10/23/rollout-example.jsonl` (Codex) | absolute host path |
| `cwd` | `/home/user/work` | absolute host path |

- Checked before saving: no tokens, no auth headers, no `/home/agent` or `/tmp` paths remain. The only text content is `last_assistant_message: "ok"`.
- Kept as captured:
  - Claude: `permission_mode` "default"; `effort` {"level":"medium"}; `hook_event_name` "Stop"; `stop_hook_active` false; `background_tasks` []; `session_crons` [].
  - Codex: `model` "gpt-6.1-sol"; `permission_mode` "bypassPermissions"; `hook_event_name` "Stop"; `stop_hook_active` false.

## Limits
- One payload per runtime, from a single turn with `stop_hook_active: false`. No capture covers a re-entered stop (`stop_hook_active: true`), a turn with tool calls, a long or multi-line final message, or an empty final message. Tests for those cases build on these field shapes but are synthetic.
- The Codex `permission_mode` value reflects this exec invocation's flags, not an interactive session.
- No capture is claimed for other runtime versions. The fixtures pin exactly claude-code 2.1.309 and codex-cli 0.163.0.
