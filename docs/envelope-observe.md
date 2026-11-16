# Receive-side envelope observation (M0)

Ordinary `a2a inbox` and `a2a read` label messages with exactly one result:

- `verified: <author>`: the exact full Matrix sender is bound to the signed
  author key by the host roster, and the pinned `VerifyMessage` accepts.
- `unsigned (no envelope)`: no `works.relux.swarm.envelope` content member.
- `invalid: <named reason>`: malformed, oversized, mismatched or unverifiable
  input; the suffix is a fixed local code, never received text.
- `unknown-author`: a structurally parseable author cannot be resolved against
  an available protected host roster, including missing/unreadable configuration.

Text adds a quoted `verification` field to the generated event header, placed
before every peer-controlled field (sender, room, kind, topic, re, body excerpt).
Header values from peers are not quoted, so a peer can write its own
`verification=...` text later on the line; only the FIRST `verification` token
of a generated header is the observation. Prefer `--json` for machine reading.
`--json` adds the string `verification`
member to each message record. Inbox brief summaries, private saved frames and
`inbox show` retain the observation. Saved observations describe the original
read; `inbox show` does not refresh them. Invites and gap records have no label.
The strict `read --verification-input` and `read --envelope-input` extraction
modes keep their existing contracts and do not add a verdict.

**Observe mode refuses nothing.** The result never changes matching, mailbox
storage, delivery, read receipts, acknowledgements, wake or hook decisions.
An invalid envelope is still printed and receipted under the existing output
success rules. An observer error is a label, never a command failure.
There is no enforcement switch, signer, key creation, private key input,
network lookup or roster writer in this feature. A label grants no action or
operator authority. General grant/action-schema authorization is outside M0.

## Host configuration and protection

In the selected configuration root, create `envelope-observe.json` with an
explicit absolute local roster path:

```json
{"roster_path":"/etc/a2a-swarm/author-roster.json"}
```

Named connections use their original configuration root (`Paths.Root`), so
identity or connection selection does not select an unprotected key cache.
There is no implicit path, environment key-discovery fallback or chain lookup.
The config is bounded to 4096 bytes. Missing, malformed, unreadable or relative
configuration means an unavailable roster. Changing this selector can select
only another protected roster; it cannot make an agent-written roster trusted.

On Unix the roster must be a root-owned ordinary file, and every ancestor must
be a root-owned directory. No group/other write permissions and no symlinks
are accepted at any level. The effective caller must be non-root. The agent
cannot chmod, overwrite, rename or replace this path. An agent-owned file with
mode 0444 is **untrusted**. Use the physical path on systems where `/etc` is a
symlink. Other platforms currently treat host rosters as unavailable, retaining
mail. This change provisions no privileged file or live trust store.

The host must supply independently authenticated, current verifier evidence;
the roster JSON is a protected local snapshot, not a new signed wire format or
an assertion accepted from an agent. A bare public-key list cannot meet the
pinned verifier's attestation and revocation requirements. The host owns snapshot
refresh, clock qualification, generation/currentness and journal continuity.
M0 reads once per inbox/read command and never advances the verifier's candidate
watermark or journal plan. It cannot qualify those host duties by reading JSON.

## Roster schema

The bounded, duplicate-free, UTF-8 JSON has exactly these top-level members:

```json
{
  "version": "a2a-envelope-roster/1",
  "authors": {"@worker:example.invalid": "ed25519:<canonical-public-key>"},
  "facts": {}
}
```

The key above is a placeholder, deliberately unusable as trust. `authors`
contains 1..4096 exact full ASCII Matrix sender identifiers (at most 255 bytes)
to valid Ed25519 or BIP-340 public key IDs. Display names and event-supplied
metadata cannot add authors. A known sender with a different signed author is
`invalid: author_mismatch`; an absent sender is `unknown-author`.

