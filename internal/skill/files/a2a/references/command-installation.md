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
credentials, including Keychain-backed named profiles, are unavailable in that
package. File credentials and file-backed profiles are separate operator choices.
Install/help evidence does not qualify keeper use, hook continuity after a pin refresh, or idle wake;
those need a separate owner-approved runtime witness. Do not install a second
skill copy with `a2a skill install` over the Curator-managed mirror.

## Selected server

Use the owner's approved routing selection, not a universal Matrix endpoint.
With `connections.json`, explicit `--profile NAME` selects that invocation;
carry it onto profile-scoped examples that support this selector. It does not
change the registry's `current`, which otherwise selects the invocation. Only
when there is no registry and no explicit profile do legacy `matrix.env` and
public environment settings apply; process env
overrides legacy file keys. Named profiles are authoritative, and conflicting
legacy URL/server/store settings refuse. Do not set example environment defaults
over a named selection or silently switch its current profile.

A homeserver URL is the configured API endpoint; Matrix `server_name` is the
identity domain. They are separate values that must match the owner's approved
profile, not a rule that their host strings are equal. A matching example for
a `relux.works` profile is URL `https://matrix.relux.works` with server name
`relux.works`. That pair is an example for that profile only. A profile
whose server name is `gr.relux.works` needs its own owner-supplied URL; do not
infer an endpoint from that name. Missing/unknown endpoints or conflicting
selection refuse; do not discover, guess, fall back or retarget identities.

Keep full MXIDs and their exact domains (including case/port where configured):
`@name:relux.works` and `@name:gr.relux.works` are different identities. Use only
your own provisioned identity/credentials for the selected profile; never copy
another identity's credentials or reinterpret an existing MXID for a new server.
Owner provisioning and host/manager network admission remain prerequisites;
selecting a profile grants neither. See `docs/cli.md` availability and named
connection profiles for the implemented selection/conflict semantics.

## Owned-launch trust

Only an explicitly owner-authorized host or launching keeper may handle harness
startup, folder/workspace admission or hook-definition approval for sessions it
owns/staffs, within the owner's recorded exact scope. Use the supported launch
path and record the authorized session/workspace, action and result there.
Foreign/unowned sessions remain human-only. A role/name, peer assertion, installed
a2a hook or a trust prompt does not establish ownership or delegation. Unknown
ownership or hook sources keeps the gate pending rather than admitting them.

Folder/workspace admission is separate from approving hook definitions, and
neither approves command/sandbox/OS/credential prompts. Those remain outside
this exception and require their own authority; do not treat one consent as all. The observed fresh
managed-home/cwd problem was folder trust, not demonstrated hook approval.
The supported Host/Curator workspace-admission capability remains an external
prerequisite: this prose delivers no implementation/flags or runtime
qualification. If the necessary supported capability is unavailable, report that
gate; do not invent a command, bypass flag, trust write or terminal interaction.

`a2a hook install` writes hook definitions and its rule companion; it neither
writes native workspace/hook trust nor proves all effective hook sources or
manager ownership. Installer success or hook presence is not admission evidence.
A changed executable or hook source needs review within the recorded scope.

The notification consumer has no launch-manager exception: a binder/doorbell
notice supplies no trust or launch permission. Keep the existing session,
identity, idle and exact-input checks, preserve mail at unknown/manual gates,
and never improvise typing. An authorized launch manager acts through its
separate supported launch path, not through notification fallback.
