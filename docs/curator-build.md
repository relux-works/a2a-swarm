# Curator command build branch

`curator-build` retains the source closure for Curator's `go-repository-v1`
command build. Main has no `vendor/` tree. Each build refresh adds regenerated
vendor and root `skill-build.json` to an explicitly named main commit. The
descriptor selects `a2a`, with `build_root: "."` and `source_dir: "cmd/a2a"`.
Everything else must match that main commit, including file modes.

Consumers select a full commit object ID, never a branch name. An optional tag
is an extra assertion. The [declaration contract](https://github.com/relux-works/curator-spec/blob/35c705be3f60ef2fc33acafa85cfb0df0d4c8041/protocol/core.md#L934)
and section 6.3 require an exact-object fetch; they do not require membership
in main or `curator-build`. Curator's network installation path reaches
`internal/buildrepo.AcquireNetwork` through `internal/install/external.go` and
fetches the OID into `refs/curator/locked`. Successful installation proves
object availability, admission and compilation; branch membership needs the
separate ancestry check below.

## Retention and refresh rule

The branch is append-only: never force-push, rebase, delete or replace it.
The repository owner and organization administrator own the ruleset that
prevents deletion and non-fast-forward updates. No signed build tag is required.
GitHub has served force-pushed-away commits by OID in observed probes: an
install can succeed after retention was broken. The violation can remain
invisible to installation until object collection, when fresh installs fail
with `build_repository_source_unavailable`. OID availability is not retention.

For the first publication, start at current main and commit the descriptor and
`go mod vendor` output together. For a later refresh, merge later main into
the existing build branch without committing yet, regenerate vendor, retain
the descriptor, and create one signed merge commit. Resolve every non-vendor,
non-descriptor entry to the named main tree. Preserve all published pins as
ancestors; push normally. The repository owner decides when to refresh after
source fixes land. Before updating consumers, append the full new OID to
`docs/curator-build-pins.txt` on main and run the committed-tree gate.

The small `curator-build-retention` workflow uses public Git reads on a hosted
runner, with no credentials, secrets, actions or vendor generation. Every six
hours and on dispatch it checks main's published anchors against the branch.
Deletion and forced-push events fail by name. It becomes active when this
workflow lands on main; push coverage requires a subsequent build refresh
containing the workflow (the initial pin does not contain it). The scheduled
check detects loss of registered pins, not every rewrite preserving those
pins or loss of unregistered intermediate commits. It detects violations;
the owner ruleset prevents them. Do not treat a pending workflow as enforcement.
The last anchor is checked even without a terminating newline. Missing main
and anchor-file reads fail with `build_main_unavailable` and
`build_anchor_file_unavailable: docs/curator-build-pins.txt`, respectively.

## Committed-tree gate

First fetch main and `curator-build` into their `origin` remote refs. The gate
uses these local refs, so stale refs are not a current remote retention proof.
Run Go with a disposable absolute HOME, cleared configuration selectors and a
prepared module cache. From the main support checkout:

```sh
make verify-vendor BUILD_REPOSITORY="$REPOSITORY" \
  BUILD_COMMIT="$FULL_BUILD_OID" MAIN_COMMIT="$FULL_MAIN_OID"
```

`tools/vendorcheck` requires full OIDs. It verifies the named main commit is
reachable from `origin/main`, compares every committed entry outside `vendor/`
and root `skill-build.json` to that main tree, and verifies the selected pin
is an ancestor of `origin/curator-build`. A source change fails with
`source_tree_drift: cmd/a2a/main.go`; a changed `go.mod` with regenerated vendor
still fails with `source_tree_drift: go.mod`. An unretained pin fails with
`build_pin_not_on_branch`.

The committed `skill-build.json` must declare schema 1 and exactly one target,
`a2a`, with driver `go-repository-v1`, build root `.` and source `cmd/a2a`.
JSON key order and whitespace do not matter. Swapped selectors, malformed JSON,
duplicate or unknown keys, extra targets and trailing JSON fail with
`build_descriptor_invalid: skill-build.json`; a missing file fails with
`build_descriptor_missing`. Descriptor validation reads the selected blob
before vendor generation; a correct worktree file cannot repair a bad commit.

The gate reads Git trees and blobs directly, independent of HEAD, worktree,
ignore rules and archive filters. It regenerates vendor from the committed
source with network-disabled Go settings and checks names, bytes and Git
executable modes in both directions. Missing files and mode drift fail with
`vendor_tree_drift: <path>`. Success names both commits and the branch:
`vendor_tree_equal commit=<oid> main=<oid> branch=curator-build`.
Timestamps and directory permissions are not Git tree properties.

The narrow tests exercise the complete CLI with local dependency fixtures:
source and replace plants, changed HEAD, clean-looking worktree over a bad
commit, ignored embedded files, missing paths, executable modes, unreachable
main, unretained pins and invalid committed descriptors, plus restored positive
controls. They require no vendor tree on main. The existing hosted `go` workflow remains unchanged.

`make curator-command BUILD_REPOSITORY="$BUILD_CHECKOUT" OUTPUT="$ARTIFACT"`
is a separate maintainer compilation check of that checkout, with offline Go
flags, CGO disabled and no native Sherpa tag. It does not qualify a commit.
Curator independently executes its fixed build driver.

## Current source and mirror boundary

The initial pin `9eb007917d7fc18e7e3f1f2c820fad6119c6b573` matches main
`711ed0d6f88ddb498b711521039f0e9980788aa0` outside the two allowed additions.
Its vendor bytes need no replacement. The skill mirror can carry newer
installation instructions from a reviewed source candidate while this binary
pin remains unchanged. Its embedded `a2a skill install` prose then remains
older: use the Curator-managed mirror. After the source candidate lands, the
owner may append a build refresh and update the consumer pin. Unlanded source
changes must not be smuggled into the build branch as vendor-only changes.
