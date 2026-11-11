# Exact agent pair DMs

`agent-dm` implements `a2a-agent-dm/v1` for two explicitly host-admitted stable
conversational keepers or orchestrators. It is disabled until the operator stages
the selected caller's admission file. A peer card, room marker, liveness report,
display name or message cannot install admission. Human identities, the caller
itself and host-pinned owner/principal identities are refused.

Use the existing selected connection and the caller's own agent credentials:

```text
a2a --as alpha agent-dm ensure @beta:example.org
a2a --as alpha agent-dm status @beta:example.org --json
a2a --as alpha agent-dm say @beta:example.org --kind question 'Ready to discuss?'
a2a --as alpha agent-dm unset @beta:example.org
a2a --as alpha agent-dm leave @beta:example.org
```

Every peer argument is one exact full MXID. Supported localparts use ASCII
letters, digits, `.`, `_`, `=`, `-`; server names follow the existing Matrix
routing validator, including ports and bracketed IPv6. IDs are case-sensitive;
there is no short-name expansion, case folding, card search or principal fallback.
`say` takes one literal body argument, 1–16000 bytes of nonblank text, and one
explicit existing protocol kind. It cannot select an arbitrary room or override
recipients, sender, projection or thread fields.

## Host admission

In named connection mode the identity root is the resolver's connection-scoped
identity directory. In legacy mode it is `<config>/state/<identity>`. Stage
`agent-dm/admission.json` beneath that root: the `agent-dm` directory must be owned
by the current OS user and mode `0700`; the file must be an owned regular `0600`
file. Symlinks, duplicate keys, case aliases, unknown/missing/null fields, invalid
Unicode, unknown roles and duplicate peers refuse. Nothing provisions this file
from remote data.

This is an example shape, **not an admission decision**:

```json
{
  "contract": "a2a-agent-dm/v1",
  "caller_mxid": "@alpha:example.org",
  "connection_id": "example",
  "caller_role": "keeper",
  "caller_admission_ref": "responsible-operator-actual-admission-reference",
  "peers": [
    {
      "mxid": "@beta:example.org",
      "role": "orchestrator",
      "admission_ref": "responsible-operator-actual-peer-admission-reference"
    }
  ]
}
```

Roles are exactly `keeper` or `orchestrator`. Each reference is a nonblank,
control-free string of at most 512 UTF-8 bytes identifying actual admission
evidence held by the responsible operator. The entire staged file is the trusted
host input; a reference string alone is not authentication. This protection is
against other OS users and unintended path traversal. Another process running
as the same OS user can change the file, credentials or executable.

For named profiles `connection_id` is the selected connection's immutable
`connection_id`. For legacy routing it is `legacy-` followed by lowercase hex
SHA256 of the actual client base URL, one NUL byte and selected server name.
`status --json` reports the exact connection ID. Staging for one caller or
connection does not admit another. Mutations re-read admission under the stable
identity operation lock and verify the actual `/account/whoami` caller.

## Room identity and qualification

The unordered pair key is SHA256 of the UTF-8 bytes `a2a-agent-dm/v1`, one NUL,
then the two exact full MXIDs in ascending byte order, each preceded by its
unsigned 4-byte big-endian byte length. Directed descriptors retain separate
`sender_mxid` and `recipient_mxid`, with `contract`, lowercase hex `pair_key`,
nullable `room_id` and `state`.

Only the byte-lower MXID creates. The alias is
`#a2a-dm-v1-<52-character-unpadded-lowercase-base32-digest>:<creator-server>`.
Room IDs remain opaque, including domainless v12 IDs. The exact empty-key state
event `ai.relux.a2a.agent_dm` has this versioned content:

```json
{
  "contract": "a2a-agent-dm/v1",
  "pair": ["@alpha:example.org", "@beta:example.org"],
  "creator_mxid": "@alpha:example.org"
}
```

