# Qualifying a new Claude Code session-registry schema

Terminal readiness admits a Claude Code session-registry record only for exact,
source-qualified versions (`internal/tmuxwake/registry.go`). An unknown version
keeps the legacy-layout admission only. Adding a version is a reviewed source
change. This page lists the evidence that change needs, and the one step a repo
tool performs: describing a live record and deriving a synthetic fixture.

## The live record: `a2a-registry-record`

```bash
go build -o bin/a2a-registry-record ./cmd/a2a-registry-record
bin/a2a-registry-record --record "$CLAUDE_CONFIG_DIR/sessions/<own-pid>.json"                  # keys and types
bin/a2a-registry-record --record "$CLAUDE_CONFIG_DIR/sessions/<own-pid>.json" --fixture NEW.json  # plus a fixture
```

Exit status: 0 success; 2 unusable input, bad usage or a refused fixture.

- **Bounded read.** The record is opened without following a final symlink and without blocking on a FIFO. The opened descriptor must be a regular file of at most 64 KiB, and the read itself is limited, so a file swapped or grown after the check cannot exceed it. (On non-unix platforms a pathname symlink check precedes the open and the race remains.)
- **Manifest.** It prints the record's top-level keys in order with JSON types, never a value. Duplicate keys, trailing data and non-objects are refused.
- **Fixture.** It writes a new file, never an existing one.
  - Identity values (`pid`, `sessionId`, `cwd`, timestamps, `procStart`, `tmux`, `messagingSocketPath`, `name`, `bridgeSessionId`) are replaced with the fixed synthetic values the committed fixtures use.
  - Protocol fields keep their value only if it is in the reviewed vocabulary: a `N.N.N` version, a small `peerProtocol` number, a `peerFeatures` array of known features, and known `kind`, `entrypoint`, `pidDomain`, `nameSource` and `status` values.
  - Any other value refuses the whole fixture, however short, as do `null` where an array is expected, an unknown key, a non-empty `formerNames` history, or a changed identity type. The refusal names the field, never its value.
  - Admitting a new vocabulary value is a reviewed change to `internal/registryqual/record.go`.

A live record shows only the status values observed while sampling, and a capture from one installation is not a capture of every launch mode (for example a user-named session). State both limits in the evidence.

## Static comparison of the bundles (manual)

There is deliberately no repo tool for this. Two independent reviews of a heuristic comparator for minified bundles (issue 135) showed that lexing and scope questions need a real JavaScript parser: for example, whether a later `x=…` rebinds the compared function or a local. A tool that reports "equivalent" without that analysis would overclaim.

When static evidence is useful, compare the candidate's registry record constructor, reader and partial-update writer with the newest qualified version, reading the bundles as bytes only and never executing them. Record the method, what was compared, what was not (helpers, callers, shared state), and every limit, as PR 127 did in `internal/tmuxwake/testdata/README.md`. Such a comparison is supporting evidence, never proof of equivalence.

## Qualifying a release

1. On a session of the candidate version, run `a2a-registry-record` (with `--fixture` if the shape is new). Compare the key/type manifest with the qualified schema in `internal/tmuxwake/registry.go`.
2. Optionally add static bundle evidence as described above, with its limits.
3. Make the reviewed source change: the exact version in the allowlist, the captured synthetic fixture with paired negatives (busy status, next version, unknown key), and the documentation lists. Get independent review as for any source change.

No evidence step widens the allowlist by itself, and no tool edits it.
