## Local agent presence (candidate interface)

After a containing release and separately admitted adoption, an admitted
pre-launch custodian can query local metadata without acting as the target:
`a2a --profile NAME connection identity-presence --target-agent NAME --json`.
Keep the actual `A2A_AGENT` caller. Every `--as` (even own/empty) and `--human`
is forbidden. Named mode requires an explicit profile; registry-free legacy
mode omits it. This convention does not authenticate host/operator admission.

Validate `a2a.local-identity-presence/1`, the complete four-field result and its
exit pairing; unknown/missing output fails closed. Only `present` is local
presence evidence, with distinct `named_bound_metadata` and weaker
`legacy_regular_file_only` kinds. Legacy contents are unopened and validity is
unknown. Pending/inconclusive are not absent; absence never authorizes
registration/replay or proves remote/secret absence. No credentials, Keychain,
HTTP, state writes or generic owner-channel warnings are part of this query.
It grants no member/host/native/launch authority. See `docs/cli.md` for bounds,
strict public schemas, retained complete-journal positives and snapshot limits.
Before release/adoption, treat this source interface as a candidate only.
