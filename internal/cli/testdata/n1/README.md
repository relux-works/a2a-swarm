# N1 private source fixture — PREPARATION ONLY

The build tag `n1_fixture` alone does not instrument production. The reviewed
`prepare.go` creates a Go source overlay that retains the production guard,
image, selectors, conflict checks, locks and terminal consumer. It replaces only
enumeration and adds fail-closed PID admission and deterministic observation
latches. Never run the tagged process selectors without the later Source GO.
Ordinary builds do not include these files or this package.

Exact execution selectors and ownership limitations are in the frozen R4 plan.
Compiler success is not a lifecycle witness. The global enumerator, ordinary
full suite, other process tests and provider/native use remain unqualified.

Prepare source maps and both overlays with the already reviewed private offline
environment: `go run ./internal/cli/testdata/n1 .temp/92-r4b/overlay`.
This command formats bytes with Go's parser and writes files only. Compile with
`go test -c -p=2 -tags=n1_fixture -overlay=<fixed-overlay.json> -o <fixed.test> ./internal/cli`;
use the corresponding old-snapshot overlay for the single plant. These commands
compile, and do not run any selector. Outputs belong in `.temp/`; exported
packets exclude test binaries, runtime directories and synthetic credentials.
