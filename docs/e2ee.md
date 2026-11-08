# E2EE rooms: first usable MVP (#308)

Status: proposed design, not activation approval. Source baseline:
`08976619cfd38d54a79c5c20f0060ea8b77ba4c6`. Revised against the offline S0 verdict
`../.out/review-s0-verdict.md` and the five follow-up findings in
`../.out/review-s0b-verdict.md`. This revision changes this document only and stays
uncommitted. No network, tests, builds or tmux are used for this revision.
Library sources unavailable locally remain **verify in U1**; proposed commands,
storage schemas and controls below are implementation requirements, not shipped APIs.
Section 11 decisions gate U1, not merely live activation. Recommendations are not approvals.

## 1. Outcome and boundary

Each a2a identity owns Matrix master, self-signing and user-signing keys and one
local device. The master signs the subordinate keys; the self-signing key signs
the device-key object. Its owner verifies the identity master in their own client.
The agent neither impersonates that client nor obtains the owner's private keys.

MVP encrypted application traffic is text in explicitly selected, invite-only DMs
with exactly two joined accounts: this identity and one pinned human principal.
General encrypted group rooms are deferred. Unexpected third members suspend all
application admission and sends; control-state reconciliation continues. Ordinary
plaintext project rooms keep their existing delivery behavior after context checks.
Encryption and device provenance grant no application authority: existing principal
scopes and protocol rules 13/14/24 still apply.

An accepted Megolm event is associated with the **session-originator device** and
its anchored cross-signing chain, under section 3's assumptions. It is not proof
that this device alone authored the ciphertext. Megolm keys are shared; another
key holder can forge a message under the session. The homeserver asserts the outer
sender MXID, which is not itself authenticated by Megolm plaintext. Two-member
scope reduces third-party key holders; it does not remove this protocol limitation.

Acceptance: one real identity sends to a joined, verified human peer in that DM,
reads a reply associated with the peer's verified session-originator device, and
visibly refuses an unverified device's reply without delivery effects. Own-message
sync echoes also pass with local outbound provenance. No plaintext fallback exists.

Excluded: encrypted media, backup/restore, secret sharing, forwarded/imported room
keys, automatic key recovery, device cloning, group-room application traffic and
encrypted projection/archive. Unsupported operations refuse before upload/export.
The server still sees membership, timing and ciphertext sizes. Plaintext at rest
in the mailbox and explicitly saved output requires owner approval Q5; this spec
does not silently authorize it. E2EE alone gives no local disk isolation.

Local sources checked: `internal/matrix/client.go`, `txn.go`, `envelope.go`;
`internal/cli/account.go`, `connection_registration.go`, `room.go`, `topics.go`,
`delivery.go`, `wait.go`, `inbox.go`, `project.go`, `json.go`, `credentials.go`,
`keychain.go`, `owner_dm*.go`, `admin_registration.go`, `wait_retry.go`, `runtime.go`;
`internal/matrix/admin_registration.go`; `internal/config/config.go`, `connections.go`,
`credentials.go`, `mailbox.go`, `consumer_lock_unix.go`; `docs/cli.md`,
`docs/protocol.md`, `docs/curator-build.md`, `README.md`, `go.mod`.
No authoritative issue bodies for #163/#173 were recovered; do not infer them.

## 2. Decision register and dependency gate

| Decision | Options | Recommendation / constraint |
| --- | --- | --- |
| Crypto engine | mautrix-go crypto + goolm; libolm/cgo; Rust FFI/sidecar; bespoke crypto | Admit a pinned mautrix-go/goolm closure only after U1 proves the contracts. No guessed release/API, cgo or bespoke Olm/Megolm. |
| Dependency delivery | Runtime download; warm cache; vendor on main; Curator build branch | Pin source dependencies on main; supply reviewed offline inputs; regenerate vendor only on `curator-build` per its existing flow. |
| Mutable store | SQL plus pure-Go SQLite; sealed snapshot adapter; memory only | Sealed snapshot behind a narrow adapter, conditional on U1 durability and latency evidence. Memory only loses ratchets/replay state. |
| Root custody | Backend bundle plus sealed account; broker/hardware; host-unlocked secrets | Q1, including file backend on macOS and Linux. |
| Account-password custody | Persist from registration; owner reset/UIA; admin bypass; new identity | Q2. Existing identities usually have no recoverable password; see section 3. |
| Owner ceremony / pin provisioning | Cross-user signature; interoperable SAS; host provisioning mechanism | Q3. A local trust boolean/TOFU is never evidence. |
| New cross-signed devices | Automatic admission; explicit approval per device | Q4; no automatic policy is silently selected. |
| Plaintext at rest | Existing private mailbox/output; seal both; ephemeral reads only | Q5; changes output/storage scope and U1/U4 design. |
| Room creation | Owner-provisioned room; new narrowly authorized agent DM exception | Q6; existing sole agent exception is unencrypted Owner DM S1. |
| Sync | SDK cursor; second cursor; shared coordinator | One coordinator and one version-fenced cursor, including projector refusal. |
| MVP audience | Groups; two-member DMs | Two-member DMs only, with the qualified provenance guarantee in section 3. |
| S1 migration | In-place; automatic adoption; separate room | Separate provisioned room. Preserve Owner DM S1 encryption refusal and #173 boundary. |

No download is attempted here. A later authorized U1 admission lane obtains exact
candidate/transitive sources, checks provenance, checksums, licenses, security
history and Go 1.25.5 compatibility, and records versions/hashes/tags in review.
With admitted inputs, compile the full CLI offline using `CGO_ENABLED=0 GOPROXY=off
GOSUMDB=off GOTOOLCHAIN=local GOENV=off`; prove no libolm/toolchain fetch. Targets
are supported macOS/Linux architectures, not mobile. goolm tag: **verify in U1**.
Main has no vendor tree. Refresh the append-only `curator-build` branch, run its
committed-tree equality gate and publish the full build OID through its pin flow.
A warm module cache alone is not reproducible admission.

U1 must prove the following from actual library sources, each **verify in U1**:

- Olm/Megolm interoperability, authenticated session-source metadata, message
  indices and typed integrity errors; outer device hints never suffice.
- Standard cross-signing generation/signatures, upload/query/claim, signature
  upload and UIA, including visibility of the chosen owner ceremony's evidence.
- External ingestion of to-device/device-list/OTK/fallback-key data without an
  SDK sync loop, automatic key requests, forwarding, backup or secret sharing.
- Account/session export or callbacks permitting atomic isolated preparation,
  including rollback of rejected speculative to-device processing.
- Policy hooks before prekey consumption/session mutation and before key sharing.
- Exact decrypted content bytes for later strict application verification. If
  absent, ordinary display may work; strict extraction stays unsupported.
- Bounded codecs, secret inventory and error mapping; admission/ratchet state can
  survive interruption without reprocessing an already committed Olm message.

If any load-bearing contract fails, U1 records the exact constraint and clean
alternatives. Do not add mocks/flags that conceal a protocol or platform limit.

## 3. Trust model, pins and cross-signing bootstrap

### Host-written trust artifact

Trust anchors are public master keys bound to full MXIDs and connection binding,
never names, invitations or a server response. Keep Owner DM `NAME.pin` unchanged.
Use a separate host-managed artifact outside the agent's writable config tree:

- macOS: `/Library/Application Support/a2a-swarm/e2ee-trust/B/CLASS/H/NAME.pin.json`.
- Linux: `/etc/a2a-swarm/e2ee-trust/B/CLASS/H/NAME.pin.json`.
- `B` is the selected connection-binding SHA256 hex digest, `H` is SHA256 hex of
  exact MXID bytes, `CLASS` is `agents` or `humans`, `NAME` passes existing validation.
  Legacy binding hashes a versioned tuple of endpoint, server, backend, class and
  full MXID; named binding uses `ConnectionBinding.Fingerprint()`. No token input.
- Root/admin owns each directory (0755) and regular file (0644); no agent write
  permission or ACL on file or ancestors, no symlinks. Public keys need integrity,
  not secrecy. A host using another service UID must prove equivalent exclusion.
- Strict JSON, no duplicate/unknown fields, size <=64 KiB:
  `{"v":1,"binding":"B","identity":"@agent:server","device_id":"D",
  "revision":1,"owner":"@owner:server","principals":[{"mxid":"@owner:server",
  "master_key_id":"ed25519:K","master_public_key":"BASE64",
  "master_sha256":"HEX","approved_devices":["OWNERDEVICE"]}]}`.
  Hash is SHA256 of decoded public-key bytes; key ID, algorithm, length and usage
  must agree with the fetched standard master-key object. Device approvals apply
  only under Q4's explicit-admission option; they do not replace signatures.
- The owner compares fingerprints in their trusted client and supplies public
  values to the host administrator outside the agent. The administrator renders,
  validates and atomically provisions the artifact, file fsync then rename then
  parent-directory fsync (including newly created ancestor entries). Like Owner
  DM pin provisioning, this is an owner/host step, never an a2a command.

The CLI never creates, edits, repairs or replaces this artifact, including on init,
refresh, recovery or rotation. U2 owns its reader, strict validation, safe open,
change detection and provisioning instructions; U1 owns binding/hash primitives.
Absent/invalid/unreadable pin blocks E2EE readiness. Same revision with changed
bytes, decreasing revision or conflicting master enters `changed`. Higher revision
requires host authority and invalidates cached trust; it never silently admits a
replacement root. Reopen and compare the artifact before each trusted operation.
This protects against an agent that can write its config; it cannot protect against
root/host compromise or a host that lets the agent modify its own pin directory.
The host principals MXID coverage list remains separate from cryptographic trust.

### Trust-root and device-approval lifecycle

The following is the proposed lifecycle for owner Q3/Q4 approval, not permission
for an agent to replace pins. Persist the last accepted artifact revision/digest
and each accepted principal root in sealed state. A host update uses exactly
accepted revision + 1, including recovery from `changed`; skipped revisions require
the host to supply and validate the intervening revisions. Quiesce trusted
operations for host provisioning, then resume under the crypto lock. No operation
may keep using a cached approval across an artifact revision/digest change.

