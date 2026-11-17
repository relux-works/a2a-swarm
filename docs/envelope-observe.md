# Receive-side envelope observation (M0)

Ordinary `a2a inbox` and `a2a read` label messages with exactly one result:

- `verified: <author>`: the protected host roster admits the signed author key
  and exact Matrix carrier, and the pinned `VerifyMessage` accepts. Version 1
  prints the sender MXID; version 2 prints the host's author label.
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
network lookup or roster writer on the observe path; the host operator's
separate tool is described in "Provisioning the roster". A label grants no action or
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

Both versions are bounded, duplicate-free UTF-8 JSON with exactly `version`,
`authors` and `facts`. The exact `version` selects the schema; a malformed
version 2 file never falls back to version 1.

### Version 1: one messaging account per author

Use version 1 when each author publishes through its own Matrix account. Its
schema, accepted sender spelling and output labels remain unchanged:

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

### Version 2: shared publishing accounts

Use version 2 when a bridge carries several authors' envelopes through one
Matrix account, or one author permits several carriers:

```json
{
  "version": "a2a-envelope-roster/2",
  "authors": [
    {"author_key": "ed25519:<canonical-public-key>", "author": "alpha", "publishers": ["@bridge:example.invalid"]},
    {"author_key": "bip340:<canonical-public-key>", "author": "beta", "publishers": ["@bridge:example.invalid"]}
  ],
  "facts": {}
}
```

These are unusable placeholder keys. `authors` is a closed list of 1..4096
objects, each with exactly `author_key`, `author` and `publishers`:

- `author_key`: the same canonical Ed25519 or BIP-340 public key ID as version 1.
  Each exact key occurs once. No key case folding or encoding normalization is
  applied; base64url payload case is significant. Escaped JSON spellings of the
  same key are duplicates, and noncanonical key encodings are refused.
- `author`: the host's logical author label, not an event display name. It uses
  the version 1 label character bound: 1..255 ASCII bytes from `!` through `~`,
  without spaces or controls. It need not itself be an MXID.
- `publishers`: 1..64 distinct exact full Matrix sender IDs, with the same
  admission rule as version 1. Membership is case-sensitive and never rewritten.
  One carrier may appear under many keys; a carrier admitted under one key is
  not thereby admitted under another.

Duplicate keys or members, case-variant or unknown member names, missing members,
empty/oversize lists, invalid labels, noncanonical keys and invalid MXIDs make
**the whole roster unavailable**. There is no partial salvage. The selector,
root ownership, ancestor protection and non-root reader rule are identical for
both versions.

Both roster versions use this observation order:

1. Run the pinned verifier with the actual delivery binding, including only the
   selected-room signed-post fallback described below.
2. If present, bind the carried attestation to signed `body.author_attestation`.
3. Check signed-post metadata and the room-addressed recipient list.
4. Only after those checks accept, resolve the author: version 1 by sender;
   version 2 by the signed `body.author` key, never the publishing account.

A known author's failed check takes precedence over a sender/key or publisher
mismatch. Version 2 resolves the final author decision as follows:

| Version 2 observation | Label |
| --- | --- |
| No envelope member | `unsigned (no envelope)` |
| No available protected roster | `unknown-author` |
| Signed author key absent from the roster | `unknown-author` |
| Known key, verifier or carried-attestation/metadata/recipient check refuses | The existing `invalid: <named reason>` |
| Known key, verifier accepts, exact sender absent from that key's publishers | `invalid: publisher_mismatch` |
| Known key, verifier accepts, exact sender present | `verified: <author>` using the host label |

Malformed carrier/envelope input retains its existing closed invalid label.
An unadmitted key missing from version 2 is classified as `unknown-author`
after the verifier reports `signer_unknown`; it is not a successful verification.
Other verification failures retain their closed invalid label. No roster entry
or publisher can turn unsigned or badly signed content into `verified`. For known keys, bad signatures and wrong room, recipient or text
bindings retain their verifier refusal, even if the publisher is also unlisted.
`publisher_mismatch` is a fixed local label, not peer-supplied text. The
version 1 sender/key mapping and ordinary labels are retained; its author
lookup also follows the mandatory content checks. This is
still observation only: neither version changes delivery, storage or receipts.

### Shared verifier facts and bounds

The file is at most 1 MiB and depth 16. The outer version 2 author list has the
4096-row bound; publisher lists have the 64-item bound. Verifier facts retain
the pinned codec's original bounds, including its 64-item array bound.
`facts` accepts only these members,
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

## Provisioning the roster

