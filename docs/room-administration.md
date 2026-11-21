# Room names and Space child declarations

Use the selected own account and connection. Read access is decided by the
selected server; mutations require authority for the target. These commands
read, list, or change name and Space-side child declarations:

```text
a2a room name get ROOM [--json]
a2a room name set ROOM --expect-room-id !ROOM --name NAME [--json]
a2a space child get SPACE --child CHILD [--json]
a2a space child list SPACE [--json]
a2a space child add SPACE --child CHILD --expect-space-id !SPACE --expect-child-id !CHILD --via SERVER [--via SERVER ...] [--json]
a2a space child remove SPACE --child CHILD --expect-space-id !SPACE --expect-child-id !CHILD [--json]
```

Flags may be interspersed after the verb. Only `--via` repeats. `--json` without
a value means true; `--json=true` and `--json=false` select JSON and text.
Room selectors use existing alias/default-server forms or opaque IDs such as
`!x`. Resolved and expected IDs are case-sensitive UTF-8 strings of 2..255 bytes,
beginning with `!`, without Unicode spaces or controls. Aliases resolve once;
every mutation requires matching explicit expected IDs. Space and child differ.
No implicit join or same-name substitution occurs.

## Read-only child list

`a2a space child list SPACE [--json]` reads the direct `m.space.child`
declarations from one complete, bounded Space-state snapshot. Use the selected
identity joined to the Space; no write power is required. The server decides
whether that identity may read the snapshot. The command makes no writes or
joins, requests no child-room state, and does not recursively walk child
Spaces.

The response is limited to 1024 child state entries and 1 MiB of state data,
inclusive. Exceeding either limit refuses the whole command without printing a
partial list. A missing `via` property is a removed link and is omitted. A
present but malformed `via` is kept as an `invalid` row so it cannot disappear
silently; this includes an empty array, more than 16 entries, entries over 261
UTF-8 bytes, an aggregate over 4096 bytes, invalid UTF-8, and server names
rejected by the existing `ValidateParentVia` grammar. Valid `via` entries
retain server order. Rows sort by child ID, except rows with an invalid `order`
are placed after valid or unset orders.

An invalid `via` row has `status=invalid` and one fixed `reason` code matching
the existing single-child reader:

| Reason | Meaning |
| --- | --- |
| `ambiguous_property_field` | A noncanonical case variant of the `via` field is present |
| `malformed_child_via` | `via` is not an array |
| `malformed_child_server` | An array entry is not a string |
| `invalid_child_via` | Entry count, entry or aggregate length, UTF-8, or server-name validation failed |

The command prints every row and exits 3 if any `via` row is invalid. A bad
`order` does not change the row status or exit code. `order` is valid when it
contains at most 50 printable ASCII characters (`\x20`–`\x7E`). An invalid
value is represented by `order_invalid`, containing only its raw byte length;
for non-string JSON values the length is that of their compact JSON encoding.
The value itself is never printed. A missing order remains distinct from a
present empty string. No room name, topic, unknown event extension, or invalid
field content is included in the result.

JSON emits one version 1 object with `type=space-child-list`, `operation=list`,
the selected caller and Space ID, `state_type=m.space.child`, and a `children`
array. Rows contain `child_id`, `via`, `order` or `order_invalid`, `status`, and
`reason`. An invalid `via` row has no `via` field; text shows `via=invalid`.
Unset order is omitted in JSON and shown as `order=unset` in text. For example:

```text
space-child-list v1 room="!space:example.org" state_type="m.space.child" rows=1
child="!child:example.org" via=["example.org"] order="alpha" status=valid reason=
```

Exit 0 means the complete result contains no invalid `via` rows. Exit 1 means
the snapshot or output failed; limit refusals print `RULE admin_space_limit: $.space`
or `RULE admin_child_limit: $.children` with no rows. Exit 2 is invalid command
input. Exit 3 means a complete list was printed with at least one invalid `via`
row and `RULE admin_child_invalid: $.children` on stderr.

Names contain 1..255 UTF-8 bytes and no Unicode control characters. Spaces,
case and Unicode spelling remain literal; there is no trimming or implicit
empty-name clearing. Name set replaces only empty-key `m.room.name` content
with `{"name":NAME}` and does not rename an alias.

Child add replaces only `m.space.child` on SPACE with CHILD as its state key
and exact content `{"via":[SERVERS]}`. Supply 1..16 server entries, each at most
261 UTF-8 bytes and together at most 4096 bytes. Existing DNS/IPv4/bracketed
IPv6 and optional decimal-port spellings apply; order, case and duplicates are
preserved. Child remove writes exactly `{}` to that same key. It clears only
the selected Space-side declaration; reverse parent declarations and room
membership are separate facts. A selected event's old optional extensions are
replaced, and get is not an arbitrary-state restoration backup.

