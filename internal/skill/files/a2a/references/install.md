# Install the self-built command

- **Curator-managed:** If either directory exists — the project's `.agents/bin/` or the manager's `global/bin/` — use the managed route. The manager home is `$HOME/.curator` by default, or the directory of `CURATOR_CONFIG` when that is set. Test directory existence, not the `a2a` file; `.agents/` or `.agents/skills/a2a` alone is not evidence. Resolve project `.agents/bin/a2a` (`a2a.cmd` on Windows), then the manager's `global/bin/a2a` (`a2a.cmd` on Windows). Refuse if neither managed command is valid or resolution is ambiguous; never fall back to PATH lookup, a self-built binary or building one.
- **Self-built:** Only when both directories are confirmed absent and the owner has authorized the self-built install recipe, follow that recipe and use its installed command path.
- **Otherwise refuse:** If the manager home, directory existence or route is uncertain, or self-built recipe authorization is missing, refuse and ask the owner which route applies. Never default to building.

Throughout this skill and its references, `a2a` is shorthand for the absolute installed command selected by this rule. Examples spelling `"$HOME/.local/bin/a2a"` use the self-built form; Curator-managed installs must substitute their resolved absolute command everywhere, including `--installed-command`.

See [managed installation and limits](command-installation.md) for the managed
route. Run the recipe below only after the self-built conditions above hold.

After owner authorization, clone the repository (or use an existing checkout),
then run the recipe below from that checkout. It works with Bash on macOS/Linux.
Each installation uses a fresh image directory: no hash utility is needed and
an existing or interrupted image is never reused. Publish the command only after
`install` succeeds; an interrupted build/install leaves the previous command intact.

```bash
git clone git@github.com:relux-works/a2a-swarm.git ~/src/a2a-swarm
cd ~/src/a2a-swarm
```

```bash
set -eu
umask 022
mkdir -p .temp
go build -o .temp/a2a-install ./cmd/a2a
mkdir -p "$HOME/.local/libexec/a2a" "$HOME/.local/bin"
image_dir=$(mktemp -d "$HOME/.local/libexec/a2a/image.XXXXXX")
image="$image_dir/a2a"
install -m 0555 .temp/a2a-install "$image"
link_stage=$(mktemp -d "$HOME/.local/bin/.a2a-link.XXXXXX")
ln -s "$image" "$link_stage/a2a"
mv -f "$link_stage/a2a" "$HOME/.local/bin/a2a"
rmdir "$link_stage"
```

The image must belong to the installing user. Both its ancestry and the installed
command's ancestry must be free of Git markers and group/world-write mode bits.
`umask 022` protects newly created directories; it does not repair existing ones.
Before installing, inspect existing custody paths, including `$HOME` and its
ancestors. For an existing directory you own and whose permissions you control,
remove group/world write with `chmod go-w /absolute/path/to/directory`; otherwise
ask its custodian for a suitable installation path. Do not relax the custody checks.
These checks inspect POSIX mode bits, not macOS extended ACL grants; `docs/cli.md`
records that unresolved custody limitation.

Do not build over a checkout's `bin/a2a`: it may already link to a live installation.
Do not retarget a live command without its custodian's authorization. Interrupted
installation may leave an unpublished image or link-stage directory; remove only
your own recorded leftovers, retaining every image still referenced by hooks.

For self-built installs, use the installed command `"$HOME/.local/bin/a2a"`, which
points directly to the image at `"$image"`, for onboarding and hook installation,
including across fresh tool shells. For bare `a2a` examples, first add
`export PATH="$HOME/.local/bin:$PATH"` in each shell or persist it in your approved
shell configuration; otherwise use the same absolute spelling. Curator-managed
installs must substitute their resolved absolute command in every example,
including `--installed-command`; do not add this self-built directory to PATH.

Onboarding records a fixed image path. To opt into future command-link upgrades,
run the manual hook install in SKILL.md with
`--installed-command "$HOME/.local/bin/a2a"`. The command link targets the fully
installed image directly, and its same-directory rename publishes it atomically.
