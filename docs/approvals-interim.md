# Owner approvals: waggle-format interim (design, for review by ivan-tb-keeper)

Status: DESIGN, no code yet. Replaced by waggle's verifier at CM1. Formats are waggle v5.2 verbatim; terms are curator-trust v0.4's.
Prior art and citations: research/owner-approvals-prior-art.md.

## Pieces
- **Operator key** (curator-trust §3, §8/§9): a presence-bound key per operator. Target: mac-keyvault Secure Enclave key with user
  presence (needs a provisioned binary, mac-infra EPIC-260915-1g0e1c). Interim until then: `ssh-keygen -t ed25519-sk -O verify-required`
  on a FIDO2 key (§9). Principal `op:alexis@relux`, later `op:ivan@relux`.
- **Gateway** (waggle §19.3): `svc:telegram@relux`, a service on the Mac mini under its own macOS user `a2a-gateway`. Its key is
  non-extractable (mac-keyvault, extraction none) under that user. It signs ONLY `event.v1@waggle`, never approvals.
- **Trust root**: `/etc/a2a/approvers`, root-owned 0644, a stock allowed_signers file (waggle §3) with per-line namespaces:
  `op:alexis@relux namespaces="approve.*,board.v1@waggle" sk-ssh-ed25519@openssh.com …` and
  `svc:telegram@relux namespaces="event.v1@waggle" ssh-ed25519 …`. Target: curator-trust §12's separate agent OS account.

## Canonical form

The request payload is UTF-8 JSON with keys sorted lexicographically at every object level, no insignificant whitespace, and no trailing newline.
The exact bytes are written mode 0600 to `state/approvals/<request-id>.json` and are the bytes passed to `ssh-keygen -Y sign` and later to
`ssh-keygen -Y verify`. The top-level keys are `expires_at`, `from`, `id`, `issued_at`, `schema`, `scope`, `signers`, `text`, `text_sha256`,
and `type`; timestamps are integer Unix milliseconds, `text_sha256` is lowercase hexadecimal SHA-256 of the UTF-8 text, and `signers` contains
the `approver` principal. The request event stores this same object under `ai.relux.a2a.approval`.

## Interim CLI slice

- `a2a approval request --room R --topic T --approver op:<name>@relux --scope S --text T --expires <RFC3339|+24h>` posts the approval request
  and prints its id and canonical payload path. Sign that exact file on the operator's machine.
- `a2a approval sign --payload FILE --key PATH [--post R --topic T]` invokes `ssh-keygen -Y sign` in `approve.request.v1@waggle`. With `--post`,
  it publishes the resulting SSHSIG as an approval decision.
- `a2a approval verify --room R --event $E --approvers FILE` verifies the decision signature, matching request, text hash, expiry and replay
  state. `--approvers` is required; production uses the root-owned `/etc/a2a/approvers` trust file.

## Flows
1. **Request**: an agent runs `a2a approval request --scope S --text T`; the envelope (waggle §4.1, type `approval.request`, `signers: {approver: op:alexis@relux}`) is posted to the agent's room and the gateway.
2. **Low-risk scope, tap** (curator-trust §7, waggle §19.8): the gateway renders the canonical fields itself (it, not the agent, computes what is shown), sends Telegram Approve/Reject to the pinned Telegram user id, verifies Telegram's own auth, and on Approve signs `event.v1` "op:alexis tapped Approve on request <id>, sha256 <hash>". It counts only inside a live **grant**.
3. **Grant**: `board.delegate`-schema envelope signed by the operator key with presence (Touch ID / FIDO UV), rendered locally by `a2a approval grant` on the operator's own machine: acts = approve requests of scopes S1..Sn via events from `svc:telegram@relux` bound to the operator; expiry (e.g. 30 d); budget k/day. Grants cannot cover confirmations, capability grants or high-risk classes (curator-trust §7).
4. **High-risk or outside a grant**: the operator signs the request directly with the presence key over a locally rendered view (`a2a approval sign <id>` at the Mac), per waggle §4.4 `user_verification = true`.
5. **Verify**: `a2a approval verify <room> <$event>` runs waggle §4.3's algorithm against /etc/a2a/approvers: signature + namespace + principal, request hash, scope in grant, grant live and unrevoked, budget (counted in the requester's recorded verdicts; single machine), nonce unused, expiry. Only then does the agent treat it as its owner's in-session answer for exactly that scope.

## Open for tb-keeper
- The event.v1 subtype for "chat-bridge tap" (waggle addendum by tb-keeper, not defined here).
- Budget counting across machines (waggle §12 lease store) — interim: per requester only.

## Operator one-time steps (Alexis; Ivan the same with his keys)
1. Operator key: mac-keyvault enclave key (once provisioned) or a FIDO2 key (`ssh-keygen -t ed25519-sk -O verify-required -C op:alexis@relux`).
2. macOS user `a2a-gateway` on the Mac mini; Telegram bot via @BotFather, token into that user's keychain; message the bot once.
3. Two sudo commands (provided) install `/etc/a2a/approvers` on the Mac mini and the MacBook.
4. First grant: one Touch ID / key touch.
5. Move humans/alexis.json out of the agents' reach; change the @alexis Matrix password in Element.