`a2a host roster build` is a host tool, run by the host operator as root. It
writes one of the roster schemas above from one input document and nothing
else. The default (and explicit `--version 1`) remains version 1; only
`--version 2` selects version 2. The flag and input version must agree.

**Trust statement.** The roster is the host operator's assertion: version 1
binds an MXID to a key; version 2 binds a key to a logical author label and its
allowed carriers. The tool does not verify an attestation of any key
(that arrives with the Keymaker codec), reads no key store, calls no signer
and uses no network. It checks the form of the input, not its truth.

The output always has `"facts": {}`: the input cannot carry verifier evidence.
With empty facts the reader can resolve configured authors, but strict
verification reports the missing evidence instead of `verified`. If an existing roster has non-empty `facts`, build refuses with
`existing_facts` before creating or writing a temporary file and leaves the
previous file byte-identical. There is no override flag; this tool must not
remove separately provisioned verifier evidence.

### Input: `a2a-roster-input/1`

One strict JSON document of at most 1 MiB, with exactly these members:

```json
{
  "version": "a2a-roster-input/1",
  "entries": [
    {"mxid": "@worker:example.invalid", "author_key": "ed25519:<canonical-public-key>", "source": "where this key was taken from"}
  ]
}
```

- `entries`: 1..4096 objects with exactly `mxid`, `author_key` and `source`.
- `mxid`: an exact full Matrix sender identifier as the reader admits it
  (`@local:server`, printable ASCII without spaces, at most 255 bytes). It is
  never lowercased or rewritten.
- `author_key`: a valid `ed25519:` or `bip340:` public key identifier.
- `source`: 0..64 printable ASCII bytes, the operator's own label for where
  the key came from. It is checked and not written to the roster, because the
  roster schema has no member for it; keep the input document as the record.

### Input: `a2a-roster-input/2`

Select this input explicitly with `a2a host roster build --version 2`. It is a
strict JSON object of at most 1 MiB and depth 16:

```json
{
  "version": "a2a-roster-input/2",
  "entries": [
    {"author_key": "ed25519:<canonical-public-key>", "author": "alpha", "publishers": ["@bridge:example.invalid"], "source": "public operator record"},
    {"author_key": "bip340:<canonical-public-key>", "author": "beta", "publishers": ["@bridge:example.invalid"], "source": "public operator record"}
  ]
}
```

`entries` contains 1..4096 closed objects with exactly `author_key`, `author`,
`publishers` and `source`. Key, label and publisher rules are the version 2
reader rules above; `source` retains the version 1 0..64 printable ASCII byte
rule and is not written. Duplicate keys are refused, while the same publisher
under different keys is allowed. Both writers retain the same protected
publication path and refuse existing nonempty facts, including in a version 2
roster with more than 64 authors. There is no override.

### Refusals and publication reports

Input refusals reject the whole document and write nothing. Every refusal
occurs before the rename and leaves an existing final file byte-identical;
any temporary created before a later refusal is removed. Refusals use
`host roster refused: <code>` with one of these fixed codes:

| Code | Meaning |
| --- | --- |
| `input_unreadable` | The `--from` file cannot be read |
| `input_malformed` | Not one strict UTF-8 JSON object within the bound, or a duplicate member name |
| `input_member_case`, `input_member_unknown`, `input_member_missing`, `input_member_type` | A case variant of a member, an unknown member, an absent member, a non-string value |
| `input_version` | Input version does not match the selected writer (`a2a-roster-input/1` by default, `/2` with `--version 2`) |
| `input_entries`, `input_entries_count` | `entries` is not an array of objects; it has 0 or more than 4096 items |
| `mxid_invalid` | Not a full sender identifier the reader admits |
| `mxid_duplicate`, `mxid_case_variant` | The same MXID twice; two MXIDs that differ only in letter case |
| `key_type`, `key_invalid` | A key type the reader does not admit; not a valid key identifier |
| `key_duplicate`, `author_invalid` | Version 2 repeats an author key or has an invalid author label |
| `publishers_invalid`, `publishers_count`, `publisher_duplicate` | Version 2 publishers is not an array, has 0 or more than 64 items, or repeats an exact sender |
| `source_invalid` | Longer than 64 bytes or not printable ASCII |
| `output_unencodable`, `selfcheck_failed`, `selfcheck_mismatch` | Built bytes are refused by the selected reader or load different key/label/publisher bindings; nothing is published |
| `out_path` | `--out` is not an absolute clean path |
| `not_root` | The caller cannot create a root-owned file; nothing is written |
| `out_unprotected` | A directory above `--out` is missing, a symlink, not root-owned, or group/other-writable |
| `out_symlink`, `out_not_regular`, `out_unreadable` | The final name is a symlink, not an ordinary file, or cannot be inspected or read |
| `existing_facts` | The existing roster has a non-empty facts object; nothing is written and there is no override |
| `result_unprotected` | The new file would not satisfy the reader's rule; the previous file is untouched |
| `publish_failed`, `publish_interrupted` | The temporary file could not be written or renamed; the previous file is untouched |

