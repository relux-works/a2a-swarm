# Claude input fixtures

The ASCII and Cyrillic suggestion fixtures derive from a read-only real Claude tmux capture supplied by the independent reviewer on 2026-10-18 (review/advice event `$BKDEYo_yTPsN5TAYNERZ3cMkBWIq6CY0_UgmajDwBs8`). Conversation was replaced with 30 blank lines and suggestion words with neutral text. ANSI input/rule/footer structure is preserved: NBSP separator, reverse-video cursor cell, following dim suggestion and benign bypass-permissions footer. No private transcript, peer body, secret or historical spawn log is included.

`claude-idle-bypass-footer-empty.txt` is explicitly SYNTHETIC: the reviewer replaced the suggestion with a reverse-video blank cursor. It is not evidence of an independently captured empty input.

The regression test proves these layouts parse as empty and nearby plain/reverse-video human drafts and permission/plan dialogs stay blocked. Product Send controls use an isolated real tmux server; no fixture alone proves live wake delivery or atomicity. The original parser failed all three fixtures in the bounded control before correction.
