# Curator command build branch

`curator-build` holds the source closure for Curator's `go-repository-v1`
command build. The default branch has no `vendor/` tree. The build branch
adds a generated `vendor/` and root `skill-build.json`; the descriptor selects
`a2a`, with `build_root: "."` and `source_dir: "cmd/a2a"`.
The descriptor belongs beside the vendored source at the pinned commit,
not at the skill mirror or on the default branch.

A skill pins a full commit object ID, never this branch name. An optional
tag is an additional assertion, not a replacement for the commit. The
[declaration contract](https://github.com/relux-works/curator-spec/blob/35c705be3f60ef2fc33acafa85cfb0df0d4c8041/protocol/core.md#L934)
and section 6.3 of that same specification require an exact-object fetch;
they do not require default-branch membership. A server may still refuse
that fetch: verify reachability through Curator before updating a consumer.

## Refresh rule

The branch is append-only: never force-push, rebase, delete or replace it.
For its first publication, start at current default-branch main and commit
the descriptor and `go mod vendor` output together. For later refreshes,
merge a later main into the existing build branch without committing yet,
regenerate vendor, retain the descriptor, then create one signed merge
commit. This preserves every previously published build commit as an ancestor.
Push normally and update the skill's full commit pin after checking the new
closure. Creating a fresh branch tip from main and replacing the old build
branch would break this rule.

## Local checks

Run Go commands with a disposable absolute HOME and cleared configuration
selectors. Use the Go version declared by `go.mod` and a prepared module cache.
`BUILD_REPOSITORY` must be the absolute path of the build checkout. From the
default-branch support checkout:

```sh
make verify-vendor BUILD_REPOSITORY="$BUILD_CHECKOUT"
make curator-command BUILD_REPOSITORY="$BUILD_CHECKOUT" OUTPUT="$ARTIFACT"
```

`verify-vendor` runs `tools/vendorcheck`: it regenerates vendor into a temporary
directory and compares relative names, entry types and regular-file bytes in
both directions. Success prints `vendor_tree_equal`; drift fails with
`vendor_tree_drift`. It rejects missing vendor and symlinks. It does not compare
filesystem permissions or timestamps. A changed vendored file must fail and
its restoration must pass before publishing a refresh.

`curator-command` builds `./cmd/a2a` with the fixed driver's offline Go flags,
CGO disabled, and no native Sherpa tag. The output goes to the requested
artifact path. Curator independently selects and executes its fixed driver;
these Make targets are maintainer checks, not a package-supplied build recipe.

The hosted `go` workflow retains its pinned actions and hosted runners.
Its unit checks for the comparison code use small fixtures and do not require
vendor on main. The full regeneration comparison is run explicitly against
the build checkout, not as a workflow prerequisite on the default branch.
