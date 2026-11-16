# Device-list check for Matrix devices (LIP-0029, observe only)

`internal/devicelist` implements the Matrix device-list check of
swarma-link-spec v0.1.51. LIP-0029 and LIP-0010 section 11 remain Drafts with
co-author agreement pending. The snapshot target is swarma-chain-lab
`CONTRACT.md` at **d1cab6f81d14fc86dfa31a110a04d9f3b692850a**, sections 3–6.
This is a library with no delivery-path caller, live adapter, network access,
effect or persistence.

## API and decisions

| Part | API | Specification |
| --- | --- | --- |
| Strict list codec and hash | `Parse`, `HashOf`, `List` | Sections 3.1, 3.2, 3.6, OM1 |
| Name-value pin | `PinFromNameValue`, `Pin` | O3.4–O3.5, LIP-0010 SH6 |
| Bounded, no-fetch list cache | `NewCache`, `Cache`, `Lists` | O3.11–O3.14 |
| Messenger observation | `Admit`, `Event`, `Check`, `Result` | Section 11 as OM7 changes it |
| Chain boundary | `ChainView`, `NameView`, `NameOp`, `Tip`, `SealState` | Steps 4 and 8 |
| Snapshot reader | `ReadSnapshot`, `SnapshotConfig`, `Snapshot` | Contract section 5, part (B) |
| No usable snapshot | `UnavailableView` | Local unavailable-class observation |
| In-memory observe journal | `NewObserveJournal`, `ObserveJournal` | OM5, observe approximation of O8.4 and SH5 |

`Event` contains facts already established by an OM2 verifier; this package
does not verify the event's cryptographic provenance. `Check` supplies the
consumer's check time, clock trust and independently learned live tip.
`Admit` reads no clock, file or socket. It stops at the first relevant step
and returns L1 `instruct`, refusal with code/step, or ignored for plaintext.
Every returned `Result`, including refusals, closure-recording refusals,
ignored events and error-path results, has **`ObserveOnly=true`**. An error
has no command verdict and never admits. `Result.SealState` carries the
actual step-8 answer, or is empty when no seal was read (including an
already-journaled closure). `Result.ObservedInstruct()` reports what an
enforcing reader WOULD have decided, subject to checks outside this package;
it must never gate delivery. It recognizes only marked observe-only results.
There is no admission accessor.

`Code.IsLocal()` distinguishes `CHAIN_VIEW_UNAVAILABLE` and `JOURNAL_FULL`
from the package's LIP-0029 codes. Neither local class is a specification
wire code. View errors return a fixed error class without upstream detail.

## Observe only

Nothing in a result may switch admission or feed enforcement. Each reason
below holds independently:

- A snapshot cannot see a spend after collection, or every mempool-only spend.
- Collection is only `bracketed`. Equal before/after tips cannot rule out a
  hidden A→B→A transition or make the mempool collection atomic.
- `ObserveJournal` is in memory inside the consumer process and fed by
  per-command reads, not continuous O8.3 observation. It has no persistence
  or serialized form. Enforcement requires the separately admitted
  host-owned watcher and protected append-only journal of contract section 4.

An **observe-grade closure** is not validated spend evidence: **no spending
transaction was validated, and absence from the confirmed set can also come
from a reorganisation that removed the funding of the seal**. The closure
record is private. No function exports closures to another package:
`Close` imports an external observe-grade closure, but there is no closure
getter, enumeration, export or serialization API. `Head`, `SeenUnspent` and
`Full` expose only their named non-closure facts.

## Snapshot format and filesystem

The canonical CG1 payload is exactly one object, at most 65536 bytes, with no
trailing LF. The descriptor reader consumes at most 65537 bytes.

```
{"consistency":"bracketed","mode":"observe_only","names":[NameRecord...],
 "network":"main"|"regtest","seals":[SealRecord...],
 "tip":{"hash":H,"height":N,"time":N},"v":1,"written_at":N}
```

Keys are exact and recursively ASCII-sorted; minimal integers, padded standard
base64 for exact name-value bytes, and canonical re-encoding are required.
Duplicate, escaped, fold-alias, reordered, missing, unknown or null members,
noncanonical escapes/numbers, whitespace and trailing bytes refuse. Names
are strictly sorted (1–16), seals strictly sorted by txid then numeric vout
(1–256). Every record binds the single complete tip. An expired name never
establishes a pin. The only accepted consistency is `bracketed`; even
`witnessed` is invalid at this revision.

