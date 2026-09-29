# Owner approvals: prior art (read 2026-09-29, relux-works specs via gh)

- waggle spec/waggle.md DRAFT v5.2 (tb-keeper): principals op:/orch:/svc:; roster = stock allowed_signers with per-line namespaces (§3);
  envelope with role signatures, approver signs `approve.<class>.v1@waggle` (§4); §4.4 policy per type, `key="sk", user_verification=true`
  is the ONLY form of "operator physically present"; `task-board mail approve <id>` renders every field (what you see is what you sign).
- §19.2/19.3 signed external events + service principals: a bridge (`svc:`) verifies the provider's own auth, then signs ONLY `event.v1@waggle`;
  it never holds an operator key and cannot sign board/coord/cmd.
- §19.8 delegated acts: the operator signs a `board.delegate` grant once (acts, scope, conditions, expiry, budgets); delegated acts are signed
  in the `delegate` role naming the grant; the verifier checks grant + delegate + bounds; revocation ends it; everything else waits for the person.
- agent-session-host spec/session-host.md §6.2: every agent runs as the operator's UID, so authorization uses capability tokens; tokens in a
  session's env are readable by same-user processes.
- Consequence for a2a: a Telegram gateway that MINTS approvals with its own key contradicts waggle. The waggle-consistent shape is:
  gateway = svc: bridge signing `event.v1` "owner <telegram id> tapped Approve on request R" + an operator-signed delegation grant that
  says which scopes such events may approve, until when, with which budgets. High-risk classes keep requiring the op key with user verification.