For a legitimate principal master rotation, the owner independently compares the
new exact MXID/master fingerprint with that principal in a trusted client/outside
the agent. The host then provisions revision + 1 with that explicit replacement
root; Matrix discovery or signatures from the old root alone cannot authorize it.
U2 quarantines the changed principal and invalidates cached chains and the agent's
owner-verification evidence. Complete a fresh bounded query and validate the new
master/self-signing/device chains against the replacement pin. Repeat the selected
Q3 ceremony for the **existing agent master**: a fresh owner user-signing signature
(under the currently pinned owner master), or the approved SAS alternative with
retained master-binding evidence. If the rotating principal is the owner, only
their new pinned chain can supply this evidence. The principal's new devices must
also satisfy Q4. Do not regenerate the agent's keys to accommodate a peer rotation.

`changed` exits only after the authorized revision + 1 is durably accepted, all
applicable chains validate, and the renewed Q3 evidence is durably sealed; readiness
then returns to `owner-verified` subject to room/device gates. Until the ceremony
completes keep readiness `changed`, report the pending `awaiting-owner` ceremony
separately and block traffic. Repairing rollback/same-revision tampering requires a host
revision + 1, even when restoring the previous root, and renewed Q3 evidence.
Failed/partial queries leave the transition pending; init/refresh cannot clear it
by accepting server keys. No prior refusal is promoted and no old key is reimported.

With Q4 explicit admission, an owner-approved `approved_devices` addition/removal
is an atomic host revision + 1 update, bound to the same MXID and master. U2 reloads
it under crypto lock, invalidates cached eligibility and requires a complete fresh
query/valid chain for each added device before admission. Approval never overrides
changed keys, deletion/block tombstones or signature checks. Removal immediately
withholds affected pending output and sharing; U3 rotates affected outbound sessions
before further sends. Additions also rotate before sharing, giving a new device no
old session keys. U4 revalidates pending evidence; final refusals remain final.
With unchanged roots, this device-list-only update does not repeat Q3 or reset the
agent account. Under Q4 automatic admission, `approved_devices` grants no authority;
fresh valid cross-signing under the unchanged pin controls eligibility instead.
Q3 owns approval of the root re-ceremony; Q4 owns who approves device updates and
the selected admission mode. U2 implements both transitions; U3/U4 consume revisions.

### Password availability and UIA

Current registration behavior, not a proposed recovery mechanism:

| Identity / backend | Password source today | Cross-signing upload implication |
| --- | --- | --- |
| Legacy / file | `account.go` generates a random password; returns credentials without it; `config.WriteCredentials` also clears `Password`. No retained source. | Neither agent nor owner can retrieve it; password UIA needs an approved recovery route. |
| Legacy / Keychain | Same generator; only access token is written to the legacy service. No retained password item. | Keychain token access does not solve password UIA. |
| Named / file (macOS or Linux) | `connection_registration.go` keeps generated password only in memory; metadata has no password. | After registration it is unavailable, also to the owner. |
| Named / Keychain | Create-only item before registration: service `Connection.Store.PasswordService`, account exact full MXID. | U2 explicitly reads this selected item for password UIA, adds it to scrub registry and never copies it to credential JSON. |

`WriteCredentials` clearing is intentional current behavior; old stray password
fields are not a supported recovery source. An access token/registration token is
not the account password. UIA requirements depend on the server; flows/session
handling and upload API remain **verify in U1**.

Q2 offers: persist account passwords in the selected credential store from future
registration onward; owner-performed admin password reset or supported UIA step
for existing accounts; server-admin bypass if explicitly supported/authorized;
or recreate the identity. Persistence alone cannot recover an already discarded
password. An owner UIA step only helps if that flow actually supports their access;
an owner who never knew the password cannot simply type it. Admin reset may revoke
tokens/devices and requires a coordinated recovery plan. No automatic reset, bypass,
password custody expansion or identity replacement is authorized by this spec.
U2 implements only the approved per-backend route; unavailable UIA leaves keys
persisted locally and status `awaiting-uia`, with no upload-success claim.

### Bootstrap and session-originator evidence

Bind bootstrap to `(connection binding, class, full MXID, device ID)` and matching
credential metadata/authenticated `whoami`. Reuse a device only without conflicting
published keys. Missing local account with published device keys requires explicit
new-device recovery; never generate a replacement account under that device ID.
Persist roots, wrapping key and account before uploading any keys. Repeat uploads
of exact persisted public material after interruption; never implicitly rotate.
States: `absent -> local-ready -> awaiting-uia -> published -> awaiting-owner ->
owner-verified`; no-UIA servers skip `awaiting-uia`. Conflicts enter `changed`,
missing secrets `unavailable`. Self-signing one's own device is not owner approval.

For incoming session-originator eligibility require all applicable checks:

1. A host-anchored sender master, valid usage and exact MXID binding.
2. Valid master -> self-signing -> signed device chain and device self-signature.
3. Device ID, Ed25519 and Curve25519 keys match authenticated key-distribution
   provenance. Outer device ID/sender key and decrypted JSON claims are hints.
4. Successful full key query for that MXID, no known deletion/block/conflict and
   selected Q4 device policy. Query at activation, on device-list changes, before
   sends, and at most 60 seconds before new ingress/output. Failed/partial refresh
   blocks the operation; during sync it aborts the entire unprepared batch with
   cursor and durable outcomes unchanged, without final event refusals. Retry from
   the same committed `since`; only a completed query can establish device absence.
5. Key source is either `direct-olm` from that verified session-originator device,
   bound to room/session, or `own-outbound-session`: generated by this exact local
   account/device and durably recorded in its outbound session before sending.
   Own echoes require outer sender == local MXID and exact persisted outbound
   session/key/digest association. An outer own-MXID claim cannot create provenance.

Forwarded/imported/backup keys and another device's assertion do not prove origin.
Fresh queries provide a recent **observation**, not cryptographic freshness: a
malicious server can replay individually valid device keys and hide deletion.
Guarantee assumes honest sender-MXID binding by the server, no colluding/compromised
key holder and authentic host pins; it cannot promise detection of hidden revocation.
No stronger signature over application text is invented. Device-list observation
and timestamp appear in evidence; never label it a freshness proof.
Changed keys are quarantined. Known deletion/block immediately prevents new
sharing/admission/output; past shared keys cannot be recalled. A new cross-signed
device is admitted only under Q4's approved policy. Pending output is revalidated.

## 4. Crypto state, custody, locking and resource bounds

Paths use selected `Paths.Base`: legacy `<config>/crypto/NAME/`, named
`<config>/connections/ID/identities/CLASS/H/crypto/NAME/`. Store full identity,
device and binding in authenticated metadata. No cross-profile/name fallback.
Endpoint/backend/device changes need explicit migration; token rotation on the
same binding must not erase crypto state. Root custody is conditional on Q1.

| Artifact | Contents / protection |
| --- | --- |
| Root-secret bundle | Version/generation, three 32-byte cross-signing seeds, random 32-byte wrapping key; immutable per generation. |
| `<crypto-dir>/generation.json` | Strict versioned pointer containing `G`, namespace digest `N`, binding, full MXID and device; regular 0600 file in the 0700 crypto dir defined above, no symlinks. File fsync, create-only publish and directory/created-parent fsync **before** creating the root item/bundle. |
| `state.json` in crypto dir | Versioned AES-256-GCM envelope (key ID, fresh random nonce, ciphertext); AAD binds version, binding, MXID, device and generation. |
| Sealed payload | Account/private OTKs, Olm/Megolm sessions, trust/context revisions, replay windows/retirement tombstones, verdicts, outbox and prepared batch. Codec: **verify in U1**. |
| `refusals.jsonl` | Bounded compactable metadata journal with monotonic sequence; no content, ciphertext or secret material. |
| `crypto.lock` | Exclusive OS lock for mutation and crypto-dependent admission/output/send. |
| Active delivery generation | Versioned state/mailbox/output paths and legacy-path fence, described in section 5. |

### Exact Keychain item and file backend

Do not append an arbitrary suffix to the token service. Derive from a canonical
length-prefixed UTF-8 tuple `(v=1, endpoint, server, connection_id-or-legacy,
backend, token_service, password_service-or-empty, class, full_MXID)`:
`N = lowercase hex SHA256(tuple)`. Generation `G` is a 32-digit lowercase hex
random identifier persisted before root creation. E2EE service is
`works.relux.a2a.e2ee.v1.` + `N` (88 ASCII characters); account is `root.` + `G`
(37 characters). Both fit service regex `[A-Za-z0-9][A-Za-z0-9._:-]{0,199}` and
account bound 255. Tuple encoding uses 32-bit big-endian byte lengths, no delimiter
ambiguity; legacy token service is `works.relux.a2a.relux.works`.

U1 owns `generation.json` persistence and safe open; U2 owns init/recovery using
it. Init durably publishes the pointer before the create-only Keychain write, then
reads back and validates that exact `root.G` bundle before writing/fsyncing the
first sealed state. No public upload precedes durable account/state. A crash after
the Keychain create but before `state.json` leaves the pointer as the recovery
locator: reopen it, read only the derived service/account, validate the full bundle
binding/generation and reuse its seeds/wrapping key to finish local bootstrap.
Never generate another `G`, overwrite the item or enumerate/fall back to other
items on retry. Before first state, any generated account was unuploaded and can
be recreated only after checking that no conflicting device keys are published.
An existing first state is validated/reused, not replaced.

