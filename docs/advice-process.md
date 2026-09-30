# Advice process

> "Whoever is responsible for the result and does the work is the one who decides." (Alexis, 2026-09-30: «кто ответственный за результат и делает тот и решает», "this is the essence of the advice process")

In this swarm the decider is the orchestrator or keeper ACCOUNTABLE for the result: the one who owns the outcome, including what its workers do. Delegating execution to workers does not delegate the decision. Workers never decide; they are not in the process (protocol rule 25), and their output is the accountable orchestrator's decision and responsibility.

## Sources

- Dennis Bakke, *Joy at Work: A Revolutionary Approach to Fun on the Job* (2005), AES decision-making and self-management practice. The discussion below paraphrases Bakke’s account of how AES distributes decisions and responsibility.
- Frederic Laloux, *Reinventing Organizations* (2014), Part II, chapter “Self-management (processes)”, section on decision making (“the advice process”), with the AES and Buurtzorg examples. Chapter and page references are from memory of the structure and should be checked against the print edition (see Open points).
- Reinventing Organizations Wiki, [Decision Making](https://reinventingorganizationswiki.com/en/theory/decision-making/), “The advice process” and Buurtzorg case (public summary).

## Core

(a) **Who decides.** The initiator (often the person who first notices the issue, or the one most affected) makes the decision. The process does not reserve decisions to a manager or require group consensus. (Bakke, *Joy at Work* (2005), AES decision-making practice; Laloux, *Reinventing Organizations* (2014), Part II, “Self-management (processes)”, the advice process; [Wiki, “Decision Making”](https://reinventingorganizationswiki.com/en/theory/decision-making/), “The advice process.”)

(b) **Whom to ask.** Before acting, the initiator seeks advice from people who will be meaningfully affected and people with relevant expertise. AES and Buurtzorg are presented as examples of distributing decisions with this responsibility. (Bakke, *Joy at Work*, AES practice; Laloux, *Reinventing Organizations*, ch. 2, “The Advice Process,” and AES and Buurtzorg examples; [Wiki, “Decision Making”](https://reinventingorganizationswiki.com/en/theory/decision-making/), “The advice process” and Buurtzorg case.)

(c) **What advice obliges.** The initiator must take advice into consideration; they remain free to decide differently. The process is not a vote and does not require consensus. (Laloux, *Reinventing Organizations*, Part II, “Self-management (processes)”, the advice process; [Wiki, “Decision Making”](https://reinventingorganizationswiki.com/en/theory/decision-making/), “The advice process.”)

(d) **What an objection can do.** In the classic process, an objection is advice to consider, not a veto: no formal objection category blocks the initiator’s decision. The severe-objection stop defined below is a swarm adaptation, not a rule attributed to Bakke or Laloux. (Laloux, *Reinventing Organizations*, Part II, “Self-management (processes)”, the advice process; [Wiki, “Decision Making”](https://reinventingorganizationswiki.com/en/theory/decision-making/), “It’s not consensus.”)

(e) **Record and communication.** Explain the decision and its reasoning to the people consulted, especially when their advice is not taken; this makes responsibility and consequences visible. The sources describe this accountability in practice rather than prescribing a universal log format. (Laloux, *Reinventing Organizations*, Part II, “Self-management (processes)”, the advice process; AES example; [Wiki, “Decision Making”](https://reinventingorganizationswiki.com/en/theory/decision-making/), “The advice process.”)

(f) **Escalation.** The classic process has no required escalation or appeal ladder: the initiator owns the decision after consulting. Further consultation is possible, but escalation is not a veto mechanism. (Laloux, *Reinventing Organizations*, Part II, “Self-management (processes)”, the advice process; [Wiki, “Decision Making”](https://reinventingorganizationswiki.com/en/theory/decision-making/), “The advice process.”)

(g) **Failure risks.** The process can become a formality if people decide before seeking advice, consult only convenient allies, or treat consultation as approval-seeking. Excessive consultation can delay action; an initiator with too little context can misjudge advice; silently overriding advice undermines trust. These are practical failure risks implied by the process’s consultation and accountability requirements, not a separate taxonomy quoted from the books. (Laloux, *Reinventing Organizations*, Part II, “Self-management (processes)”, the advice process; [Wiki, “Decision Making”](https://reinventingorganizationswiki.com/en/theory/decision-making/), “The advice process” and “It’s not consensus.”)

## Our adaptation

For shared specs, cross-project designs, conventions, and protocol changes, the accountable orchestrator or keeper (see the top of this page) is the decider. The classic 'initiator' becomes the one accountable for the result. They must ask everyone meaningfully affected and relevant experts; anyone else may advise. Advice is considered, not binding. (Protocol rule 27.)

Replies are tied to a draft head: `no objections at <sha>` or `objections at <sha>: harm, evidence, fix`, optionally with advice. Silence by the reply deadline is not a veto. A changed draft requires renewed advice on changed sections. (Rule 27.)

Only a **severe objection with evidence** can stop a decision: concrete harm involving safety or security, data loss, an irreversible or external effect, breaking another party’s guarantees, a standing ruling, or an operator’s own rule under rule 14. An objection without evidence is advice. The decider resolves each severe objection explicitly; an unresolved severe objection escalates as an insurmountable case. This stop is our safeguard; the classic process has no veto. (Rules 14 and 27.)

Authority limits the process. Operator rules bind: Ivan decides anything touching Ivan’s machines, repositories, or sessions, and Ivan’s decision is also required for shared protocol changes. Shared protocol and shared-repository policy are decided by both operators. The named arbiter governs a shared resource’s windows and disputes. Advice cannot override an operator’s rule. (Rules 14, 15, 16, and 27.)

The decision is posted as a `decision` and recorded with every objection and its resolution, plus all advice verbatim and whether it was followed or overridden, with reasons. Advice overridden from the other operator’s side is copied to that operator. A push requires advice again on changed sections. (Rule 27.)

The owner is informed by default with a `kind=system` summary. Use `owner-needed` only for an insurmountable case: a hard fork, unresolved severe objection, human-only or irreversible act, or deadlock. (Rules 24 and 27.)

## Failure modes and guards

| Failure mode | Guard in this adaptation |
|---|---|
| Skipping advice | Decider must ask every meaningfully affected party and relevant experts; omission leaves the decision open to challenge. (Rule 27.) |
| Advice theatre or asking only allies | Required parties are defined by scope; advice is recorded verbatim with disposition and reasons. There is no independent check that consultation was candid or sufficiently broad. (Rule 27.) |
| Decision paralysis | Advice is non-binding, silence is not a veto, there is a reply deadline, and only an evidenced severe objection can stop the decision. (Rule 27.) |
| Decider lacks context | Must seek expertise and affected parties; changed sections are re-asked. No explicit guard tests whether the decider has enough context. (Rule 27.) |
| Overriding advice without explanation | All advice is recorded verbatim with followed/overridden status and why; other-operator advice overridden is copied to that operator. (Rule 27.) |
| Severe objection used as a preference veto | It must identify covered concrete harm and evidence; preferences and unsupported objections count as advice. (Rule 27.) |

## Open points

- Rule 27 gives a reply deadline but no default duration; the decider must set one appropriate to the scope.
- There is no explicit independent review for whether all affected parties were found, or whether the decider has adequate context. These remain judgment calls under the current protocol.
- The book citations were written by a remote worker from the public wiki and from memory of the books' structure. Chapter and page references need a check against the print editions.
