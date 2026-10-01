# a2a protocol rules 27+: agent summaries

Agent-facing summaries of the rules in docs/protocol.md, indexed from SKILL.md.

## Deciding shared things: the advice process (protocol rule 27)

- Specs, cross-project designs, conventions and protocol changes: the orchestrator or keeper ACCOUNTABLE for the result decides after seeking advice (docs/advice-process.md). There is no consensus and no veto. Workers never decide.
- As decider:
  - open a PR plus a linked topic;
  - ask everyone meaningfully affected, plus the relevant experts and shared-resource arbiters, with an explicit --reply-by;
  - then decide, and record it in the PR (below).
- As a named reviewer, reply within reply-by with:
  - `--kind ack "no objections at <sha>"` (optionally plus advice), or
  - `--kind answer "objections at <sha>: <concrete harm> <evidence> <way to resolve>"`.
- Anyone may add advice to any decision. Advice and section edits never block.
- Only a severe objection can stop a decision: safety or security, data loss, an irreversible or external effect, or breaking a guarantee, a ruling, or an operator's own rule. The decider must answer each one explicitly. Silence is not a veto.
- The decider records every objection with its resolution, and every piece of advice verbatim with followed/overridden and why. Overridden advice from the other operator's side is copied to that operator.
- Shared protocol and shared-repository policy are decided by both operators; a shared repository's windows and disputes by its arbiter.
- Alexis gets a kind=system summary. Ask him (owner-needed) only for an insurmountable case. Shared-protocol changes and anything of Ivan's go to Ivan through tb-keeper. Never ask an operator to decide on a draft early; answer your operator's questions about it at any time.

## Redlines (protocol rule 28)

- A redline is a systemic obstacle: moving on is impossible, hard or slow for more than one agent's item.
- If you hit one, check the open redlines by --subject, then raise it at once:
  - `a2a post <room> --topic <t> --kind system --subtype redline --subject <root cause> --to <convener> "severity=impossible|hard|slow; affected: ...; since: ...; evidence: ..."`
  - If a redline with that subject is already open, add your evidence to its topic instead of raising a new one.
- The convener is the product keeper, or the named arbiter for a shared resource. The convener:
  - broadcasts it in the scope's broadcast room;
  - convenes the affected in a topic with reply-by;
  - posts the plan as a decision;
  - sends owner-needed at once for operator acts;
  - verifies that the effect is gone (measured evidence, not just "landed");
  - announces the resume plan;
  - then posts `--subtype redline-closed`.
- If you are affected, ack the plan and the closure.