A crash with only the durable pointer and no bundle may resume create-only local
bootstrap under that same `G` only if no state or published device keys indicate
an existing account. Missing/invalid pointer with existing roots/state, a missing
root for existing state, binding mismatch or conflicting published keys refuses
with explicit new-device recovery; init never guesses a generation. File backend
uses the same pointer-first order for `secrets.json`. Explicit rotation/recovery
must durably record its old/new generation intent before switching this pointer,
retain the old locator while pending/outbox evidence needs it, and recover that
transition before normal operations; an ordinary retry cannot switch generations.

Before any lookup/write, reject equality of the E2EE service with **any** configured
token/password/registration-token service in the registry or the legacy token
service. Validate full identity binding on decoded readback; no alias fallback.
This reserves a namespace even if another profile is maliciously misconfigured.
No token/password item is reused, probed for an E2EE bundle or overwritten.

Encode the strict compact JSON bundle as `a2a-e2ee-root-v1:` + unpadded base64url
of UTF-8 JSON; each seed/key inside JSON is unpadded base64url, not raw binary.
Reject controls, noncanonical encoding, unknown/duplicate fields and wrong lengths.
Bound encoded bundle to 1024 bytes and actual fully quoted `security -i` line
including target path/newline to <4096 bytes. Refuse an overlong path/command.
Use existing probe, deadlines, cooldown, quoting and readback; no secret argv/env.
U1 measures actual size and readback. Every root write uses `createOnly=true`,
never `-U`; a duplicate is reconciled by readback equality or refused. Init never
silently replaces roots. Rotation is only through explicit owner-operated
`a2a e2ee rotate --new-device` with renewed verification, not init/refresh/retry.
That verb is separately gated and must preserve old pending/outbox evidence.

For file backend on **macOS and Linux**, roots live in separate regular 0600
`secrets.json` in the 0700 crypto dir; no Keychain calls. Credentials retain the
existing token policy. Q2 password persistence, if approved, uses a distinct
password-store item/file, not this root bundle or public connection metadata.
Sealed state plus adjacent wrapping key does not protect a stolen whole directory.
Keychain roots likewise do not make exported device pickles non-exportable keys.

### Secret handling and budgets

Before a loaded/generated secret can reach a library, subprocess or error path,
register it with `App.secrets` (`redact.Set`), including root-bundle text, base64/
base64url/hex key encodings, password/UIA material, account/session pickles, private
OTKs, room keys and serialized secret-bearing snapshots. The adapter inventories
library encodings in U1; register every emitted encoding at creation/load, not
by dumping arbitrary structures into errors. Never render library error strings,
crypto objects or decrypted bodies as diagnostics; map typed errors to fixed codes.
Errors that embed plaintext are suppressed, not merely passed through token scrub.
No secrets in argv/env/logs/metadata/Git/archive/owner messages; public keys and
signatures are public. Room keys leave only over eligible Olm. Go zeroing is best
effort; same-UID/root/RAM dumps remain outside this custody boundary.

Full snapshot unseal/reseal must fit the existing 5-second hook total budget.
U1 measures cold/warm p50/p95 at 1 MiB and the 64 MiB cap on macOS/Linux: local
crypto I/O target p95 <=500 ms, lock wait <=100 ms for hooks, leaving >=1 second
for cancellation/output. No measured claim exists yet. A process may reuse an
unsealed snapshot only while holding the lock with matching generation/revision;
no cross-process plaintext cache or daemon. No-change sync skips reseal. A hook
that cannot reserve the measured prepare+commit time skips before crypto mutation,
leaves cursor unchanged and uses bounded diagnostics; interactive consumers recover.
Honor existing 5-minute Keychain cooldown; never retry interactively from a hook.
If full-cap persistence cannot meet budget, U1 must change the store design before
admission rather than relax durability or extend hook deadlines silently.

### Bounded retention and recovery

Sealed payload hard cap is 64 MiB; reserve 8 MiB within it for one prepared batch
and recovery/compaction. Limit admitted DMs to 32, live inbound sessions to 128,
replay window to 4096 indices/session and recent verdicts to 8192 records or 7 days,
whichever prunes first. Pending mail/prepared/outbox evidence is never evicted.
For each session retain index -> event ID/digest inside its window and a monotonic
rejected-through floor. Eviction raises the floor; any older index is refused
`e2ee_replay_window_expired`, even previously unseen delayed history. Never reopen
an old index after pruning. A same-event reread is idempotent only with retained
mapping/evidence; otherwise it refuses, not re-delivers. State limits are local
policy, not claims that Matrix promises ordered delivery.

Retiring a session removes key material and records its ID/key hash as a no-reimport
tombstone; <=4096 retired sessions/room. Hitting that bound suspends new session
admission for that room, not all plaintext delivery. Closed-room tombstones count
within a 32-room lifetime budget for this device generation; no silent tombstone
expiry. Exhaustion requires explicit new-device recovery, not dropping protections.
At live-session pressure, retire least recently used non-pending sessions; refuse
new sessions if none can be safely retired. Trust-root conflicts remain sticky.

Journal cap: 4 MiB / 4096 records / 7 days; atomically compact oldest completed
entries while keeping monotonic sequence and oldest-retained sequence. Recent
accepted/refusal verdict eviction never erases replay floors/tombstones. Keep
per-room/reason/minute aggregate counters plus at most 10 sampled individual
refusals/minute/room and 100/minute/identity; overflow is a durable count-only
`e2ee_rate_limited` record per batch, not an unbounded record per hostile event.
Cap counter buckets at 256 and roll older buckets into totals. Rate limits and
sampling are independent of delivery/wake accounting. A known recent individual
refusal is sticky. Keep a fixed 64 KiB nonclearing refused-event digest Bloom filter
per admitted room (7 domain-separated SHA256 bit positions); pruning detail never
clears it. A hit without retained detail refuses `e2ee_refusal_history_blocked`.
False positives may require a new event; false negatives are forbidden. At 50% bit
occupancy suspend that room for close/new-device recovery, never reset the filter
on rejoin. This bounds memory without silently promoting aged-out refused history.

Before Olm prekey processing, reject outer senders outside the anchored trust set
or without a joined eligible DM, and sender-key hints not mapping to a freshly
queried eligible device. These hints only authorize a bounded attempt, never
acceptance. Limit attempts to 20/minute/peer, 100/minute/identity; persist counters.
Validate authenticated room/session after decrypting in isolated working state;
use a per-attempt savepoint so rejecting one payload preserves earlier accepted
working changes. Wrong-room/forwarded/secret payloads commit no OTK/session mutation.
If protocol requires decrypting to learn the room, this bounded attempt is the
limit of prefiltering; do not claim authenticated room knowledge before decryption.
OTK isolation/policy hooks are **verify in U1**; inability blocks engine admission.

`a2a e2ee store prune [--room R --close-room]` first completes prepared recovery,
compacts journals/verdicts, advances replay floors and retires eligible sessions;
it never drops pending mail, uncertain outbox or tombstones. A closed noisy room
refuses new keys/traffic. Status names capacity/pending blockers without content.
Full-store recovery: stop traffic from the noisy room, run prune, reconcile any
uncertain send, retry. If irreducible protected state fills the cap, use explicit
owner-operated `a2a e2ee recover --new-device`, reverify and establish fresh sessions;
keep old device data read-only for pending-mail handling. No automatic reset or
plaintext fallback. An individual attacker cannot grow the journal without bound.

Global lock order: optional outer owner-DM lock -> crypto lock -> optional existing
subscription-policy lock -> state lock -> mailbox mutation. Ordinary subscriptions
are serialized by the state lock today (`updateSubsWithPolicyBy`); do not invent a
second subscription mutex or acquire a policy lock from its state callback.
Runtime-consumer lifetime lock remains separate; E2EE coordinator owns the cursor
capability under crypto lock. Never acquire crypto/owner-DM locks from state/mailbox
callbacks; preserve inherited consumer FDs over exec. Section 7 defines output
revalidation before `lockedDeliver` and the revision check inside it.

## 5. Activation, encryption context and recoverable sync

### Version and cursor fence

An unknown JSON version field is insufficient: current state preserves unknown
fields and older sync ignores to-device. U4 implements a durable activation record
`<Paths.Base>/crypto/NAME/activation.json` with `v:1`, phase, generation,
`min_reader:1`, `min_writer:1`, `sync_owner:"e2ee-coordinator-v1"`, binding and
active paths `<Paths.Base>/state/e2ee-v1/NAME/{state.json,mailbox.jsonl,outputs/}`.
All current reads/writes require supported versions and generation. Only the
coordinator can update `NextBatch`, with cursor revision CAS under crypto lock;
post/read/inbox/projector cannot independently set or acknowledge a newer cursor.

Activation is a host-coordinated window, never implicit init: quiesce all consumers,
hooks, projector, admin-registration sync sessions and old processes; verify PIDs
exited and the launcher executable/build allowlist, then hold the consumer lock
and migrate in the global lock order. Existing
old binaries cannot learn a new fence retroactively. If host cannot exclude their
execution or prove quiescence, activation refuses; credentials must be isolated or
old access revoked by the owner before proceeding. Do not claim that an on-disk
version alone prevents a cached old process from acknowledging to-device traffic.

Durably copy and validate old state/mailbox/saved outputs to the active paths,
then replace the legacy `state/NAME.json` with a regular 0600 tombstone whose
`next_batch` is an object `{"requires":"e2ee-v1","generation":"G"}`. Current old
`State.NextBatch` string decoding fails; missing state must never serve as a fence.
Replace legacy mailbox and saved-output paths with deliberately non-mailbox/output
fence records; old binaries must fail parsing rather than render decrypted text.
New saved encrypted-origin frames use distinct E2EE-v1 begin/end markers and a
versioned evidence schema, so old `inbox show --from` rejects even an explicit
active-generation file path instead of ignoring extra evidence fields.
Move any `LastInbox` reference to the active generation. U4 audits every legacy
reader/writer and plants an old-version read/write attempt. New version dispatch
recognizes activation before opening legacy paths. Persist migration intents and
every copy/fence rename; incomplete activation resumes before any sync/output.
Only after all fences and generation paths are durable mark activation `active`.

