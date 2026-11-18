# Typed room properties

Room name and Space child operations have distinct version1 result types and
fixed safe diagnostics. Their mutations require exact expected IDs and a strict
concrete-event power preflight; child operations prove the target is a Space.
See `docs/room-administration.md` for bounds, closed output and uncertainty rules.

```text
a2a room name get ROOM [--json]
a2a room name set ROOM --expect-room-id !ROOM --name NAME [--json]
a2a space child get SPACE --child CHILD [--json]
a2a space child add SPACE --child CHILD --expect-space-id !SPACE --expect-child-id !CHILD --via SERVER [--via SERVER ...] [--json]
a2a space child remove SPACE --child CHILD --expect-space-id !SPACE --expect-child-id !CHILD [--json]
```

Child changes affect only the Space-side declaration. They do not clear reverse
parents or change membership. UNKNOWN is not permission to repeat a mutation.

Use the owner-approved selected own identity and connection. These source
commands do not authorize a live mutation or installation by themselves.
