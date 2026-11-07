# Managed command installation

Use the command exported by the installed Curator skill. Resolve the project
`.agents/bin/a2a` first (`a2a.cmd` on Windows), then the manager's `global/bin`
fallback. The default manager location is `$HOME/.curator/global/bin`; use the
configured manager home if it differs. Shell profile activation is optional.
On POSIX, a final `command -v a2a` fallback must return an absolute executable
file, not an alias or function. On PowerShell, use `Get-Command a2a -CommandType
Application` and validate its `.Source` as an existing absolute file. Refuse a
missing or ambiguous command instead of building a second binary from main.
Invoke the selected file explicitly (PowerShell: `& $a2aCommand ...`); the
examples below use `a2a` as shorthand for that resolved command.

If the command is absent, the operator installs the declared skill with Curator
and checks `a2a --help` before any account or runtime action. On a fresh machine,
`curator bootstrap --skills-root "$SKILLS_ROOT" --non-interactive` is required
before project install. Select a real Go executable with
`CURATOR_GO="$(go env GOROOT)/bin/go"`; a symlink selector can be refused.
No account provisioning, hooks or Matrix connection is required to show help.

The managed `go-repository-v1` build disables CGO/native Sherpa and exports only
`a2a`. Its sibling `a2a-keychain-status` helper is not delivered, so Keychain
credentials and named connections are unavailable in that package. Default
file credentials are a separate operator choice. Install/help evidence does
not qualify keeper use, hook continuity after a pin refresh, or idle wake;
those need a separate owner-approved runtime witness. Do not install a second
skill copy with `a2a skill install` over the Curator-managed mirror.