`project.go` currently owns its own sync and writes `NextBatch`; while activation
is preparing/active it refuses **before** `/sync`, invite acceptance or state write,
with `e2ee_operation_unsupported`, even for plaintext projection. No shared-filter
alias exception. A second coordinator may fetch from committed `since`, but stale
revision loses CAS and must discard without decrypting/advancing. No SDK cursor.
Rollback may select only a binary understanding this active generation/schema;
otherwise stop operations. Never restore legacy paths/cursor or downgrade a room.

U4 also fences `issueAdminRegistrationToken` in `internal/cli/admin_registration.go`
and `Client.AdminRegistrationSync` in `internal/matrix/admin_registration.go`.
The latter currently parses only rooms/`next_batch`, discarding to-device data;
its initial no-`since` poll and subsequent polls must **never** use an identity
whose E2EE activation is preparing/active (including interrupted activation).
Refuse `e2ee_operation_unsupported` before any admin `/sync`, token-issue message,
registration effect or cursor write. Check the selected **admin session's** binding,
not the new account being registered. U4 places preflight in `cmdAdminRoomRegister`
and the admin branch of `registerNamedAgentUsing` (`connection_registration.go`)
before destination journal/password-store creation, as well as guarding the issue
helper and transport. Hold the activation exclusion across the admin-sync operation
so activation cannot race a
successful preflight. U4 supplies a checked transport capability so direct calls
cannot bypass the CLI fence; unknown/unreadable activation state fails closed.
Authoritatively established absence of activation permits the non-activated path.
Keep unencrypted registration available through a separate non-activated admin identity;
MVP does not retrofit this parser as a second coordinator.

### Establish room context

U4 owns context for every joined room, not just selected DMs. At activation fetch
a complete joined-room inventory and full state (encryption plus complete member
state) for each room. Initial sync requests full state with state filter types
`["m.room.encryption","m.room.member"]`, `lazy_load_members:false`; subsequent
sync uses the same types and timelines including encryption/member changes,
`m.room.encrypted`, messages and redactions. Preserve suppression of unrelated
presence/ephemeral/account data. Exact Matrix request/response support is owned
by U3, library ingestion is **verify in U1**.

Context is `unknown`, `plaintext-confirmed` or `encrypted-sticky`, with generation,
state event IDs, member snapshot and revision. Only a successful complete state
response demonstrating absence of encryption establishes plaintext; omitted
incremental state, invite preview, timeout/403 and partial responses never do.
Process state at the start of each sync batch, then membership/encryption events
in timeline order, then admit messages at the corresponding context revision.
Reconcile inventory/new joins on every sync; fetch full room state for unknown,
limited/gapped, rejoined or inconsistent rooms. Recheck plaintext-confirmed rooms
with a full state fetch at most 60 seconds old before admission; a missing expected
change or conflict sets unknown. Recent server observation is not Byzantine proof.

Once encryption is observed it is sticky across omissions/removal/leave/rejoin.
Ciphertext itself prevents treating the room as plaintext: set unknown and
reconcile before deciding. Unknown means no application delivery/send/thread
learning for that room; record `e2ee_context_unknown`. Unknown events are not later
automatically promoted; peer resends after reconciliation. Other confirmed rooms
continue delivering. State/control processing is separate, never message authority.
Activation does not skip encrypted events by taking a timeline-only position;
only a recovered committed batch may advance the cursor. Ordinary plaintext-room
positive controls must pass alongside unknown/encrypted injection refusals.

### Prepare, commit and recover

The shared coordinator covers `takePosition`, `syncNow`, wait/watch, hooks and
supervised consumers. Long-poll HTTP is outside locks and uses only committed
`since`. Extend `SyncResponse` for to-device, device-list changes, OTK/fallback
counts, state and leaves; no independent SDK sync. Recover before making another
sync request that might acknowledge a token. Under crypto lock reload binding,
activation, cursor/revisions and prepared record before inspecting a new response.

Complete production `/sync` inventory at the source baseline (disk `File.Sync`
calls are unrelated); **each caller is fenced by U4**:

| Current call site | Required activated-identity fence |
| --- | --- |
| `delivery.go`: `takePositionAll`, `syncNow`, `cmdWatch` | Coordinator only, including first position, inbox/hooks and watch; no direct token acknowledgement. |
| `wait_retry.go`: `waitSync` (wait/retry path) | Coordinator only; retries use committed `since` and recover prepared state first. |
| `runtime.go`: `runNative` | Coordinator only for the supervised native consumer. |
| `project.go`: `cmdProject` initial and polling calls | Preparing/active identity refuses before either call, as above. |
| `admin_registration.go`: `issueAdminRegistrationToken` baseline and polling calls | Preparing/active admin identity refuses before either `AdminRegistrationSync`, as above. |
| `matrix/client.go`: `Client.Sync`; `matrix/admin_registration.go`: `Client.AdminRegistrationSync` | These are the two HTTP `/sync` implementations; require the corresponding coordinator or non-activated-admin capability. Neither is an unguarded escape hatch. |

No other production HTTP `/sync` implementation/caller was found in the local
code inventory. These are implementation requirements, not claims of shipped fences.

Recovery order has precedence over normal processing:

1. If a sealed prepared record exists for `(generation,since,next,batch_digest)`,
   **do not process that response's to-device/device-list/OTK data again**. Use its
   already-ratcheted snapshot, accepted outcomes, refusals and replay reservations.
   Recovery short-circuits normal steps A-D below, even for to-device-only batches.
2. If cursor is `since`, resume E-F idempotently; if it is `next` with matching
   committed batch ID/revision, verify durable artifacts and finish cleanup.
   An unrelated/newer cursor is corruption/conflict, not permission to overwrite.
3. Revalidate trust/context/mute eligibility without decrypting again. Changed
   trust may withhold an outcome; it never regenerates a room key or reruns Olm.
4. Deduplicate journal by refusal identity and mailbox by event ID/admission digest;
   complete journal/mailbox/subscription/state commit and required fsyncs below.
5. Only after durable cursor/state and matching mailbox/journal readback may a
   new sealed snapshot omit the prepared record; fsync that replacement and its
   directory. Then release locks and permit a sync acknowledging `next`.

For a fresh response with current `since` and no prepared record:

A. Strictly validate bounded control JSON, snapshot revisions; reject stale cursor
   before any engine mutation. Reserve preparation space and budget accepted content
   plus evidence to fit the 8 MiB prepared allowance; excess candidates receive
   rate-limited aggregate refusals before decrypt. Work on an isolated account copy.
B. Mark changed/left users stale, perform bounded key queries and context
   reconciliation. Every required `keys/query` must complete for all requested
   MXIDs/servers, with bounded valid master/self-signing/device results or explicit
   complete absence. Timeout, transport/server failure, malformed/truncated or
   partial response (including per-server failures/omissions) aborts the whole
   unprepared batch as `e2ee_key_query_unavailable`. Discard all working mutations,
   staged verdicts/refusals/aggregates and intents; no journal/history-filter,
   mailbox, replay, OTK, outbox or cursor change. Do not prepare/commit a subset
   or acknowledge `next`; retry from committed `since`. No partial query confers
   trust or a final event refusal. Apply section 4 prefilters only with required
   eligibility observations available. Prepared recovery still takes precedence.
C. Process eligible to-device payloads into the working copy; no auto-forward,
   backup/secret import or unverified key sharing. Stage OTK uploads/shares as
   durable outbox intents; no network crypto effect before preparation.
D. Gate all timeline/backfill candidates before relevance/thread/principal checks.
   Atomically persist and fsync one sealed snapshot containing account mutations,
   exact accepted content/evidence, refusals/aggregate intents, replay reservations,
   `(since,next,batch_digest)` and `prepared` status. No separately persisted Olm
   ratchet ahead of this record. Crash before prepare discards all working changes.
E. Acquire optional policy/state/mailbox locks in global order. Recheck cursor,
   generation, trust/context/mute revisions; current mute reclassifies prepared
   accepted events without decryption. Trust/context changes rerun eligibility.
   Preserve bounded three-attempt mute deferral and first-sync recovery behavior.
F. Persist refusal journal first, then eligible mailbox adds, then learned threads/
   one-shot closures and delivery state with `NextBatch=next`, batch ID and matching
   trust/context revision. Subscription writes retain account-data-before-state
   ordering and idempotent reconciliation. No network read receipts. Cleanup only
   in recovery step 5. A no-change response needs no ratchet reseal but still a
   durable cursor transaction; refusal-only/to-device-only batches are not skipped.

Every boundary artifact has explicit durability, not just rename/readback:

| Artifact / operation | Required persistence before moving on |
| --- | --- |
| New roots/file bundle, activation intents, active-path migration copies | File fsync; create-only publish/rename; containing directory fsync; fsync parents for new directories. Keychain: successful create-only readback before account/public upload. |
| `generation.json` | Regular 0600 pointer: temp-file fsync, create-only publish for init, crypto-directory and newly created parent fsyncs before Keychain/file root creation. Explicit generation switch: durable intent, temp fsync, atomic rename, directory fsync; recovery retains the prior locator until safe retirement. |
| Sealed prepare, outbox mutation, committed snapshot | Same-directory 0600 temp, file fsync, rename, crypto-directory fsync. |
| Refusal journal / mailbox append | Fsync file; if newly created, fsync parent before cursor. Rewrite/compaction: temp fsync, rename, directory fsync. |
| Delivery state/cursor, subscription recovery metadata, display watermark, legacy fences | Temp-file fsync, rename and actual containing-directory fsync, even when outside crypto dir. |
| Prepared cleanup, retired artifacts | Durable replacement/unlink plus parent-directory fsync; never before cursor directory durability. |

