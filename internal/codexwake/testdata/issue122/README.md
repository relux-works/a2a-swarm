# ISSUE-122 current protocol fixtures

These are complete OpenAI release-tag schema snapshots, retrieved 2026-10-29 UTC:

- https://raw.githubusercontent.com/openai/codex/rust-v0.163.0/codex-rs/app-server-protocol/schema/json/v2/TurnStartParams.json
- https://raw.githubusercontent.com/openai/codex/rust-v0.163.0/codex-rs/app-server-protocol/schema/json/v2/ThreadItemsListResponse.json
- https://raw.githubusercontent.com/openai/codex/rust-v0.165.0/codex-rs/app-server-protocol/schema/json/v2/TurnStartParams.json
- https://raw.githubusercontent.com/openai/codex/rust-v0.165.0/codex-rs/app-server-protocol/schema/json/v2/ThreadItemsListResponse.json

The ordinary 0.163.0 pair was independently matched byte-for-byte to the installed
Linux CLI's `codex app-server generate-json-schema --out DIR` export. The 0.165.0
pair was independently matched to a supplied, manifest-verified installed
Darwin/arm64 export using the same non-experimental command. That binary was not
installed or executed by this producer. The experimental
`0.163.0/TurnStartParams.experimental.json` was exported locally with
`codex app-server generate-json-schema --experimental --out DIR`; optional
experimental fields explain its different fingerprint. The matching installed
0.165.0 experimental export is also retained as
`0.165.0/TurnStartParams.experimental.json`; its supplied archive had all460
manifest schema sizes/hashes/JSON independently verified. Across matching
experimental exports the tool output definitions are identical; 0.165 adds the
unrelated optional responsesapiClientMetadataPatch property, which this adapter
does not send. Comparing unlike export flags does not establish a version
incompatibility.

| File | SHA256 |
| --- | --- |
| Both ordinary TurnStartParams.json | 835e0d4ca1a0ddc4fb6db1e2b3249dd67f07c50ac2506092ba81543efc92829b |
| 0.163.0/ThreadItemsListResponse.json | 0ecf37ff62571c8411aab271ea65d22be4da1108cc2fd2dffb15a7034bfedb28 |
| 0.165.0/ThreadItemsListResponse.json | fa5a85adff4a594f6c7183aca89414338fbe51b692d5f5ff8edd88d01f51dc4d |
| 0.163.0/TurnStartParams.experimental.json | abf0d6d29d223b38c650ff1155cb9c9f9b4211e6d0e20c57d750a9e1babfdb41 |
| 0.165.0/TurnStartParams.experimental.json | f671d01c24995e99fcc110862ff3251c26d334fba212c7dc12857fb939506e47 |

Run `go test ./internal/codexwake -run '^TestCurrent' -count=1` from repository
root. The tests pin the exact schema bytes, the empty-input toolOutput shape,
retention field and the current `input_text` content tag through the actual
Client serializer/history reader using isolated server fixtures. They also require
production Dial to continue refusing both versions. Source/fixture evidence
cannot qualify an attached TUI, native model receipt, safe human-state scheduling,
live ownership/leases, recovery or empty-turn cost. Those require separately
admitted disposable native sessions. The current helper consumes the existing
host-authored journal output; it is not a replacement notice encoder or a way
to authenticate arbitrary text as host-authored.

Current documentary context: https://learn.chatgpt.com/docs/app-server
(`Start a turn` / standalone tool output). This unversioned page supports the
shape's intended meaning, not either installed build's native wake qualification.