The file is at most 1 MiB and depth 16. `facts` accepts only these members,
using the in-process shapes documented by the pinned
[verifier contract](https://github.com/relux-works/swarma-envelope/blob/065e25f69da8b28cdf3ea35d9fcd7fc4aacc4d5e/verifier/README.md):

| Host member | Verifier evidence / representation |
| --- | --- |
| `audience` | Exact independently configured installation string |
| `service_root` | Purpose-5 `{key,purpose}` |
| `agent_revocation_root` | `{key,purpose,attestation_root}` |
| `operator_root`, `operator_owner` | Optional protected approver root and owner |
| `attestations` | Signed content ID -> canonical unpadded base64url original attestation bytes |
| `operator_attestations` | Device key ID -> base64url original attestation bytes |
| `active_generations` | Author key -> full protected current-generation binding |
| `revocations` | Issuer key -> `{availability,max_age,rollback_safe,snapshot}`; snapshot is base64url original signed bytes; integer `max_age` <=60 seconds |
| `time` | Independently qualified `{trusted,fresh,authority_until}` for the local receive-time clock |
| `watermark_available`, `watermark` | Host-established continuity flag and exact durable timestamp |
| `id_journal_available`, `seen` | Host-established continuity flag and content ID -> base64url original accepted envelope bytes |
| `signer_facts` | Protected current device facts when approvers are present |
| `commit_signing_key`, `enrollment_publisher` | Optional separate role keys |

Missing facts stay unavailable; empty journals must be explicitly supplied
only when the host has established they are complete. Unknown facts, invalid
key/record encodings or malformed snapshots make the roster unavailable.
The consumer fixes required roles to `author`, derives admitted author keys
only from `authors`, and supplies `Now` from the current local receive-time
clock. It supplies `Delivery` from the actual selected room ID, own full
credential recipient ID and exact original Matrix `content.body` UTF-8 bytes.
Those inputs cannot be overridden in the roster or envelope.

The verifier checks the signed `body_sha256` over the complete Matrix body,
before header removal, display sanitization or truncation. It also checks
room/recipient/audience, signatures, profiles, root attestations, generations,
revocation, time and the host-provided journal. All attached approvers must
pass. No key is trusted because it appears inside an envelope.

Strict verification occurs **at read time**. Historical envelopes may report
`invalid: expired`, and stale host facts may report e.g. `time_unavailable` or
`key_binding_unavailable`. M0 does not invent signing-time trust or downgrade failures
to crypto-only success. It supports the inherited own-snapshot freshness path;
registry-currency receipts and owner-act authority are later integration work.

The existing 60 KiB event-content budget and 65536-byte envelope ceiling also
bound observation. Oversized carried content is labelled `content_too_large`
and still delivered. Invalid base64url, duplicate keys, malformed UTF-8 and
body aliases cannot become verified. Input remains untrusted after labelling.

## Signed posts

A signed post is an envelope whose signed `action` is `a2a.post/1` (send side:
the "Signed posts" section of [cli.md](cli.md); two-sided contract:
[signed-post-contract.md](signed-post-contract.md)). Envelopes with any other
action keep the existing recipient and metadata rules. For a signed post the
observer adds two steps, and the host roster still decides who the author is:

1. **Recipient.** The envelope is verified first with `Delivery.to` = the
   reader's own full user id. Only when that result is
   `message_recipient_mismatch`, AND the signed `to` starts with `!`, AND it
   equals the selected room id, it is verified once more with `Delivery.to` =
   that room id. There is no other fallback: a post signed for another user or
   another room stays `invalid: message_recipient_mismatch`.
2. **Metadata.** After the verifier accepts, the signed metadata is compared
   with the event: the signed `arguments` must be exactly `{kind, topic, re,
   to}`; `kind`, `topic` and `re` must equal the event header's values (an
   absent header member is the empty text); `to` must equal the event's
   `m.mentions.user_ids` in order (absent is the empty list), hold at most 64
   distinct ids, and be exactly the signed `to` for a post to one user; the
   signed target must be the fixed `{"id":"a2a/post","kind":"station"}`. A
   missing header and a case variant of `ai.relux.a2a` or `m.mentions` also
   differ. Any difference is `invalid: metadata_mismatch`.

A room-addressed post is then `verified` only when the reader's own id is in
the signed list or the list is empty (a room broadcast); otherwise it is
`invalid: recipient_not_addressed`.

For any verified envelope, a present `works.relux.swarm.author_attestation`
content member must be a string containing canonical unpadded base64url of
1..8192 raw bytes. The observer uses the pinned module's `record.ContentID`
for the parsed `attestation/1` body, also checking the record's own `id`, and
compares it with signed `body.author_attestation`. A mismatch, malformed value
or oversize value is `invalid: attestation_mismatch`. This member is outside
the envelope and signed bytes; its binding is the signed content id. An absent
member preserves roster-only verification.

Named limits:

- **Read time.** The verifier runs at read time, so a signed post read more
  than 120 seconds after it was signed is labelled `invalid: expired`
  (observed in `TestObserveSignedPost/expired` and
  `TestPostSignReadAfterLifetime`). Observe mode still delivers it.
- **Roster only; carried issuer not verified.** The author is decided by the
  host roster and its protected facts, never by the carried attestation. Only
  the carried content id is compared: no purpose-5 issuer pin exists yet, so
  the carried attestation adds no purpose-5 expiry or revocation evidence.
  A withdrawn key stays valid until the roster changes.
- **Mentions carry the recipients.** The header's `to` holds display names
  (`all`, local names), so the signed recipient list is compared with the full
  ids in `m.mentions`, not with the header's `to`.

## Tools and validation

The repository requires Go >=1.26.8; the pinned module requires >=1.26.0.
No tool or native signer is installed. From this repository, with task-scoped temporary/cache directories:

| Tool | Entry point | Output |
| --- | --- | --- |
| Observe contract fixtures | `go test ./internal/envelopeobserve ./internal/config ./internal/cli -run 'Test(Observe|EnvelopeObserve|InvalidEnvelope|Roster|Protected|EnvelopeRoster)' -count=1` | Local test output; isolated fake carrier and public signed fixtures |
| Full race gate | `go test ./... -race -timeout 30m` | Test output; retain command log under `.temp/` |
| Static/build gates | `go vet ./...`; `go build ./...` | Diagnostics under `.temp/`; normal Go cache |

The 13 fixtures under `internal/envelopeobserve/testdata/` are unchanged from
swarma-envelope `065e25f69da8b28cdf3ea35d9fcd7fc4aacc4d5e`: Ed25519 positive,
empty body, wrong room/recipient, body-binding and bad-signature witnesses plus
BIP-340 positive/high-s and adjacent negative cases. They contain public test
records only. See their provenance and digests in `testdata/README.md`.
`TestObserveLabels/tampered-body` must fail when the verifier invocation is
replaced with an always-accept result; the producer handoff records the isolated
Go overlay control and its actual failing-test evidence. No planted source is
installed or committed. Native host-roster provisioning/adoption is separate.
