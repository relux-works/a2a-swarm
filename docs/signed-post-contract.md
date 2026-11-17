Two-sided contract of record for the signed post (sender and key holder). Sections 1-11 below are r4, carried unchanged; section 12 is amendment 1.
# M2 two-sided contract r4: the signed message between an a2a sender and Keymaker

Status: r4 = r3 (accepted by Keymaker, sha256 624bd202...) with section 11, one carrier-side row that names the event member carrying the attestation. For Keymaker's re-ACCEPT by hash. Nothing here is implemented or live. Sources read by the drafter, in full:
swarma-keymaker at 682f5637 `messageauthor/README.md` and `spec/keymaker-message-author.md`; a2a-swarm main 051777b5
`docs/envelope-observe.md` and `internal/envelopeobserve` (reader). Not read: Keymaker's code, the envelope module's
source, the 0.1.54 specification text. Where this draft restates Keymaker's own text it is marked SAME; where it adds
or decides something it is marked NEW; where it needs Keymaker's word it is marked ASK.

## 1. Parties and the one rule
- Sender side (a2a): builds the message, asks for one signature, verifies it, publishes. It never holds, reads or
  derives the key.
- Holder side (Keymaker): owns one message-author key per agent identity (purpose 9, lead's ruling), signs exact
  bytes it admitted, returns the key id and the signature.
- Rule: a2a publishes a signed message only when it verified, itself, that the returned signature verifies over the
  exact bytes it sent under the key it expected. On anything else it publishes nothing; it never falls back to an
  unsigned post.

## 2. The signed object (one table)
| Item | Value | Status |
| --- | --- | --- |
| Key type | `bip340:` + base64url_no_pad(x-only 32) = K | SAME (spec, "Exact upstream profile") |
| Key purpose | Keymaker purpose 9 `message-author`, one key per agent identity; not the request key (7), not the delegation key (8) | SAME + lead ruling |
| Record | R = `{body:BODY,key:K,role:"author",version:"swarm-envelope/2"}` | SAME |
| Domain tag | ASCII `swarma-msg/2` then LF | SAME |
| Preimage | P = tag LF CCJ-1(R), original bytes, no final LF, at most 24576 bytes | SAME |
| Digest | H = SHA256(P), exactly once, inside the holder | SAME |
| Signature | S = BIP-340 over H, raw 64 bytes | SAME |
| Envelope module | swarma-envelope v0.0.0-20261114153213-065e25f69da8 on BOTH sides | SAME |
| BODY type | `message/1`; members exactly `type aud nonce iat exp room to body_sha256 author author_attestation action target arguments` | NEW (a2a owns the body) |
| `author` | equals K | SAME (provider admission) |
| `author_attestation` | equals T, the `sha256:` content id of the attestation the holder returned in the identity reply | SAME |
| `exp - iat` | at most 120 s | SAME |
| `action` | `a2a.post/1` | NEW |
| `to` | one recipient: its full MXID; zero or several: the room ID, with the ordered list in `arguments.to` (1..64 MXIDs, each at most 255 bytes, no duplicates) | NEW |
| `arguments` | `{kind, topic, re, to}` exactly as the Matrix event header carries them | NEW |
| `body_sha256` | SHA-256 of the exact `content.body` bytes of the Matrix event | NEW (as the landed receiver checks) |

## 3. The exchange (events x who checks)
| Step | a2a does | Holder does | On failure |
| --- | --- | --- | --- |
| 0 connect | dials ONE explicitly configured Unix socket; no discovery, no fallback; authenticates the holder (peer pins) before any request | native version-1 hello, peer admission | a2a: `signer_unavailable`, nothing published |
| 1 identity | `message_author_identity` with params `{}` | returns K, T, A | refusal code table below |
| 2 identity check | K well formed and of the admitted type; T well formed; A non-empty; **verification of A under the per-host purpose-5 pin** | none | `signer_unqualified`, nothing published |
| 3 build | builds BODY with `author`=K and `author_attestation`=T, then P | none | local refusal before any sign request (oversize recipients, P over 24576) |
| 4 sign | `sign_message_author {preimage: P}`, one outstanding request per connection, own request id kept | admits P (exact tag, canonical CCJ-1, key and author equal own K, attestation equals current T), signs | refusal code table below |
| 5 verify | returned K equals the K of step 2; S verifies over SHA256(P) under K, checked ONCE | none | `signer_result_invalid`, nothing published |
| 6 publish | assembles the envelope once; publishes; a retry before `exp` re-publishes the SAME bytes | none | Matrix error: retry the same bytes |
| 7 after expiry or a new T | new nonce, iat, exp and a new request id; never an old signature under a new attestation | none | none |

## 4. Refusal codes: how a2a treats the holder's closed vocabulary
- Retryable by the holder's own bit (`busy`, `rate_limited`, `generation_source_unavailable`, `audit_unavailable`,
  `request_deadline_exceeded`): a2a may retry the SAME request id with the SAME P, at most twice, inside the message
  lifetime; then `signer_unavailable`. NEW (the retry bound).