Existing `writeSecretAtomic` lacks directory fsync; U1/U4 must extend the actual
E2EE boundary writers, including state save, not merely add it to crypto storage.
Directory fsync failure is a failed commit; readback is not a substitute. Torn
journal/mailbox tails are repaired only from prepared intents with bounded strict
validation; unrelated corruption refuses. Full disk leaves prepared record/cursor
recoverable. Reserve recovery space before mutation. Power-loss fixtures cover
each file/directory boundary and prove no acknowledged Olm key disappears.

Refusals are not mail or synthetic gaps. Cursor coverage requires durable refusal
or bounded aggregate accounting. They cannot learn threads, match/close subs,
mark shown/received, change unread counts, trigger a doorbell or reset wait time.
Limited/post-join history keeps existing bounds/gaps and includes ciphertext before
relevance filtering. Missing old keys refuse; no plaintext retry. Admin/archive
exclusions remain in force before exposing content.

## 6. DM lifecycle and outbound behavior

Encrypted creation (if Q6 permits it) sets `m.room.encryption` algorithm
`m.megolm.v1.aes-sha2` in initial state, private_chat, is_direct, invite-only,
joined history and the exact named principal invite. is_direct/title are not
proof of audience. Recommended path is an owner-provisioned separate room.
Do not add general `room new --encrypted` in MVP or change S1 binding implicitly.

`join R --require-encrypted --peer @principal:server` is proposed admission, not
current `cmdJoin` behavior (which joins then immediately subscribes). Invite-only
state may be unreadable before joining. Precheck whatever is visible, join with
no subscription/delivery admission, reconcile complete state, encryption and
exact self/peer membership, then register the selected DM and subscribe. If peer
is still invited, keep `awaiting-peer`, no key sharing, topics or text sends.
If room is plaintext/wrong audience, refuse and automatically leave **only if this
invocation newly joined it**; report whether leave succeeded. Failed leave leaves
quarantined membership and no subscription, with explicit `leave R` recovery.
Already-joined rooms are quarantined on refusal but never silently left. Unknown
state stays quarantined and requires explicit retry; no plaintext conversion.

Before every send require owner-verified readiness, complete fresh context with
exactly self and intended peer **joined**, and at least one eligible device for
peer. Invited/left/banned peer is `e2ee_peer_not_joined`; no text or session share.
Query complete device sets; partial lookup blocks. Share only to eligible devices
under Q4, including own eligible devices; unverified devices get no key. Send-side
`ready --room R --peer P` verifies these conditions without payload/network share;
every actual send rechecks, so readiness is not a lasting authorization token.

Explicit `leave R` first fences new sends/admission and suspends that room, retires
outbound session/sharing, then leaves and unsubscribes after successful server ACK.
On failure remain locally suspended and report it; pending mail is preserved,
withheld until revalidation. Observed peer leave/third member similarly suspends
traffic. Rejoin reconciles full state and uses new outbound keys, not old history.

Rotate outbound sessions on membership/eligible-device-set/revocation/root change
and room rotation limits. New recipients get a fresh session only; old keys cannot
be recalled. Library Olm establishment/rotation APIs: **verify in U1**.
Persist exact ciphertext, transaction ID, recipient/context revisions, outbound
provenance and ratchet before send. ACK loss retries identical bytes/transaction;
eligibility change holds the old attempt for explicit reconciliation, never silently
re-encrypts/resends it. Keep existing strict ACK, redirect, timeout/cancel rules.

All send paths including `topic new`, topic/reply resolution, `post`, raw
`SendMessageTxn`, envelope carriage and automatic notices use policy or refuse.
Root/topic reads use U3's accepted-event contract, not outer ciphertext relations.
`attach`, files download/export, `post --voice` (before even the text post), archive,
projector and unsupported administrative paths refuse encrypted content before
upload/plaintext send/export. U3 owns outbound/media audit; U4 owns inbound/export
and projector audit. A room once encrypted never enables plaintext fallback.

## 7. Ingress, mailbox evidence and deterministic refusal tree

The internal accepted-event type cannot be constructed by wire JSON unmarshal.
It includes exact content plus authenticated local provenance; an event's alleged
`crypto` field has no authority. U3 defines this type before U4 consumes it.
Megolm plaintext must bind inner room/type to authenticated session provenance;
outer sender consistency is checked under section 3's stated server assumption,
not misrepresented as an inner sender signature. No decryption-only acceptance.

U4 adds explicit versioned `MailEntry.CryptoEvidence` storage, separate from Event:
`v`, binding/generation, event/digest, admission ID, trust/context revisions,
originator MXID/device/public-key fingerprints, key source, session/index,
validated chain and observation time. Bind it with wrapping-key HMAC over exact
event bytes + canonical evidence fields + entry identity. Full validated public
chain can stay in sealed referenced records pinned while the mail is pending.
Mailbox reload verifies MAC/binding; `itemOf` must require the reconstructed
accepted-event type before rendering decrypted content. Missing/forged/unknown
evidence with encrypted origin withholds output and makes no receipt. Q5 chooses
plaintext body storage or a sealed alternative before implementation.

Compaction preserves evidence and pinned records for every pending entry. Receipt
may release pins only after durable receipt; bounded replay floors remain. Redaction
removes payload/evidence rendering but retains minimal dedup/safety state. Version
fences prevent old readers ignoring evidence and printing plaintext. Saved output
uses the same generation and evidence checks; it grants no new delivery authority.

For inbox/showNew/wait/watch/ack: acquire crypto lock first, load/revalidate trust,
context and candidate admissions, capture revision and entry digests, then enter
`lockedDeliver`. Inside its state/mailbox callback recheck generation, trust/context
revision, mute/subscription revision, current pending entries and evidence digests.
Mismatch releases **all** locks and retries from crypto, bounded to three attempts;
never acquire crypto or do key refresh inside that callback. Hold crypto through
print and receipt/shown update, so cooperating trust writers cannot interleave.
Reopen/check host pin digest immediately before output; out-of-process host changes
require the host's coordinated window. Withheld mail is neither deleted nor acked.

### Decision tree

First failed stage wins; evaluate later stages only with the required evidence.
Typed engine errors, not error strings, distinguish integrity from missing keys
(**verify in U1**); indistinguishable failures use `e2ee_undecryptable` consistently.
Historical verdict reuse applies only within section 4's retained evidence window.

| Stage | Deterministic condition -> reason |
| --- | --- |
| 0: wire shape | Duplicate keys, wrong types, oversized/invalid required fields -> `e2ee_malformed_event`. No claimed authenticated device. |
| 0a: refusal history | Retained exact refusal -> its saved reason; room digest-filter hit without detail -> `e2ee_refusal_history_blocked`. No re-decryption or automatic promotion. |
| 1: context | Unknown context -> `e2ee_context_unknown`; encrypted room outside selected two-member DM scope -> `e2ee_room_scope_unsupported`; ciphertext in complete plaintext-confirmed context -> `e2ee_ciphertext_in_plaintext_room`. Reconcile first if ciphertext newly contradicts cached context. |
| 2: outer kind | Plaintext application payload in encrypted context -> `e2ee_plaintext_in_encrypted_room`; unsupported declared encryption algorithm -> `e2ee_unsupported_algorithm`. Control events use their separate validators, never application admission. |
| 3: observed device eligibility | Known changed pinned/root/device key -> `e2ee_device_changed`; known deletion/block tombstone -> `e2ee_device_unverified`; no device mapping after a completed successful bounded query -> `e2ee_device_unknown`; root unanchored or device disallowed by Q4 -> `e2ee_device_unverified`. Tombstone wins over absence from the current list. Failed/partial required query is an operation/batch abort, never this event verdict. |
| 4: chain | Master invalid usage/MXID/signature -> `e2ee_ciphertext_invalid`; absent self-signing key or absent required self-signing signature -> `e2ee_self_signing_missing`; present but invalid master/self/device signature -> `e2ee_ciphertext_invalid`. Missing is distinguished from forged; changed known keys already failed stage 3. |
| 5: key availability / origin | Retired session/index below floor -> `e2ee_replay_window_expired`; no usable session/key -> `e2ee_undecryptable`; available forwarded/imported/backup or source lacking direct-Olm/own-outbound evidence -> `e2ee_key_provenance_unverified`. |
| 6: decrypt / bindings | Typed cryptographic integrity failure -> `e2ee_ciphertext_invalid`; unavailable/indistinguishable decrypt failure -> `e2ee_undecryptable`; valid decrypt with room/session/outer-originator conflict -> `e2ee_sender_mismatch`; invalid decrypted JSON/structure -> `e2ee_malformed_event`. |
| 7: inner payload | Inner event not `m.room.message`, msgtype not `m.text`, attachment/media/secret payload or edit relation `m.replace` -> `e2ee_content_unsupported`. Valid text/topic/thread relations continue through existing structural validation. |
| 8: replay | Same retained event ID + identical digest/index -> prior outcome, no duplicate mail; same index/different event ID or event ID/different digest -> `e2ee_replay`; otherwise reserve mapping durably and return accepted-event. |

Stage 3 may use a signed sender-key mapping for lookup, but still labels it claimed
until direct session provenance succeeds. A valid chain with a ciphertext failure
can show chain verified while authenticated originator is null. A wire hint never
selects an invented authenticated device. Multi-failure fixtures pin precedence.

To-device has a separate deterministic prefix: malformed -> `e2ee_malformed_event`;
outer sender outside trust/joined-peer set -> `e2ee_sender_not_eligible`; exceeded
budget -> `e2ee_rate_limited`; algorithm -> `e2ee_unsupported_algorithm`; then
stages 3-4, Olm decrypt typed errors, authenticated sender/recipient/key conflict ->
`e2ee_sender_mismatch`; unsupported key/forwarding/secret type ->
`e2ee_key_provenance_unverified`; room outside reconciled joined DM ->
`e2ee_sender_not_eligible`; otherwise accept key into isolated prepared state.
No persistent account/OTK/session mutation on any rejected attempt. Failure to
persist safety accounting aborts the batch; it is not an incoming event refusal.

