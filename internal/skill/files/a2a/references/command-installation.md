# Managed command installation

- **Curator-managed:** If either directory exists — the project's `.agents/bin/` or the manager's `global/bin/` — use the managed route. The manager home is `$HOME/.curator` by default, or the directory of `CURATOR_CONFIG` when that is set. Test directory existence, not the `a2a` file; `.agents/` or `.agents/skills/a2a` alone is not evidence. Resolve project `.agents/bin/a2a` (`a2a.cmd` on Windows), then the manager's `global/bin/a2a` (`a2a.cmd` on Windows). Refuse if neither managed command is valid or resolution is ambiguous; never fall back to PATH lookup, a self-built binary or building one.
- **Self-built:** Only when both directories are confirmed absent and the owner has authorized the self-built install recipe, follow that recipe and use its installed command path.
- **Otherwise refuse:** If the manager home, directory existence or route is uncertain, or self-built recipe authorization is missing, refuse and ask the owner which route applies. Never default to building.

Throughout this skill and its references, `a2a` is shorthand for the absolute installed command selected by this rule. Examples spelling `"$HOME/.local/bin/a2a"` use the self-built form; Curator-managed installs must substitute their resolved absolute command everywhere, including `--installed-command`.

Use this reference for the managed route; the owner-authorized self-built recipe
is in [install.md](install.md). Shell profile activation is optional.
On POSIX, require the selected managed path to be an absolute executable file,
not an alias or function. On PowerShell, use `Get-Command` on that path with
`-CommandType Application` and validate its `.Source` as an existing absolute
file. Invoke the selected file explicitly (PowerShell: `& $a2aCommand ...`).

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