- Every other code: `signer_refused` carrying the holder's code name and nothing else; no retry. NEW.
- A reply that does not decode, an unknown code, a wrong correlation: `signer_result_invalid`; no retry on that
  connection. SAME as the codec's `ErrResponse`.
- A lost reply proves nothing about a commit (SAME). a2a therefore never sends a DIFFERENT P under the same request id.
a2a's own exit vocabulary for `--sign`: `signer_unconfigured`, `signer_unavailable`, `signer_unqualified`,
`signer_refused`, `signer_result_invalid`, `recipients_too_many`. NEW.

## 5. What the receiver needs (so both sides agree what a signature means)
- First run: the receiver maps the Matrix sender MXID to ONE author key through the host roster (root-written file,
  already landed as a reader). It does not need A on the wire. A signature therefore means: "the holder of the key the
  HOST OPERATOR listed for this MXID signed exactly this text for this room and these recipients".
- A is not carried in the Matrix event in this version. NEW, and see ASK 3.

## 6. Contract tests (both sides run the same bytes)
One vector file, `m2-contract-v1.json`, with its SHA-256 pinned in both repositories; proposed home: Keymaker's
`messageauthor/testdata/` beside wire-v1, because the holder's admission is the stricter side. Rows:
1. Positive: BODY fields -> exact P bytes (hex) -> the digest; a public fixture key signs; a2a's builder must emit
   those P bytes and the holder's admission must accept them.
2. One row per body shape: single recipient; room broadcast (empty `arguments.to`); three recipients.
3. Negatives the HOLDER must refuse, each beside row 1: wrong tag; `author` not equal to K; `author_attestation` not
   equal to T; non-canonical CCJ-1; P over the cap; `key` in R not equal to K.
4. Negatives the SENDER must refuse before any request: 65 recipients; a duplicate recipient; a recipient over 255
   bytes.
5. Negatives the SENDER must refuse after the reply: K of the sign reply differs; S flipped in one bit.
Each side reports the vector file hash it ran and the count of rows by verdict.

## 7. Questions to Keymaker (ASK)
1. The native hello before the first request: which text defines it for a message-author client, and which peer pins
   must a2a check on the socket (owner, group, mode, kernel peer uid, as for the request-role dialer)?
2. Identity check step 2: the per-host purpose-5 pin under which a2a verifies A. Where does a2a read that pin on a
   host, and in what format? Until that exists a2a can only do the shape checks, and I would name the state
   `signer_unqualified` rather than sign.
3. Is it acceptable for the first run that A is NOT carried in the Matrix event (the receiver trusts the roster
   instead)? If you need the sender to carry A, name the member and the size bound.
4. Timeout: the codec allows 1..2000 ms. Proposed: a2a uses 2000 ms and one connection per post. Agree?
5. Retry under a retryable code: you say a retry must reuse the retained signature. Is "same request id and same P"
   the exact key of that retention on your side?
6. One key per agent identity: what names the identity on the wire, since requests carry no selector? I read it as
   "the kernel peer of the connection, mapped by the holder". Confirm, so that a2a never sends a name.