To-device events need no `event_id`: record `source:"to-device"`, nullable room/
event ID and `refusal_id = SHA256(length-prefixed(binding,observed sender,type,
exact raw content bytes))`. Malformed fields use empty components plus raw event
digest. Prepared `(batch_digest,ordinal)` locates repetitions for recovery; the
stable digest deduplicates exact repeats across batches. Room-event identity is
`(room,event_id,ciphertext_digest)`; malformed/missing IDs use a raw digest too.
Journals persist digests only, never the raw event/content. Rate aggregation retains
counts and sampled refusal IDs; aged/unsampled event lookup reports unavailable.

Within retention, refusal is final for event/digest in automatic consumers; a later
verification cannot promote refused history or close subscriptions. Resend a new
event after repair. Store/command failures are separate fixed codes:
`e2ee_store_unavailable`, `e2ee_store_invalid`, `e2ee_store_full`, `e2ee_busy`,
`e2ee_version_unsupported`, `e2ee_owner_verification_required`,
`e2ee_recipient_unverified`, `e2ee_peer_not_joined`, `e2ee_operation_unsupported`,
`e2ee_key_query_unavailable` (retryable query failure, never a final refusal).
Preserve pending mail/receipts/cursor on failure; no alternate backend fallback.

## 8. CLI, refusal presentation and exact acceptance path

All new E2EE verbs below are proposed; existing post/topic/read syntax is retained.
Every command uses explicit selected identity/profile; no owner impersonation.

| Surface | Contract |
| --- | --- |
| `e2ee init`; `e2ee status --json`; `e2ee refresh --json` | Idempotent local bootstrap, public readiness, bounded public-chain refresh. No pin writing, trust override or implicit rotation. Status includes generation/version/context/capacity and awaiting UIA/owner/peer. |
| `e2ee activate` | Explicit coordinated activation/fence/context migration, after owner gates and host quiescence; no payload send. |
| `e2ee ready --room R --peer P --json` | Validate current joined audience, context and eligible peer devices; readiness only. |
| `join R --require-encrypted --peer P`; `leave R` | Quarantined join then verified encrypted-context admission; leave semantics in section 6. |
| `dm new --to P --encrypted` | Conditional on Q6 approval and authoritative #173 review; otherwise absent. No agent creation exception is assumed. |
| `topic new R SLUG TITLE --body TEXT`; `post R --topic SLUG TEXT` | Encrypt root and text after audience/recipient checks; decrypt/validate roots when resolving topics. `post --re EVENT` similarly uses validated target. |
| `e2ee refusals [--room R] [--event E | --refusal-id D] [--json]` | Read-only latest 50 retained metadata records by default, max 200; no receipt, display-watermark advance or network sync. |
| `e2ee events [--room R] [--event E] --json` | Bounded retained verdict metadata, including irrelevant/muted accepted events; retention limits explicit. |
| `e2ee store prune`; `e2ee recover --new-device`; `e2ee rotate --new-device` | Explicit maintenance/recovery/rotation contracts in section 4; never automatic destructive repair. |

Accepted JSON retains current event/relevance types and adds local `crypto`:
`encrypted:true`, full `sender_mxid`, `session_originator_device_id`, public
Ed25519/Curve25519 fingerprints, master key ID/fingerprint, chain verified,
trust/context revision, `device_list_observed_at`, `verified_at`, and
`key_source:"direct-olm"|"own-outbound-session"`. If compatibility requires
`sender_device_id`, document it as an originator alias, not unique authorship.
Text adds originator/chain label inside existing untrusted-content framing.

Refusal JSON: `type:"e2ee_refusal"`, monotonic `sequence`, `refusal_id`, source,
nullable `room_id`/`event_id`, observed full sender, nullable authenticated originator,
optional claimed device, cross-signing state, fixed reason and trust/time. No body,
excerpt, attachment/header or raw crypto fields. Plaintext injections have no device.

Refusal-display state is distinct from mail receipts: sealed per-consumer map
`diagnostics[consumer_key]={last_sequence,last_seen}`, max 64 consumers, LRU expiry
30 days. Key is hash(binding + generation + native session/binding identity +
channel `inbox|wait-watch|bound-diagnostics`); no native session uses persistent
`interactive:identity:channel`. Hook ingestion never advances display watermarks.
Inbox/wait drain retained sequence > watermark to stderr (NDJSON for `--json`),
max 50/invocation; after each successful diagnostic write persist watermark using
section 5 durability. Print-before-save crash can repeat a diagnostic, never lose
mail. Reclaimed/expired watermark starts at oldest retained record; report one
fixed `diagnostics_pruned` notice if history fell out of retention. Enumeration is
per consumer, not a global "new" bit. Bound-runtime diagnostics stay local and
are never submitted as model/user input. Direct `refusals` inspection is repeatable.

Inbox stdout contains accepted mail only and receipts exactly successful prints;
wait/watch only mark shown. Refusals do not consume limit, satisfy predicates,
reset timeout or trigger wakes. Hooks remain admitted-mail count-only notices.
Read uses the same gate without cursor/mailbox/shown/receipt effects: explicit
`--event` refusal exits 1 with metadata, usage errors 2. Range reads preserve order
and exit 1 if any selected event refuses. No false complete/empty thread claim:
resolve authenticated inner relations within existing bounds or explicitly refuse.
Strict `read --verification-input` / `--envelope-input` retain original-response
contracts and refuse encrypted selections with `e2ee_operation_unsupported`.

### Acceptance runbook (future authorized U5, not run for this revision)

Zero here means no agent identity/crypto state; host routing/registration admission
must first be provisioned by its owner. This sequence deliberately uses the existing
named-Keychain password route on macOS; file/legacy routes require resolved Q2.
Owner approves Q1-Q6, supplies a named `PROFILE`, host registration admission and
principal public pins. After registration the owner creates a **separate** encrypted
invite-only DM, joins and invites the returned agent MXID. No S1 binding is adopted.
Host provisions section 3's pin for the registration's actual device, and performs
the rollout quiescence/allowlist. If registration-generated device is not yet known,
provision after `whoami`, before init. `R` is that exact room ID, `P` the full pinned
principal MXID, `NAME` the owner's authorized new agent name. `V` and `E` below are
the verified and unverified replies' exact event IDs supplied by the peer's client.
These are public values. Execute one command at a time in a dedicated acceptance
shell; do not paste through human gates. Every command must exit 0 **and** satisfy
its row below before executing the next line. A nonzero exit stops the shell;
a missing observation or incomplete human gate stops the procedure even at exit 0.

```sh
# Existing commands; PROFILE's named Keychain routing is already host-provisioned.
a2a --profile "$PROFILE" register "$NAME" --authorize-registration || exit $?
a2a --profile "$PROFILE" --as "$NAME" whoami || exit $?
# Owner now creates/joins the separate encrypted DM and invites the returned MXID.
# Host establishes delivery coverage (separate from cryptographic public pins).
a2a --profile "$PROFILE" --as "$NAME" host principals set "$P" || exit $?
# Host provisions public pin bound to returned MXID/device; agent never writes it.
a2a --profile "$PROFILE" --as "$NAME" e2ee init || exit $?
a2a --profile "$PROFILE" --as "$NAME" e2ee status --json || exit $?
# Owner compares/signs identity master in own client (approved Q3 ceremony).
a2a --profile "$PROFILE" --as "$NAME" e2ee refresh --json || exit $?
# Require owner-verified; host has quiesced older launchers/consumers.
a2a --profile "$PROFILE" --as "$NAME" e2ee activate || exit $?
a2a --profile "$PROFILE" --as "$NAME" join "$R" --require-encrypted --peer "$P" || exit $?
a2a --profile "$PROFILE" --as "$NAME" e2ee ready --room "$R" --peer "$P" --json || exit $?
# Proceed only when peer joined + eligible verified device; ready must exit 0.
a2a --profile "$PROFILE" --as "$NAME" topic new "$R" e2ee-acceptance "E2EE acceptance" --to "$P" --kind notice --body "Verified encrypted opener" || exit $?
a2a --profile "$PROFILE" --as "$NAME" post "$R" --topic e2ee-acceptance --to "$P" --kind notice "Verified encrypted text" || exit $?
# Peer replies to opener from verified device in own client and supplies V.
# Never borrow their token. Ingest to-device keys BEFORE the history read.
a2a --profile "$PROFILE" --as "$NAME" inbox --json || exit $?
# Gate: V was admitted/printed with direct-Olm verified-originator evidence.
a2a --profile "$PROFILE" --as "$NAME" read "$R" --event "$V" --json || exit $?
# In authorized disposable test, peer sends fresh-session unsigned/unapproved reply E.
a2a --profile "$PROFILE" --as "$NAME" inbox --json || exit $?
a2a --profile "$PROFILE" --as "$NAME" e2ee refusals --room "$R" --event "$E" --json || exit $?
```

Line-by-line behavior check against the local commands (E2EE additions remain
proposed U2-U4 work, not current capabilities):

