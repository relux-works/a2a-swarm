# Terminal input fixtures

The ASCII and Cyrillic suggestion fixtures derive from a read-only real Claude tmux capture supplied by the independent reviewer on 2026-10-18 (review/advice event `$BKDEYo_yTPsN5TAYNERZ3cMkBWIq6CY0_UgmajDwBs8`). Conversation was replaced with 30 blank lines and suggestion words with neutral text. ANSI input/rule/footer structure is preserved: NBSP separator, reverse-video cursor cell, following dim suggestion and benign bypass-permissions footer. No private transcript, peer body, secret or historical spawn log is included.

`claude-idle-bypass-footer-empty.txt` is explicitly SYNTHETIC: the reviewer replaced the suggestion with a reverse-video blank cursor. It is not evidence of an independently captured empty input.

The regression test proves these layouts parse as empty and nearby plain/reverse-video human drafts and permission/plan dialogs stay blocked. Product Send controls use an isolated real tmux server; no fixture alone proves live wake delivery or atomicity. The original parser failed all three fixtures in the bounded control before correction.

`codex-owned-notice-no-hint.txt` preserves only the exact SGR-bold prompt and fixed notice line plus the following blank line from the private 2026-10-19 Codex incident capture (events `$FrL4L6Xv7lXkbUC7kH3Mw2YKZa3NBlJkhYMac4FSyh8` and `$pnCD0wMvgwBbkGl4nB5LKirQlEaGe2UZRv0-3ujHA-M`). Its footer is explicitly RECONSTRUCTED as neutral status text: the original had Ready but no shortcut hint; model, paths, branch, metrics, conversation and warning content are omitted. This is a minimal layout regression, not an exact full-screen capture or live runtime proof. The original parser rejects the journal-owned notice because filled Codex input hides `? for shortcuts`. Empty admission still requires that hint; the no-hint exception requires the caller's exact fixed notice and rejects appended text, including dim/grey text.

# Session-registry fixtures

`claude-code-2.1.312-registry-derived.json` was derived mechanically from a live Claude Code 2.1.312 interactive session record (`sessions/<pid>.json`) on m1max on 2026-10-29. The session was started without `-n`, so it has `nameSource=derived`. The key set, key order and value types are as captured. These values are also as captured: `version`, `peerProtocol`, `peerFeatures`, `kind`, `entrypoint`, `pidDomain` and `nameSource`.

Identity-bearing values were replaced with the synthetic identity that the registry tests already use: `pid`, `procStart`, `sessionId`, `cwd`, `name`, `messagingSocketPath`, `tmux` and the four timestamps. `status` is `shell`, a value sampled from that session while it was idle with background shells.

Qualification of 2.1.311 and 2.1.312 also rests on a static comparison of the official bundles: 2.1.310 and 2.1.312 as installed, and 2.1.311 as the npm `@anthropic-ai/claude-code-darwin-arm64` package. All three were read as bytes and never executed.

The comparison extracts complete functions by brace matching, and it skips string, template and regex literals while doing so. Each function is then normalized:
- String literals and property names are kept verbatim. Only the values of the build constants (`VERSION`, `BUILD_TIME`, `GIT_SHA`) are masked.
- The remaining identifiers are renamed consistently within each function, including inside template expressions.

Three functions are token-identical across 2.1.310, 2.1.311 and 2.1.312, and also 2.1.302: the record construction function, the record reader, and the partial-update writer that rewrites `sessions/<pid>.json`. The nine registry call sites of that writer write the same key set in every version.

Planted controls were each reported: a property rename, a string-literal change and a swap of two callees.

This is heuristic evidence, not a proof. Its limits:
- Helpers are compared only where a call target resolves to exactly one function or class definition, and most do not.
- `status` values are supplied by callers that were not compared. An unknown status still refuses readiness.

No live 2.1.311 record and no live user-named 2.1.312 record were captured. The user-named shape stays covered by the synthetic `registryFixture`.
