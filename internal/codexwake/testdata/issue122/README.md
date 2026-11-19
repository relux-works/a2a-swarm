# ISSUE-122 current protocol fixtures

These are complete OpenAI release-tag schema snapshots, retrieved 2026-10-29 UTC:

- https://raw.githubusercontent.com/openai/codex/rust-v0.163.0/codex-rs/app-server-protocol/schema/json/v2/TurnStartParams.json
- https://raw.githubusercontent.com/openai/codex/rust-v0.163.0/codex-rs/app-server-protocol/schema/json/v2/ThreadItemsListResponse.json
- https://raw.githubusercontent.com/openai/codex/rust-v0.165.0/codex-rs/app-server-protocol/schema/json/v2/TurnStartParams.json
- https://raw.githubusercontent.com/openai/codex/rust-v0.165.0/codex-rs/app-server-protocol/schema/json/v2/ThreadItemsListResponse.json
- https://raw.githubusercontent.com/openai/codex/rust-v0.167.0/codex-rs/app-server-protocol/schema/json/v2/TurnStartParams.json
- https://raw.githubusercontent.com/openai/codex/rust-v0.167.0/codex-rs/app-server-protocol/schema/json/v2/ThreadItemsListResponse.json

There is NO `TurnStartParams.experimental.json` at that path in tag
`rust-v0.167.0` (404); the 0.167.0 experimental snapshot below comes only from
a local `--experimental` export.

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

The ordinary 0.167.0 pair was independently matched byte-for-byte to a local
export of the official release asset `codex-x86_64-unknown-linux-musl.tar.gz`
of `rust-v0.167.0` (SHA-256
9d64836106d5b374e90ee4454c7ebcde7ca7106558bd7e41a34b9f26ebda251a), unpacked
into scratch and run only as `codex app-server generate-json-schema --out DIR`
and `codex app-server generate-json-schema --experimental --out DIR` with
HOME, CODEX_HOME and TMPDIR inside scratch. `0.167.0/TurnStartParams.experimental.json`
is that experimental export's `v2/TurnStartParams.json`. Between 0.165.0 and
0.167.0 the `TurnToolOutput` definition and the sent `threadId`/`input`/
`toolOutput` properties are identical in both ordinary and experimental
exports. Outside the sent slice, 0.167.0 changes the `AudioUserInput` variant
of `UserInput` (this adapter always sends `input: []`) and adds an optional
`name` to `AdditionalContextEntry` (never sent). On the read side,
`FunctionCallOutputBody`, `FunctionCallOutputContentItem` and the
`functionCallOutput` history item are identical; `ThreadItem` only gains
optional lifecycle timestamps and an image-generation identifier.

| File | SHA256 |
| --- | --- |
| 0.163.0 and 0.165.0 ordinary TurnStartParams.json | 835e0d4ca1a0ddc4fb6db1e2b3249dd67f07c50ac2506092ba81543efc92829b |
| 0.167.0/TurnStartParams.json | 4e25cd48986f73e600d9949a055ddb994a75b4a38790e935274646f0fece00ad |
| 0.163.0/ThreadItemsListResponse.json | 0ecf37ff62571c8411aab271ea65d22be4da1108cc2fd2dffb15a7034bfedb28 |
| 0.165.0/ThreadItemsListResponse.json | fa5a85adff4a594f6c7183aca89414338fbe51b692d5f5ff8edd88d01f51dc4d |
| 0.167.0/ThreadItemsListResponse.json | 054ebb1e51fad510f1ff9bd2fd06a96d9db06ca4f8963207dccfef91ea8e540b |
| 0.163.0/TurnStartParams.experimental.json | abf0d6d29d223b38c650ff1155cb9c9f9b4211e6d0e20c57d750a9e1babfdb41 |
| 0.165.0/TurnStartParams.experimental.json | f671d01c24995e99fcc110862ff3251c26d334fba212c7dc12857fb939506e47 |
| 0.167.0/TurnStartParams.experimental.json | d13cb408b906669e8d3e8694b42148a75180d4dcd0eeac6867271add8dfa1864 |

Run `go test ./internal/codexwake -run '^TestCurrent' -count=1` from repository
root. The tests pin the exact schema bytes, the empty-input toolOutput shape,
retention field and the current `input_text` content tag through the actual
Client serializer/history reader using isolated server fixtures. They also require
the unqualified production Dial to continue refusing every current version, and
DialQualified to admit one only when a protected host entry names that exact
version under qualification (see docs/cli.md "Native Codex doorbell").
Source/fixture evidence
cannot qualify an attached TUI, native model receipt, safe human-state scheduling,
live ownership/leases, recovery or empty-turn cost. Those require separately
admitted disposable native sessions. The current helper consumes the existing
host-authored journal output; it is not a replacement notice encoder or a way
to authenticate arbitrary text as host-authored.

Current documentary context: https://learn.chatgpt.com/docs/app-server
(`Start a turn` / standalone tool output). This unversioned page supports the
shape's intended meaning, not either installed build's native wake qualification.