Every child operation first checks the Space's actual creation content in one
complete strict snapshot. For writes, that same snapshot must establish caller
JOIN, supported room-version/creator semantics and enough authority for the
concrete `m.room.name` or `m.space.child` event. An event override takes
precedence over `state_default`; neither ordinary-message power nor authority
for an unrelated event grants this permission. Child authority is checked on
SPACE only. Child get does not require local write power or JOIN on either
target; the server decides read access. A successful preflight is an observation,
not a lock on remote membership or policy.

Exact property/ACK entities are bounded to 64 KiB and policy snapshots to 1 MiB,
inclusive. Reads require complete original-endpoint responses and strict JSON.
Only validated original `404 M_NOT_FOUND` means exact-key absence. Get projects
only `name` or `via`, preserving missing fields separately from an explicit
empty string/list. Unknown extensions remain unprinted. Malformed interpreted
fields, case variants, duplicate JSON keys, truncation or permission errors
cannot become an empty successful view.

Each mutation attempts one PUT. A strict original HTTP200 ACK with an event ID
permits one exact-key readback; VERIFIED requires equality of the full JSON
tree, including absence of unexpected fields. An uncertain ACK is UNKNOWN,
with no readback or automatic retry. A valid ACK followed by absent, mismatched
or unavailable readback is UNKNOWN with its actual event ID retained. Final
connection/context drift overrides provisional success. Inspect uncertainty
through a separately authorized get; readback cannot prove server honesty,
historical transaction correspondence or later UI state.

## Output

JSON emits one buffered version1 object plus LF. Types are `room-name` and
`space-child`; existing avatar/parent/power records retain their own contracts.
Required fields are `type`, `version`, `operation`, `caller`, `room_id`,
`state_type`, `state_key`, `outcome`, `acknowledged` and `readback`.
`event_id` appears only with a validated PUT ACK. `content` appears only for
PRESENT/EMPTY reads or VERIFIED writes. No field is null or carries raw error,
policy or extension data.

| Observation | Outcome / readback | ACK | Exit |
| --- | --- | --- | --- |
| Nonempty projected read | PRESENT / present | false | 0 |
| Cleared or missing interpreted field in HTTP200 | EMPTY / present | false | 0 |
| Validated exact-key absence | ABSENT / absent | false | 0 |
| Uncertain admitted read | UNKNOWN / unavailable | false | 1 |
| Entered write guard refusal or validated PUT permission/token rejection | REFUSED / not-attempted | false | 1 |
| Uncertain/unverified PUT ACK | UNKNOWN / not-attempted | false | 1 |
| Exact acknowledged readback | VERIFIED / match | true | 0 |
| ACK followed by absent/mismatching/unavailable readback | UNKNOWN / absent, mismatch or unavailable | true | 1 |

Preflight failures print no result. A guard refusal before a getter's first
property request also prints no result. Text output is one line:

```text
TYPE v1 OP room=QUOTED_ROOM key=QUOTED_KEY outcome=OUTCOME acknowledged=BOOL event_id=QUOTED_ID_OR_EMPTY readback=READBACK content=COMPACT_JSON_OR_null
```

Identifiers use Go `strconv.QuoteToASCII`; `null` in text is the absent-content
sentinel. JSON content is escaped and never contains literal control characters.
Output is buffered before its first write; a failed OS stream can still be
partial and cannot roll back a possible remote effect.

Errors on these new commands print exactly `RULE <rule>: <location>` on stderr.
Usage/selector/name/via input failures exit2 before credentials or networking.
Session, target, Space, policy, power, context, state, write, readback and output
failures exit1. Fixed locations are `argv`, `$.target`, `$.child`, `$.expected`,
`$.name`, `$.via` or `$.via[N]` (zero-based), `$.session`, `$.space`, `$.policy`,
`$.context`, `$.content`, `$.write`, `$.readback` and `$.output`.
Failed values, private paths and server/helper error bodies are omitted.

Fixture checks use isolated selected-account stores and fake HTTP:

```sh
go test -p 2 ./internal/cli ./internal/matrix -run 'TestRoomAdmin|TestRoomPropert|TestRoomPower' -count=1
```

They cover executable JSON/text expectations, positive/refusal neighbors,
request/effect ledgers, diagnostic markers, closed-consumer rejection plants and
old-command compatibility. They do not qualify live room changes or installation.