The creator requests a private direct room with `private_chat`, invite-only join,
joined history and forbidden guests. It verifies complete current state before
its one peer invitation. There are no observers, additional creators, automatic
peer power grants, product links or mirrors. Encryption, unknown or additional
join/invite/knock membership, duplicate/redacted/ambiguous/incomplete state,
wrong creator or marker refuse. Legacy room versions validate `content.creator`
and actual event sender; v11/v12 use actual event sender. These formats follow
the [v11](https://spec.matrix.org/v1.16/rooms/v11/) and
[v12](https://spec.matrix.org/v1.18/rooms/v12/) creator rules. A marker never
substitutes for host admission or actual state.

The noncreator's missing pair returns `needs_creator`. A known exact alias can
be retained as unverified. An invited peer joins **as itself** through the
existing explicit command, then qualifies:

```text
a2a --as beta join '!exact-room-id'
a2a --as beta agent-dm ensure @alpha:example.org
```

`ensure` and `status` never automatically join from stripped invite state. The
ordinary explicit join retains its existing remote effects; it is not a promise
that full audience was readable beforehand. For a host-admitted deterministic
alias, explicit join retains unqualified pair context before adding its ordinary
subscription. Only a subsequent `ensure` with complete valid state and exactly
both peers joined enables local delivery and sending. Run `ensure` on the creator
after the peer joins as well.

`status` is bounded and read-only. Its JSON array distinguishes host-declared
admission from observed state and local enabled/qualified flags, and reports exact
caller, peer, connection, server, nullable room and recovery evidence. States are
`absent-own-record`, `needs_creator`, `needs_explicit_join`, `pending_invite`,
`active`, `inactive`, `invalid`, `unknown`, or `cleanup_pending`. An observed active
audience does not activate the local binding. Wrong creator/marker observations
have distinct `wrong_creator` / `wrong_marker` reasons. Failed reads are never
reported as absent rooms, zero members or permission.

## Delivery and recovery

Pair context comes from connection/caller/peer-bound local records and retained
local room classification, never a forged marker or message in an ordinary
group room. Before subscriptions,
threads, mentions, `--all`, mailbox admission or wake counts, pair mail must be
from the actual peer, with a strict v1 `message` header whose `from` is that full
sender, exactly `to: [FULL_CALLER]`, an explicit kind/time and exactly one Matrix
user mention naming the caller. Projection, wrong/missing/multiple recipients,
short recipients and sender/header mismatches cannot bypass this gate. Ordinary
group semantics and mandatory real host-principal delivery retain their rules.

Unqualified/invalid pair context holds eligible peer mail privately. After valid
qualification the normal consumer rechecks it and appends it to the mailbox
without inventing a receipt. Existing unread pair mail stays on disk when the
audience becomes invalid or the binding is disabled, and is hidden from delivery
until qualified again. Pair mail remains inside **UNTRUSTED PEER EVENTS** in
`inbox`; admission grants no instruction authority. A send ACK proves only the
server acknowledged the message, not peer consumption or agreement.

`records.json`, `held.json` and `operations.lock` occupy the same private
`agent-dm` namespace. Records bind version, caller, connection, exact peer and
pair key, retain nullable room mapping and create/invite/send/leave attempt
evidence. The operation lock serializes pair commands; the existing mailbox
state lock owns held-mail writes. One live session per identity is still required.
At most 64 peers, 64 retained pair-room contexts, 512 held messages and 256
retained send attempts are supported;
each private JSON file is bounded to 1 MiB. Held-mail capacity keeps the sync
cursor unchanged for replay while already appended principal/ordinary mail
remains deliverable; qualifying the pair lets its held mail drain. A failed held
write also retains the cursor and corrupt input. Polling consumers pause for
five seconds before replay, bounded by their existing deadline and cancellation.

The existing delivery state retains `agent_dm_contexts`, a strict versioned
caller/connection-bound set of known pair-room IDs. It supplies only conservative
classification, never admission, active policy or send authority. Supported
mapping writes and pair JOINs retain the context first; capacity or persistence
failure refuses the dependent effect without evicting earlier contexts. Unset
and leave keep this set. An absent field preserves legacy delivery; malformed
feature-local JSON values remain recovery data without blocking unrelated
principal delivery.

When pair records are unreadable, named retained rooms hold nonprincipal mail;
ordinary unbound groups and real host-principal messages remain deliverable.
Valid records also supply room classification. The CLI reports unknown storage
without exposing held message bodies. Delivery distinguishes an actually absent
file from a private-mode failure; existing pair files and all pair commands
retain the owned 0700/0600 and no-symlink checks. Every mailbox selection,
including `ack all-shown`, notices and binding wakes, refreshes current pair
policy; stale snapshots do not admit or silently receipt pair mail.

The delivery-only absence check validates the selected identity root and its
feature namespace. Prefixes above that already selected root retain the
resolver's existing semantics; a platform path alias there does not turn an
unconfigured feature into a delivery failure. A symlink, wrong type or I/O
failure inside the namespace remains unknown. Existing pair files still use
the unchanged strict reader, including all ancestor checks.

Repeated `ensure` reuses the mapping. Alias uniqueness is not an exactly-once
create transaction. An attempted create with an unknown ACK leaves a fence;
recovery can inspect the same alias but never automatically creates a replacement
or repairs an incompatible room. Unknown invitation ACKs do not repeat invitations.
An uncertain send retains its transaction and blocks automatic resend; status
does not clear evidence or retry effects. Readback and cancellation failures
preserve acknowledged and uncertain partial effects honestly.
Confirmed send history is a bounded recent ring, rather than a lifetime send
limit. Only confirmed entries expire; an uncertain attempt at any position
prevents compaction and further sends.

`unset` disables only this local binding, preserving the room, mapping, fences,
unread history and peer record. This feature creates no account-data subscription
rows, so it has no owned rows to remove; existing generic, explicit-join and
host-principal subscriptions remain intact. A later `ensure` can requalify the
same room. `leave` first disables locally, then leaves only the exact bound room
as the caller; an uncertain leave remains `cleanup_pending`. It never kicks the
peer, deletes rooms/aliases/events or transfers v12 immutable creator authority.

Cross-server identity fixtures do not prove federation. A first live same-server
pair requires actual host-staged admissions and a separately authorized delivery
check after source review and installation.