After the rename, these are **reports of a publication that happened**, not
input refusals. Both exit non-zero with `host roster published: <code>` and say
plainly that the new roster is at the final path. The temporary is gone and
there is no rollback:

| Code | Meaning |
| --- | --- |
| `published_unsynced` | The new roster is at the final path, but directory sync failed; durability is not established |
| `published_unprotected` | The new roster is at the final path, but the file or an ancestor is unprotected; the reader treats it as unavailable until the operator fixes the protection |

### Steps

1. As root, create the roster directory once. Every directory from `/` down
   must be a root-owned real directory without group/other write, and the
   agent user must be able to traverse it. The tool never creates a missing
   directory, never changes an owner and never changes the mode of anything
   that already exists.

   ```sh
   install -d -o root -g root -m 0755 /etc/a2a-swarm
   ```

2. Write the input document. For each entry, record in `source` where the
   public key was taken from. The document holds public keys only.

3. As root, build and publish. `--out` is the physical absolute path (no
   symlink at any level):

   ```sh
   a2a host roster build --from /root/roster-input.json --out /etc/a2a-swarm/author-roster.json
   ```

   For a shared account, use the version 2 input above and add `--version 2`:

   ```sh
   a2a host roster build --version 2 --from /root/roster-input.json --out /etc/a2a-swarm/author-roster.json
   ```

   The tool builds the roster in one fixed spelling (compact JSON, members in
   byte order; version 1 senders or version 2 author keys and publishers in
   byte order), loads those bytes through the selected reader's parser and
   requires the loaded bindings to equal the input. Version 2 compares every
   key, author label and publisher set, and requires empty output facts. Before
   writing anything, it refuses an existing roster with non-empty facts. It then creates a
   temporary file in the same directory (exclusive create, no symlink
   following, mode 0644), writes and syncs it, renames it over the final name,
   syncs the directory and applies the reader's protection rule to the final
   file and every ancestor again. Protection is also rechecked immediately
   before the rename, so a change already visible then leaves the previous
   roster untouched. Every refusal before the rename, including an injected
   interruption, leaves the previous final file byte-identical. A killed run
   before rename can leave a `.<name>.tmp-<hex>` file beside it; the reader
   never opens it.

   After rename, a directory-sync failure reports `published_unsynced`, and a
   protection failure reports `published_unprotected`. Both exit non-zero but
   explicitly say the new roster is already at the final path; no rollback
   occurs. For `published_unprotected`, the reader treats the file as
   unavailable until the operator fixes the protection.

4. As the agent user, select the roster in that agent's configuration root
   (`envelope-observe.json`, see above) and check what the reader loads:

   ```sh
   a2a host roster show --check
   ```

   `show` retains three columns: MXID, key type and the first 16 hex digits of
   SHA-256 over the public key identifier, tab-separated. Version 1 prints one
   line per sender; version 2 prints one line per publisher/key pair, including
   multiple rows for a shared carrier. Rows sort by MXID, then key type and
   fingerprint for version 2. It does not print source or logical author labels.
   Without `--check` an unavailable roster prints `roster unavailable` and exits 0;
   with `--check` an absent, unprotected or malformed roster exits non-zero.
   The reader refuses every roster for a root caller, so run `show` as the
   agent user, not as root.

## Tools and validation

The repository requires Go >=1.26.8; the pinned module requires >=1.26.0.
No tool or native signer is installed. From this repository, with task-scoped temporary/cache directories:

| Tool | Entry point | Output |
| --- | --- | --- |
| Observe contract fixtures | `go test ./internal/envelopeobserve ./internal/config ./internal/cli -run 'Test(Observe|EnvelopeObserve|InvalidEnvelope|Roster|Protected|EnvelopeRoster)' -count=1` | Local test output; isolated fake carrier and public signed fixtures |
| Host roster writer | `a2a host roster build --from FILE --out ABSOLUTE_PATH [--version 1\|2]`; `a2a host roster show [--check]` | The roster file at `--out`; sender, key type and fingerprint on stdout |
| Roster writer tests | `go test ./internal/envelopeobserve ./internal/cli -run 'Test(Roster|HostRoster|ObserverAuthors)' -count=1` | Local test output; run as a non-root user, ownership through the test seam |
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