Seal shapes from contract section 3:

- `unspent`, `unconfirmed`, `spend_pending`:
  `{"confirmations":N,"script_pubkey":Script,"state":S,"txid":H,"vout":O}`.
  `unspent` is confirmed and not mempool-spent; `spend_pending` is confirmed
  but mempool-spent. Both require 1..tip height+1 confirmations, also within
  the CG1 safe-integer bound. `unconfirmed` requires exactly zero.
- `not_in_utxo_set`: `{"state":"not_in_utxo_set","txid":H,"vout":O}`,
  with neither metadata field. It means absence from both the confirmed set
  and the mempool-inclusive view, not proof of a spending transaction.

Scripts are transported as lowercase even-length hex, 0–20000 characters.
Present states with vout above 2147483647 are invalid. No expected script is
defined by the pinned list format: **no seal-script validation is claimed**.
The parser validates confirmations and script bytes, then the public view
retains only seal state. Its parser data and record types are private; no
exported producer or metadata transport API is claimed. Count caps do not
promise that all combinations fit the byte bound.

`SnapshotConfig` comes from protected host configuration, never the snapshot
or environment: one absolute clean physical path, network, owner and exact
name/seal watch sets. Foreign, extra or missing records are invalid. A list
introducing an unwatched signing seal gives `invalid` with
`CHAIN_VIEW_UNAVAILABLE`, not revocation evidence; root reconfiguration is
needed. The reader never infers an updated watch set.

The reader walks descriptors from `/`, without following any symlink. Every
ancestor is root-owned and has no group/other write bits (0711, 0700, 0750
and 0555 are allowed if traversable); the containing directory is exactly
0755. The regular file is root-owned, exactly 0644, single-link. Linux refuses
all POSIX access/default ACLs, stricter than only refusing agent-writable
ACLs, except that `ENOTSUP` is accepted: a filesystem without the POSIX ACL
xattr namespace is not checked for ACLs. Darwin does not check ACLs at all;
its weaker-than-contract profile requires separate native qualification
before installation. There is no writable/sticky-directory exception. On an
opened inode with zero links, the reader reopens once relative to the validated parent;
a second zero-link inode fails closed. It refuses effective UID zero and
never creates or repairs the path. On macOS the configured path must be the
physical path, with no symlink component.

`ReadSnapshot` returns no view on failure, plus the fixed error and label:
`unknown` for an absent file, `invalid` for a present but refused file.
Pass `UnavailableView{Absent: errors.Is(err, ErrSnapshotUnavailable)}` to
`Admit` after a read failure to obtain a Result for each valid event. A zero
`UnavailableView` means present but refused. It refuses at step 4 unless an
earlier event gate applies. Neither form contains a path or upstream detail.

### Labels

The helper cannot assert a freshness label or threshold. Constants are consumer
policy in `snapshot.go`, never read from the file:

| Constant | Value | Rule |
| --- | --- | --- |
| `SnapshotMaxAgeSeconds` | 60 | Age strictly greater is stale |
| `SnapshotMaxTipLagSeconds` | 7200 | Lag strictly greater is stale; tip time more than 7200 seconds ahead is invalid |
| `SnapshotFutureSkewSeconds` | 0 | Any future write time is invalid |

Precedence: schema/filesystem/configuration failure invalid; no trusted clock
unknown; future write or implausibly future tip invalid; age or lag stale;
no independent live tip unknown; complete live-tip mismatch stale; otherwise
fresh. A snapshot-only host never has a fresh label. `Admit` recalculates age
at every use. Label combination never improves either input:
**invalid > stale > unknown > fresh**. All `ChainView` methods are a trusted
adapter boundary: a foreign adapter can lie about its live flag, label, name
or seal state. The package checks relevant shapes and closed sets, not the
honesty of foreign implementations. A live claim never removes observe-only.

## Observe journal

All state is keyed by (network, owner). Nothing is evicted.

- **Step 8:** every state except positively `unspent` refuses the command as
  `DEVICE_REVOKED`-class. `unconfirmed` and `spend_pending` create neither
  seen-unspent entries nor closures, even after an earlier unspent answer.
  Never-seen absence refuses only that command. A non-older absence after
  this reader journaled unspent creates an observe-grade closure by key and
  outpoint. Once recorded it is never lifted by any later view or head.
  Errors create no closure. At an unspent step-8 pass, the reader journals
  every seal of the authenticated pinned list that the view reports unspent,
  even if the event is later refused for ceiling or replay: the evidence is
  the view, not the event. This can precede another device's first event.
  Capacity is checked for the complete set before the first seen-unspent
  write; failure changes no seen entries or tips and marks the owner full.
