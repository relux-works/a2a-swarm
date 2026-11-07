# Terminal input fixtures

The standalone [registry bundle comparison tool](../../../docs/registryqualifier.md)
reads explicit bundle files as inert bytes and runs planted sensitivity controls.
Its fixtures are entirely synthetic and do not establish a provider release's
qualification or replace the input-layout evidence below.

The ASCII and Cyrillic suggestion fixtures derive from a read-only real Claude tmux capture from 2026-10-18. Conversation was replaced with 30 blank lines and suggestion words with neutral text. ANSI input/rule/footer structure is preserved: NBSP separator, reverse-video cursor cell, following dim suggestion and benign bypass-permissions footer. No private transcript, peer body, secret or historical spawn log is included.

`claude-idle-bypass-footer-empty.txt` is explicitly SYNTHETIC: the reviewer replaced the suggestion with a reverse-video blank cursor. It is not evidence of an independently captured empty input.

The regression test proves these layouts parse as empty and nearby plain/reverse-video human drafts and permission/plan dialogs stay blocked. Product Send controls use an isolated real tmux server; no fixture alone proves live wake delivery or atomicity. The original parser failed all three fixtures in the bounded control before correction.

`codex-owned-notice-no-hint.txt` preserves only the exact SGR-bold prompt and fixed notice line plus the following blank line from the private 2026-10-19 Codex incident capture. Its footer is explicitly RECONSTRUCTED as neutral status text: the original had Ready but no shortcut hint; model, paths, branch, metrics, conversation and warning content are omitted. This is a minimal layout regression, not an exact full-screen capture or live runtime proof. The original parser rejects the journal-owned notice because filled Codex input hides `? for shortcuts`. Empty admission still requires that hint; the no-hint exception requires the caller's exact fixed notice and rejects appended text, including dim/grey text.