| Command / preceding human step, in execution order | Required gate and current behavior |
| --- | --- |
| `register --authorize-registration` | Existing named registration admission; require returned full MXID/device and saved credentials. Use only the owner's admitted new identity. |
| `whoami`; owner room creation/invite | Existing authenticated user/device lookup; require equality with registration. Owner joins the separate encrypted room and invites that exact MXID before proceeding. |
| `host principals set P`; host pin provisioning | Existing command writes delivery coverage only; require exact full MXID. Separately require root-owned pin for the matching binding/device; this command cannot create cryptographic trust. |
| `e2ee init` | Proposed bootstrap must persist pointer/roots/account before upload; require publication complete. `awaiting-uia` stops here for the approved UIA route, never counts as success. |
| `e2ee status --json`; owner ceremony | Proposed status must match identity/device/generation and expose the public master. `awaiting-owner` is expected here, not a failed gate; owner must complete Q3 before refresh. |
| `e2ee refresh --json`; host rollout | Require completed refresh, retained Q3 evidence and `owner-verified`. Host must then prove old consumers, projector and admin-sync sessions quiesced and launchers fenced. |
| `e2ee activate` | Proposed durable activation/context migration; require `active` generation and archive exclusion before payloads. It does not join or authorize a peer. |
| `join R --require-encrypted --peer P` | Proposed extension of current join-then-subscribe command: require reconciled encrypted two-member scope and admitted subscription; `awaiting-peer`/unknown stops here. |
| `e2ee ready --room R --peer P --json` | Proposed check must confirm peer joined and an eligible device; require exit 0 and readiness. Actual sends recheck eligibility. |
| `topic new ... --to P` | Existing topic syntax creates the root; U3 encrypts it. Require strict ACK/event ID and `own-outbound-session` provenance. |
| `post ... --topic e2ee-acceptance --to P` | Existing post requires the root/slug already present; U3 must resolve its decrypted content. Require strict ACK/event ID; full-MXID recipient remains explicit. |
| Peer verified reply `V`; first `inbox --json` | Current inbox calls `startDelivery` then `syncNow`, prints pending mail and receipts successful prints. U4 must first ingest/commit to-device room keys through the coordinator. Require this exact `V` printed with verified direct-Olm evidence; empty exit-0 inbox is insufficient. No read follows if the key/reply is absent or refused. |
| `read R --event V --json` | Current read performs event/history GETs, **no sync** and no receipt. U3 may decrypt using the already ingested key; require accepted exact `V` and retained evidence after inbox receipt. Single-event selection avoids unrelated refused history making a range read fail. |
| Peer unverified fresh-session reply `E`; second `inbox --json` | Require completed coordinated ingest and no `E` mail/effects. The intended refusal is a diagnostic, not a command failure: inbox exits 0 on successful processing, prints refusals on stderr and only accepted mail on stdout. Transport/store/query failure is a failed gate. |
| `e2ee refusals --room R --event E --json` | Proposed read-only lookup must find exact retained `E`, without sync/receipt. Pin the expected reason before injection: Q4 explicit mode uses a known but unapproved device -> stage-3 `e2ee_device_unverified`; Q4 automatic mode uses a known device lacking its required self-signing signature -> stage-4 `e2ee_self_signing_missing`. Missing record, wrong reason or forbidden effects fails acceptance even at exit 0. |

Do not execute lines after a failed gate. Stop, repair the cause and explicitly
resume the failed step; a gate cannot be skipped to reach a later diagnostic.
If a reply was finally refused, use a new event after repair rather than promoting
it. `topic new` establishes the root required by `post`; slug lookup must decrypt
that root through U3 before use. The peer's
unverified reply uses a fresh session never shared by an eligible device; otherwise
Megolm cannot prove which key holder authored it. Exact single-event diagnostic:
`a2a --profile "$PROFILE" --as "$NAME" e2ee refusals --room "$R" --event "$E" --json`.
Record metadata/effects and own-outbound echo evidence, never plaintext or secrets.
Acceptance requires the final lookup visibly report the intended retained refusal and
inbox have no corresponding mail/receipt/thread/subscription/wake effects. A refusal
from a hook-only sync must later be displayed to inbox's independent consumer.

## 9. Owner ceremony and S1 compatibility

Host provisions exact owner/principal MXIDs and public masters outside the agent.
Identity publishes persisted keys and shows its master fingerprint. Owner compares
and verifies in their trusted client. Refresh validates standard owner master ->
owner user-signing -> exact identity master evidence; visibility/APIs **verify in
U1**. Store validated public signature objects, signer IDs, signed master, binding,
trust revision and observation time in sealed state; validate after restart.
If chosen client cannot expose this chain, remain awaiting-owner; Q3 selects a
standard SAS alternative only with retained master-binding evidence proven by U1.
Verifying an unrelated device, comparing emoji in chat, or a local flag is insufficient.
No agent uses an owner's password/token or completes the owner's human step.

Root change invalidates owner-verification and blocks sends; never learn a new
root from Matrix or erase Owner DM ordering watermarks. Owner-verification of the
agent is separate from verification of the peer's session-originator device.

