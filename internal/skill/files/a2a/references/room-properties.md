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
The existing avatar/parent operations below retain their previous contract.

Use the owner-approved selected own identity and connection. These source
commands do not authorize a live mutation or installation by themselves.

```text
a2a room avatar get ROOM [--json]
a2a room avatar set ROOM --expect-room-id !ID --mxc MXC [--json]
a2a room parent get CHILD --parent PARENT [--json]
a2a room parent set CHILD --parent PARENT --expect-child-id !CHILD --expect-parent-id !PARENT --via SERVER [--via SERVER...] --canonical true|false [--json]
```

Resolve aliases once without redirects. Sets require explicit matching expected
room IDs, including for opaque ID inputs. Avatar set uses an existing MXC URI
and the fixed empty `m.room.avatar` key; it never uploads or checks media.
Parent set changes only the child's `m.space.parent` key for the distinct
parent ID, preserving explicit server order/case and canonical true/false.
It creates no reciprocal link, joins no room, and changes no connection config.

Version 1 JSON/text distinguishes a bounded write ACK from VERIFIED full-content
readback. An uncertain response is UNKNOWN with any known event ID retained;
never repeat set as a blind retry. A separately authorized get can inspect the
exact key. A pre-operation refusal prints no operation result. ABSENT is a
validated exact-key not-found response, not proof that the room exists.

The complete input, refusal and readback contract is in the repository's
`docs/cli.md`, under “Typed room properties”. No installed adoption is implied.