## 8. Not in this contract
Real keys and the live coordinator (the holder's side); the roster writer (a2a, unit M3a) and attestation checking in
it (M3b); refusing unsigned senders (M4); E2EE.

## 9. Changes from r1 (Keymaker's answer, file m2-contract-r1-keymaker-answer.md, grounds swarma-keymaker a3b9def3)
These rows REPLACE the r1 text where they differ.

| Point | r2 text |
| --- | --- |
| Steps 0 and 2 | Both are Keymaker's own rule too (authenticate the holder route; verify A under the per-host purpose-5 pin). SAME, not optional. |
| Holder admission | The holder checks the tag, canonical CCJ-1, that K equals both `key` and `body.author`, and that T equals `body.author_attestation`. It does NOT interpret any other body member. |
| BODY | Its members must be exactly the closed `message/1` schema of the pinned envelope module; the receiver's VerifyMessage checks that schema. The carrier (a2a) owns the body. |
| Hello (ASK 1) | First frame `{"method":"hello","request_id":"000...001","versions":[1]}` per Keymaker spec, section 7 "Surface B: native control". After the hello the next complete frame must arrive within 2 s: a2a sends identity at once and sign without a pause. |
| Peer pins (ASK 1) | Before the hello a2a checks: configured socket path, no discovery; parent directory owned by root or the holder, not group- or other-writable; socket owned by the holder's service account, group = the host's message-author client group, mode 0660; kernel peer uid = the holder's configured uid. A failed pin is `signer_unavailable` with NOTHING sent. Exact uid, group and path come from the host's installed configuration. |
| Purpose-5 pin (ASK 2) | Does not exist yet. Until Keymaker's per-agent unit delivers it, a2a does shape checks only and the state is `signer_unqualified`: nothing is signed for real. The pin will be a root-owned host file holding the purpose-5 issuer's x-only public key and the `attestation/1` profile id; a2a's host configuration names its path. |
| A on the wire (ASK 3) | DECIDED by the carrier: A IS CARRIED from version 1: raw at most 8192 bytes, non-empty, base64url without padding, in the Matrix event beside the envelope; the sender checks that the content id of A equals T before it publishes. A first-run receiver may still decide by the host roster alone; that is a NAMED LIMIT: no purpose-5 expiry or revocation evidence, so a withdrawn key stays valid until the root-written roster changes. |
| Timeout (ASK 4) | 2000 ms; one connection per post (hello, identity, sign); one outstanding request at a time. |
| Retention (ASK 5) | The holder retains by caller principal + request id + method + canonical params (P) + binding, root, K and T. So: same id with another P is `request_conflict` (not retryable); after a T renewal build a new P and a new id; a retry from another kernel principal is another request. A retry inside those bounds returns the identical retained signature. |
| Identity (ASK 6) | No selector exists. The holder maps the kernel peer (uid plus account generation) to its binding. DEPLOYMENT RULE: each agent identity runs its a2a sender under its OWN OS account; two agents under one uid cannot have distinct keys. |
| Contract vectors | Home: Keymaker `messageauthor/testdata/m2-contract-v1/` with a manifest; both sides pin its sha256. They arrive with Keymaker's per-agent unit, not with the codec-module packaging. Holder-side negatives use the holder's exact codes: `signer_tag_malformed` or `signer_domain_refused`, `signer_identity_mismatch`, `signer_payload_invalid`, `signer_preimage_too_large`. |

Open after r2: none between the two sides. Named limits, not disagreements: the roster-only receiver; no real signing
until the purpose-5 pin and the live coordinator exist.

## 10. Changes from r2 (lead ruling of 2026-11-16 on where the bridge runs)
The holder-side facts are unchanged. Two carrier-side rows are reworded; they REPLACE the r2 text.

| Point | r3 text |
| --- | --- |
| Identity (replaces the DEPLOYMENT RULE of the row "Identity (ASK 6)") | The process that sends the hello, the identity request and the sign request runs under the agent's OWN OS account. One OS account = one message author at the key holder. A request from another account maps to another binding or to none, and signs nothing for this author. |
| Step 6, publish (replaces "assembles the envelope once; publishes") | The author-side process assembles the envelope once from the verified signature. Publishing the event is the carrier's own path: on a host with a bridge under a shared messaging account the signed envelope is handed to that bridge; on a host without one the same process publishes. Which account publishes is the host's deployment and is NOT constrained by this contract. In both cases the bytes published are the bytes assembled, and a retry before `exp` republishes the same bytes. |

Consequence stated for both sides: the holder never learns who publishes; the receiver's decision rests on the
signature, the roster and the delivery binding, never on the publishing account.

## 11. Change from r3 (carrier row: where the attestation travels)
The pinned envelope module defines no member for a sender-carried attestation, and none is needed there. One row is
added; nothing else moves.

| Point | r4 text |
| --- | --- |
| A on the wire, the member | A travels in the Matrix event CONTENT, beside the envelope member, in the carrier's own member `works.relux.swarm.author_attestation`: a string, base64url without padding of the raw attestation bytes, non-empty, at most 8192 raw bytes. It is NOT a member of `swarm-envelope/2` and is not inside the signed bytes. The binding is inside the signed bytes: `body.author_attestation` = T is the content id of A and the signature covers T. Sender: before publishing, checks that the content id of A equals T. Receiver: when the member is present, checks that the content id of A equals `body.author_attestation`; a mismatch, a malformed value or an oversize value labels `invalid: attestation_mismatch`; when the member is absent the roster decides as before. The content id is computed by the pinned envelope module's own function for an attestation record; neither side defines another. Verification of A under the purpose-5 pin is not part of this version (no pin exists yet). |

## 12. Amendment 1 (2026-11-17): connections and timeout, acknowledged by the key holder

The preimage depends on K and T, and on an unbound connection every next frame must arrive within 2 s of the previous
reply, so building the preimage between the identity reply and the sign request on one connection races that window.
These rows REPLACE the r4 row "Timeout (ASK 4)"; nothing else moves.

| Point | Amendment 1 text |
| --- | --- |
| Connections per post | Two. Connection 1: hello, identity. The sender then builds the preimage. Connection 2: hello, a FRESH identity request, then the sign request at once, with no other work in between. |
| Identity on connection 2 | Its K and T must be byte-equal to the K and T inside the preimage. If either differs, nothing is signed: the sender closes and prepares again with a new nonce, iat, exp, a new preimage and a new sign request id. A preimage is never signed under a stale T. |
| Request ids | Only the hello carries the fixed id `00000000000000000000000000000001` (it may repeat on every connection). Every identity and sign request carries a fresh random id and never that one. |
| Sign retry | A retryable refusal or a lost reply is retried on a NEW connection: hello, the fresh identity check above, then the SAME sign request id with the SAME preimage. The holder keys its retention by caller, request id, method and preimage and returns the retained signature; it never signs twice. |
| Timeout | `timeout_ms` = 1500 on each request; the sender's read budget is 2 s from before the write; one outstanding request per connection. |