Owner DM S1 stays plaintext and `ownerAudience` continues refusing encrypted rooms;
no mirror send, S2 tags/rendering or undecryptable placeholder is enabled here.
Crypto acceptance uses the separate room with archive exclusion before any payload.
For Principal DM S1 (#173), preserve lifecycle and retrieve authoritative body before
changing/adopting it. Q6 covers whether any encrypted creation exception is allowed.
Future S1 migration needs explicit owner room selection, audience/readiness, new
binding generation, archive exclusions, ordering/locks and required protocol acks.
Old rooms/exclusions remain, without copied history or retroactive secrecy. In-place
encryption is irreversible and leaves prior plaintext; rollback never downgrades.

## 10. Producer DAG, ownership and review contracts

Sequence: owner Q1-Q6 resolved -> U1 -> U2 -> U3 -> U4 -> U5. These are work units,
not instructions to spawn agents. U3 lands adapter/client/accepted-event interfaces
before U4 implements coordinated sync/delivery against them; no parallel rewrite
of `client.go`, and integration is finished before U5 acceptance.

| Unit | Owned output / integration boundary | Likely files |
| --- | --- | --- |
| U1: dependency/store foundations | Offline closure, typed engine adapter, secret inventory, store codec/durability, replay/pruning primitives, global locks, Keychain derivation/create-only and durable generation pointer, latency capability evidence. Reads approved custody/ceremony inputs. | `go.mod`, `go.sum`, new `internal/e2ee/`, config crypto paths, CLI secret-store adapter; Curator vendor/pin flow. |
| U2: identity trust/bootstrap | Per-backend UIA/password route and any approved registration custody change, host-pin reader/validation/provisioning docs, principal-root revision/re-ceremony and Q4 approval transitions, cross-signing, owner-evidence verification, init/status/refresh and explicit recovery/rotation. | `internal/cli/account.go`, `connection_registration.go`, `credentials.go`, new `e2ee.go`; config credential APIs, matrix key/UIA methods, trust adapter. |
| U3: room/send/history interfaces | Shared accepted-event/evidence contract and versioned matrix response/state/key APIs; quarantined join/leave, approved creation only, context reconciliation primitives and recipient snapshots, durable outbox, topic/root/history decrypt, outbound files/voice/raw-send refusal audit. | `internal/matrix/client.go`, `txn.go`; actual `internal/cli/room.go`, `account.go`, `topics.go`, send/media callers, crypto adapter. |
| U4: coordinated ingress/output | Activation/migration and old-binary/projector/admin-registration fences, complete section 5 sync inventory, context bootstrap/every-sync reconciliation using U3 APIs, query-failure batch abort, prepare/commit/recovery, bounded verdict/journal controls, mailbox evidence/compaction/output revalidation, display watermarks, inbound archive/files/voice/export refusal audit. U3/U4 integration lands here. | `delivery.go`, `delivery_firstsync.go`, `wait.go`, `wait_retry.go`, `inbox.go`, `json.go`, `project.go`, `internal/cli/admin_registration.go`, admin preflight in `connection_registration.go`, `internal/matrix/admin_registration.go`, hooks/runtime/backfill/archive/media readers; U3 client interfaces, config state/mailbox durability and evidence. |
| U5: real acceptance / docs | Execute section 8 only after integrated U4; real owner ceremony witness, honest provenance limits, S1 compatibility and redacted commands/outcomes. No deferred engine/coordinator integration. | Acceptance fixtures/evidence, `docs/cli.md`, README/tools, protocol docs only with agreed MSG IDs. |

Future executable controls (none run for this revision) pair positive and negative
cases, with comments stating exactly what each proves and its limits:

- **U1:** Valid engine account/session persistence on macOS/Linux beside wrong
  key/binding, corrupt/unknown codec, unsafe mode/symlink, absent item, full disk,
  each file and directory fsync interruption. Prove prepared recovery skips Olm,
  including to-device-only batch and cursor-rename power loss; no orphaned ratchet.
  Known integrity violation must cause the intended gate failure. Keychain long
  services, token/password collisions, command bounds, duplicate create and newline
  encodings refuse without modifying credential items. Scrub planted loaded secrets
  in each supported encoding and suppress body-bearing library errors. Record
  full-cap latency, offline target builds and missing-module negative control.
  Interrupt after pointer fsync, after create-only Keychain success and before
  first sealed state: recovery locates the same `root.G`, without overwrite or
  credential-item changes. Missing/tampered pointer or conflicting bundle refuses;
  uninterrupted init/restart is the neighboring positive control.
- **U2:** Stable repeat init/restart beside interrupted upload, unknown UIA password,
  named-Keychain success, approved existing-account recovery and wrong whoami/device.
  Valid host pin beside agent-writable ancestor, replacement/same-revision bytes,
  wrong binding, symlink and unknown schema. CLI never provisions/repairs a pin.
  Valid master/self/device and owner evidence beside forged/wrong-usage/MXID chain,
  absent self-signature, conflicting roots, local-flag forgery and changed master.
  Exercise Q4 chosen future-device policy and rejected neighboring option.
  Legitimate host revision + 1 root update remains blocked until renewed Q3;
  completed ceremony exits `changed` without changing the agent master. Reject
  Matrix-only rotation, revision rollback and absent/old-root ceremony evidence.
  Same-root approved-device additions require valid fresh chains; removal blocks
  pending output/sharing and rotates sessions without promoting refused history.
- **U3:** Verified joined DM sends beside invited-only peer, third member, no eligible
  device, partial key query, revocation and leave/rejoin. Join valid encrypted room
  beside plaintext/unknown room, failed leave, and already-joined refusal: no
  premature subscription. Topic root -> post slug/reply resolution round trip.
  Own sent echo uses `own-outbound-session` after restart, preserving own-message
  thread learning/history reads; forged own sender and unknown session refuse.
  ACK loss retries identical ciphertext/txn. New
  device gets fresh session only. Raw send, attach/download, voice (including
  text prelude), envelopes and automatic send callers cannot leak payloads.
- **U4:** Verified peer reply passes normal/limited/post-join sync, read, inbox,
  wait/watch and bound consumer. Ordinary plaintext room keeps delivering beside
  unknown initial context and pre-existing encrypted-room plaintext injection;
  incremental omission never clears sticky encryption. Pin stage precedence for
  deleted-vs-unknown, missing-vs-forged chain, never-encrypted ciphertext, edits,
  unsupported msgtype, to-device without event ID and multiple simultaneous faults.
  Pair valid ciphertext with corrupt/truncated bytes, wrong room/source, forwarded
  key, replay index/new ID and altered same ID. Assert exact code and unchanged
  mail/receipt/shown/thread/subscription/unread/wake state. Test trust/mute revision
  races before append/print and no inverted lock acquisition. Fault each recovery
  boundary with intended durable outcomes, not merely any crash. Old-version and
  projector attempts must fail before sync/cursor/output; valid current owner works.
  An activated admin identity must refuse both baseline and polling
  `AdminRegistrationSync`, including direct/helper bypass and activation race:
  zero `/sync` requests, token-issue messages, registration/cursor/crypto mutations,
  with queued room keys untouched. A separate non-activated admin identity must
  still register successfully. Plant an unfenced call and require this negative
  control to detect the actual forbidden request, not a setup error.
  Pair a complete query admitting a verified peer's room key/reply with failed
  and partial `keys/query` for that same batch: no cursor, refusal journal/filter,
  account/OTK/session, replay, mail or outbox changes; complete-query retry from
  the same `since` succeeds. Only complete device absence yields
  `e2ee_device_unknown`. Do not let a refusal-only subset commit on query failure.
  Flood journal/verdicts/nonmember prekeys beside eligible positive traffic: bounded
  state, no forbidden OTK/session changes, floors never reopen; prune/full recovery
  preserve protected evidence. Hook-created refusal displays once per consumer
  during normal operation; print/save crash may repeat metadata only. Archive/files/
  voice/export audits include refusal-before-effects controls. Plant a trust-gate
  bypass and require intended negative-test failure.
- **U5:** Real owner verifies master in own client; retained public evidence validates
  after restart. Section 8 sends/reply/refusal with exact MXID/originator chain;
  correctly cross-signed/approved second device is the neighboring positive control.
  State honest hidden-revocation/key-holder limitations. Preserve S1 plaintext and
  encrypted-room refusal, pin/watermarks/archive exclusions/mirror deadlines.
  Record build SHA, public fingerprints and exact command verdicts. Fake-only
  interoperability or a skipped control cannot satisfy acceptance.
  Prove first peer room-key ingest precedes `read`; an exit-0 empty inbox or a
  missing exact reply cannot pass that gate. A failed command/human observation
  stops the run before later commands; successful refusal ingestion remains a
  passing command followed by an exact metadata/effect assertion.

## 11. Owner questions, source questions and future seam

All Q1-Q6 require recorded owner decisions **before U1 begins**. This revision
records options/recommendations only. Rejected alternatives change downstream
scope and must be reflected in the spec before implementation; they are not flags
an agent may choose for convenience.

| Question | Options | Recommendation | Consequence / owning unit |
| --- | --- | --- | --- |
| Q1: root custody and recovery | Unattended backend bundle + sealed state; host-unlocked broker; hardware/external signing boundary. For file backend on macOS **and** Linux: approve 0600 roots or require another backend. Approve explicit new-device recovery without MVP backup, or add backup scope. | Approve backend bundle/sealed account only on hosts whose disk/same-UID boundary the owner accepts; prefer Keychain on macOS. | Agent can use all three cross-signing private keys; adjacent file wrapping key cannot protect whole-directory theft. Rejecting requires custody redesign before U1; U1/U2 own implementation/recovery. |
| Q2: account-password custody and existing UIA | Persist passwords in selected credential store from future registration onward (including legacy/file); owner-performed admin reset or supported UIA step; authorized server-admin bypass; recreate identity. Existing discarded passwords cannot be recovered by persistence. | Persist future passwords in a distinct selected-store purpose; use owner-operated supported UIA/reset for existing identities after token/device impact review. Keep current named-Keychain route. | Expands long-lived agent-readable secrets; file mode adds disk password custody. Existing-account reset/bypass/recreation is a separate owner action, never automatic. Decline persistence means future UIA depends on manual recovery/server support. U2 owns all per-backend paths. |
| Q3: owner verification and host pins | Standard cross-user master signature; interoperable SAS with retained master evidence; wait for a suitable client. Host/root provisions public pins; alternatively design an equivalent agent-excluded broker. Approve section 3's principal-root rotation/`changed` recovery ceremony or defer rotation and keep it blocked. | Host/root provisions section 3 artifact; owner independently verifies replacement principal fingerprint, authorizes revision + 1, then repeats the selected ceremony for the existing agent master. A rotating owner must use their newly pinned chain. U1 verifies evidence visibility. | Requires real owner/client and host-admin work, including each root change; no local flag/TOFU substitutes. Root changes invalidate prior owner evidence; neither refresh nor the host edit alone restores readiness. SAS option changes U1 capability proof/U2 flow. Agent never writes anchors. U2 owns the explicit exit from `changed`. |
| Q4: admission of future devices | Automatically admit a newly observed valid cross-signed device under the pinned master; require explicit approval of each device ID plus valid chain. For explicit mode decide who authorizes additions/removals: owner only, or a separately defined delegated host authority. | Owner authorizes each `approved_devices` change; host atomically provisions revision + 1 in a coordinated window. Apply section 3's fresh-query/revalidation/session-rotation transition; unchanged roots need no repeated Q3. | More owner provisioning when devices change; host provisioning alone is not delegated approval. Automatic option ignores `approved_devices` as authority, delegates admission to peer cross-signing-key custody and still cannot prove server freshness. U2/U3/U4 implement only the chosen policy, preserve tombstones/final refusals and withhold removed-device output/sharing. |
| Q5: accepted plaintext at rest | Approve private plaintext mailbox and saved output; seal mailbox/output bodies; ephemeral display without durable plaintext. | Approve existing private plaintext storage only if owner accepts host disk/backups/same-UID exposure. | E2EE remains transport protection; saved output may outlive receipts. Decline changes U1/U4 storage/render/receipt architecture and acceptance; never silently drop durability or store plaintext. |
| Q6: encrypted DM creation / #173 | Owner creates room and agent joins; grant narrowly scoped new encrypted-DM agent exception; defer pending authoritative #173/protocol decision. | Owner-provisioned separate encrypted DM for MVP; no new agent creation exception. | `dm new --encrypted` remains absent unless explicitly approved with required protocol decision/acks; general encrypted-room creation deferred. U3 owns selected lifecycle; U5 uses approved path. |

Source work remains separate from owner policy: U1 selects actual audited engine,
codec, typed errors and hooks from readable sources; **verify in U1** for every
unread API. U2 proves selected UIA/ceremony and pins; U3/U4 inventory bypasses,
context endpoints, exact legacy fence behavior and unsupported operations. Obtain
authoritative local #163/#173 material before changing those contracts. This
revision does not fetch it or invent policy from an unavailable issue body.

Future identity resolution returns exact MXID, **public key material**, proof bytes
and verification result from a signature-bound identity-key proof; no directory
response alone creates trust. Keep Matrix transport keys distinct from Swarma
identity/authority keys. Session-originator evidence cannot substitute for that proof.

`swarm-envelope/2` carriage **exists locally** in `internal/matrix/envelope.go` and
`docs/cli.md`: `works.relux.swarm.envelope` carries unchanged bytes in canonical
unpadded base64url with established bounds/refusals. This is carriage, not authority
verification. Future post-decrypt verification consumes exact existing envelope
bytes/text with transport context separately; no reserialization as signed input.
Authoritative `grant/1`, owner-registry and Keymaker definitions were not available
in the inspected local sources; defer their integration without inventing formats.
No replacement grant/proof/signature/Matrix wire format is introduced here.

### Review resolution

This table records exactly the follow-up S0b changes in this pass. Findings already
marked resolved by that review retain their existing contracts; they were not
reopened. Numbers below refer to `review-s0b-verdict.md`, with original S0 references.

| S0b finding (original S0) | Specification changes in this pass; implementation still pending |
| --- | --- |
| 1 (7/17) | Section 5 names both admin-registration files, fences preparing/active admin identities before baseline/poll sync and effects, and lists every production sync caller/HTTP implementation with its fence. U4 ownership and no-request/no-effects negative test with non-activated positive control are explicit in section 10. |
| 2 (11) | Section 8 ingests the verified peer reply/key with inbox before exact-event read, adds command and human observation gates, checks every line against current behavior, and treats successful refusal ingestion separately from command failure. U5 gate/order controls added in section 10. |
| 3 (12) | Sections 3/5/7 make failed/partial required key query a retryable whole-batch abort with no cursor, mutation or final refusal; only completed query absence yields `e2ee_device_unknown`. U4 retry/no-effects controls added in section 10. |
| 4 (2) | Section 3 defines host revision + 1, independently checked replacement root, renewed Q3 ceremony and exit from `changed`; Q4 device edits use revisioned fresh validation/revalidation/rotation. Owner policy remains explicitly gated by expanded Q3/Q4 in section 11; U2 transition controls added in section 10. |
| 5 (9) | Sections 4/5 name the 0600 `generation.json` artifact, binding, pointer-before-root fsync order, create-only Keychain-to-first-state crash window and same-generation recovery/refusal rules. U1/U2 ownership and interruption/negative controls added in section 10. |

Only `docs/e2ee.md` was revised; no code was changed. No tests, builds, network
calls or tmux commands were run. All proposed implementation controls await their
own units. This document remains uncommitted, with owner Q1-Q6 unresolved.