- **Ordering:** stale views write no head, seen-unspent entry or closure.
  Replay pairs are consumer-side state, not chain-derived: every instruct
  observation records its pair, even under a stale view, and duplicates
  then observe `REPLAY` under stale or young views. Owner state may be
  allocated for that pair. The observing tip (height and hash) is retained
  with the head and each seen-unspent entry. A lower observing height is
  older. Equal heights with different hashes cannot be temporally ordered:
  neither can close or overwrite the journaled head or seen tip. These
  observations give per-command verdicts only, apart from recording replay
  pairs. Equal height with the same hash can close after prior unspent.
- **SH5 approximation:** once the pin/list bindings pass step 5, a usable
  ordered view may move the head before later event refusals. The head
  contains sequence, pinned hash, earliest equal-pin name operation and most
  recent observation tip. A lower sequence at a higher operation height
  refuses `DEVICE_LIST_ROLLBACK`; a lower sequence at the same/lower operation
  height may be a reorganisation. Equal sequence with a different pinned hash
  always refuses as a rollback-class conflict. Equal sequence/hash at a
  greater operation height keeps the earliest operation. Head changes never
  lift closures. This approximation cannot query active-chain membership and
  can falsely refuse after deep reorganisations until a higher sequence.
- **Bounds:** 4096 closures and 4096 seen-unspent records per owner; 4096 replay
  pairs per owner and 65536 total; at most 16 owners. Losing required closure
  or seen-unspent capacity marks only that owner full for the process life:
  existing closure refuses `DEVICE_REVOKED`, otherwise `JOURNAL_FULL`.
  Replay exhaustion refuses new pairs at step 11, retaining existing pairs
  as `REPLAY`. One owner's volume cannot consume another's per-owner replay
  allowance; the total bound remains a separate fail-closed ceiling.

## Specification choices and remaining limits

- Room mismatch is `NOT_FOR_ME` at step 2 (room stands in for `aud` under
  OM7); OM2's table names `MESSENGER_UNAUTHENTICATED`. Policy clarification
  remains open.
- A Matrix record with presence or ceiling other than 1 is
  `DEVICE_LIST_INVALID` at step 9. A plain L1 text is never above its ceiling.
- Step 6 separates ruled-but-different algorithms (`DEVICE_ALG_MISMATCH`)
  from reserved algorithms (`DEVICE_ALG_UNSUPPORTED`). Only Matrix Ed25519
  messenger admission is implemented.
- **The darwin profile does not check ACLs and is therefore weaker than the
  contract.** The stub is retained unchanged in shape; a separately qualified
  native implementation is required before any darwin installation. Linux
  cross-vet is not native darwin runtime evidence.
- No watcher, restart durability, historical spend proof, live adapter,
  cryptographic event verifier or caller is implemented. Devices removed by
  a later list are not individually journaled as refused. Restart loses this
  observe-grade state; it cannot supply the durable contract journal.
- A watched name with `not_found` prevents helper publication. Its old file
  merely ages to stale; the file alone cannot distinguish a vanished name
  from helper failure.
- The originator-key/fingerprint integration question stays open; `Event`
  is unchanged. `swarma.owner_command` is not parsed: admitted events are
  plain L1 instruct. Lists enter only as supplied octets; no fetch path exists.
- Exact shared fixtures are vendored as
  `testdata/observations-d1cab6f8.json` and `testdata/vectors-d1cab6f8.json`
  under the package, with both full SHA-256 digests asserted in tests.
  Tests assert all 21 labels, 29 seal histories (64 steps), 45 snapshot
  schema and 3 size vectors, plus independent parser, filesystem, ordering,
  bounds and planted controls. Size vectors exercise the real parser bound;
  bounded non-JSON payloads still fail schema validation.
  The seal-history oracle's `observe_closed` maps to the private observe
  journal. `enforcement_closed` is always false: no such state exists here.
  Its `refuse_command` flag means only that the current seal read is not
  positively unspent (including read failure), independent of observe
  closure. A false flag never authorizes admission; a retained closure may
  still yield a refused consumer-check observation. Gateway request/response
  framing, upstream two-read collection, retry and witness qualification are not
  implementations of this snapshot consumer and are not claimed here.
