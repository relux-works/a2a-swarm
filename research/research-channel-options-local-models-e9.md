# Research: agent-to-agent (A2A) channel options for task-board orchestrators

- research: Claude Code research sub-agent, 2026-09-29
- scope: evaluate transports for orchestrator-to-orchestrator messaging (Claude Code and Codex sessions, same or different machines)
- context read: protocol v0 and agent card v0,
  task-board sources (`tools/board-cli/internal/sessionmanager/session_notice.go`, `manager_notice.go`, `cmd/claude_delivery.go`,
  `cmd/codex_delivery.go`, `tools/board-server/internal/auth/service.go`, `docker-compose.coolify.yml`)
- nothing in the repository was modified; no task-board mutation was run
- "unverified" marks any claim not confirmed by a primary source during this research

---

## 1. TL;DR

| Rank | Option | Verdict |
| ---: | --- | --- |
| 1 | **Native task-board topics** (append-only JSONL per topic, CLI `post/read/wait`, cards/claims/subscriptions as board records, tb-sessiond bridge) | Build it. Best fit for provider neutrality, audit, board-condition subscriptions, and the existing notice plane. Main gap: human client is a terminal until a mirror exists. |
| 2 | **Matrix** (Continuwuity/Tuwunel + mautrix-go) | Best off-the-shelf option. Threads, room state for cards, presence, receipts, excellent human clients, very active Go SDK. Use later as a *human mirror* of native topics, or as the channel if humans must be first-class. |
| 3 | **XMPP** (Prosody/ejabberd + mellium.im/xmpp) | Best native presence and PubSub semantics, but reply/thread UX is weak in clients, XEP-0461 is still Experimental, Go library is pre-1.0 with a small team. |
| 4 | **Zulip** | Best conversation model (stream + topic) and human UX, curl-friendly API, but a 5-service Linux stack (≥2 GB RAM) is too heavy for a macOS workstation. Viable only on the existing server host. |
| 5 | **NATS + JetStream** | Best *substrate* (subjects, durable streams, KV with watch/CAS/TTL, strong per-user auth, tiny footprint), but no chat semantics and no human client. Keep as the cross-machine transport under native topics if board-server long-poll is not enough. |
| 6 | **IRC / IRCv3 (Ergo)** | Light single Go binary with history, but threads are only a reply tag, chathistory is still a draft, no structured cards. |
| 7 | **Slack** | SaaS baseline. Great humans UX, but not self-hostable, code and board content leave the machine, rate limits and presence are awkward for bots. |
| n/a | Google/LF **A2A protocol**, **mcp_agent_mail** | Not recommended as the channel; borrow ideas (signed agent cards; advisory file leases, git-auditable mail). |

The deciding observation: most "notify me when X" conditions in this setting are **board-state predicates** (an element landed, a run finished, a claim was released). Any external bus only *routes* messages; something still has to *evaluate* X. The board already owns that state and already owns a wake-up path (tb-sessiond notice queue, Codex `thread/queue/add`). A native channel removes a second source of truth instead of adding one.

---

## 2. Requirements recap and the bridge constraint

R1 topics/threads with replies; R2 durable, auditable record; R3 presence (active/idle/away) plus agent cards (goal, claims, owner);
R4 notify-me subscriptions; R5 negotiation kinds (offer/request/accept/decline/handover) and multi-party decisions with acks;
R6 more than two orchestrators; R7 provider-neutral CLI or small Go library; R8 cross-machine, self-hostable, light on macOS;
R9 human can watch and take part with a normal client; R10 auth, per-agent identity, no secret leakage.

**Bridge constraint.** An idle agent cannot hold a socket. Some long-lived process must hold the subscription and turn an event into a wake-up.
What exists today (verified in source):

- `sessionmanager.EnqueueSessionNotice` writes one JSON file per notice into a per-session inbox (atomic temp+rename, `0600`), and a daemon pump
  (`StartSessionNoticeDelivery`, 1 s tick plus nudge) types a batched notice into the managed Claude PTY only when a client is attached.
  Text passes `ValidateSessionNoticeText`, which refuses native goal acknowledgement markers (`session_notice.go:81`).
- The notice contract is **run-centric**: `enqueue` rejects a notice without `RunID` and `State` (`session_notice.go:257-262`). A2A notices need a
  new contract variant (for example `task-board-session-notice-v2` with `kind: topic`), not a fake run id.
- Codex delivery uses app-server `turn/steer` when a turn is active, else `thread/queue/add`, else `turn/start` (`cmd/codex_delivery.go:330-360`);
  MSG-0001 reports a probe where `thread/queue/add` reached the model without a connected client.
- Free Claude sessions: a background shell command that blocks until a message arrives and then exits re-invokes the session (Claude Code
  `run_in_background` semantics as documented in this runtime's Bash tool). So `task-board topic wait --after <seq>` run in the background *is*
  the "small bridge" for free Claude sessions, as long as the session process is alive. Free Codex TUI sessions: no equivalent verified (unverified).

So the bridge has two halves: (a) **source**: something that learns "a new message for agent A exists" (file watch, long-poll, socket); (b) **sink**:
notice inbox (managed Claude), app-server queue (managed Codex), process exit (free Claude). The sink half is identical for every option below;
the options differ only in the source half and in how much of the message model they give for free.

---

## 3. Option-by-option evaluation

Legend: **N** native, **P** possible with conventions or glue, **M** missing (must be built outside).

### 3.1 XMPP (Prosody or ejabberd; MUC; MAM; PubSub; XEP-0201/0461)

| Req | Mapping |
| --- | --- |
| R1 | **P/N.** One MUC room per topic is native (XEP-0045). Sub-threads via `<thread>`/`parent` per [XEP-0201](https://xmpp.org/extensions/xep-0201.html) (Active, Informational, v1.0, 2010-11-29). Replies via [XEP-0461 Message Replies](https://xmpp.org/extensions/xep-0461.html), which is still **Experimental** (v0.2.x, 2026). In MUC the reply must reference the room-assigned `stanza-id`, not the sender's `id`. |
| R2 | **N.** MAM (XEP-0313) archives MUC history server-side. Prosody 13.0.4 auto-loads `mod_muc_mam` when `mod_mam` is loaded ([release notes](https://blog.prosody.im/prosody-13.0.4-released/)). Audit = server archive; no tamper evidence. |
| R3 | **N (best of all options).** Presence with `show` = `chat/away/xa/dnd` plus free-text status ([RFC 6121 §4.7.2.1](https://datatracker.ietf.org/doc/html/rfc6121#section-4.7.2.1)); MUC occupant presence is broadcast to the room. Cards: vCard4 or a PEP node per agent (P). |
| R4 | **N.** [XEP-0060 PubSub](https://xmpp.org/extensions/xep-0060.html) is **Stable** (v1.30.0, 2025-11-11): nodes, subscriptions, event notifications (Observer pattern). Condition evaluation is still external: someone must publish "X happened" to the node. |
| R5 | **P.** Custom payload element or a structured body convention; acks via message receipts/markers or reactions (P). |
| R6 | **N.** MUC. |
| R7 | **P.** No stock CLI for MUC+PubSub; must ship a small Go CLI. |
| R8 | **N.** Federation-capable, self-host. ejabberd is in Homebrew core (26.07, checked locally with `brew info`). Prosody's Homebrew tap is marked **unmaintained** on [prosody.im/download](https://prosody.im/download/start). |
| R9 | **P.** Gajim added XEP-0461 in 1.9.0 ([Gajim ChangeLog](https://github.com/gajim/gajim/blob/master/ChangeLog)); Monal runs on macOS ([monal-im.org](https://monal-im.org/)) but XEP-0461 support there is unverified. Thread UIs are generally poor. |
| R10 | **N.** SASL SCRAM + TLS, one JID per agent, MUC affiliations. |

- **Self-host weight:** Prosody (Lua) is light; ejabberd (Erlang/BEAM) is heavier but packaged. Low to moderate. Exact RAM unverified.
- **Go clients:** [mellium.im/xmpp](https://pkg.go.dev/mellium.im/xmpp) v0.23.0 (2026-05-04, pre-v1, BSD-2), has `muc`, `pubsub`, `history`, `disco`, `carbons`, `websocket` packages; ~141 GitHub stars, small maintainer group ([GitHub](https://github.com/mellium/xmpp/)). Completeness of `pubsub` (subscription management, not only publish/fetch) is **unverified**. Alternatives: [xmppo/go-xmpp](https://pkg.go.dev/github.com/xmppo/go-xmpp) (simple client, active releases), [FluuxIO/go-xmpp](https://github.com/FluuxIO/go-xmpp) (activity unverified).
- **Bridge design:** the bridge daemon *holds one XMPP session per agent* (it can hold sockets; the agent cannot). It sets presence from tb-sessiond state (idle → `away`, no session → `xa`/unavailable), receives MUC messages and PubSub events, and enqueues notices. This is the most natural bridge fit of all options because XMPP presence is exactly "who holds a live session".
- **Risks:** Experimental replies XEP; pre-1.0 Go library with small bus factor; poor thread UX for humans; XML stanza handling cost for a CLI; two stores of truth (server archive vs board).

### 3.2 Matrix (Continuwuity / Tuwunel / Synapse; mautrix-go)

| Req | Mapping |
| --- | --- |
| R1 | **N.** Room per topic; `m.thread` relations since Matrix v1.4, `GET /rooms/{roomId}/threads`, thread-scoped receipts ([v1.4 release](https://matrix.org/blog/2022/09/29/matrix-v-1-4-release/), [MSC3440](https://github.com/matrix-org/matrix-spec-proposals/blob/main/proposals/3440-threading-via-relations.md)); no nested threads. Replies via `m.in_reply_to`. Current spec v1.19 ([spec](https://spec.matrix.org/latest/client-server-api/)). |
| R2 | **N.** Event DAG persisted server-side; redactions are visible as events. No external tamper evidence. |
| R3 | **N/P.** Presence `online/unavailable/offline` + `status_msg` (spec presence module). Continuwuity has `allow_local_presence` (default on) and federation presence switches ([config](https://continuwuity.org/reference/config)). Cards: **custom state events** (`type: dev.relux.agent_card`, `state_key: <agent-id>`) are a clean native fit; current state is queryable and history is kept. |
| R4 | **P.** No condition subscriptions. Agents join a "notify" room or DM; the promising agent posts. Push rules exist for human clients only. |
| R5 | **P.** Custom `msgtype`/content fields; acks via reactions (`m.annotation`) or read receipts; decisions via a state event listing required ackers. |
| R6 | **N.** |
| R7 | **P.** HTTP+JSON, curl-able (`PUT /rooms/{id}/send/{type}/{txnId}`), but sync-based reading is awkward from a CLI; a small Go CLI is needed. |
| R8 | **N.** Continuwuity: Rust single binary with embedded RocksDB, very active (v26.6.0, 700+ commits in the release) ([intro](https://continuwuity.org/introduction), [TWIM 2026-07-17](https://matrix.org/blog/2026/07/17/this-week-in-matrix-2026-07-17/)); Tuwunel is the other conduwuit successor, sponsored by Switzerland ([GitHub](https://github.com/matrix-construct/tuwunel)). Native macOS binaries: unverified (Docker or Nix static build documented, [deploy docs](https://continuwuity.org/deploying/generic)). Federation can be disabled. |
| R9 | **N (excellent).** Element (web/desktop/mobile) supports threads; gomuks is actively developed by the mautrix author ([blog](https://mau.fi/blog/2026-09-mautrix-release/)). |
| R10 | **N.** Per-user access tokens, room power levels, optional E2EE (avoid for bots in the pilot; goolm is still maturing per [April 2026 notes](https://mau.fi/blog/2026-04-mautrix-release/)). |

- **Self-host weight:** Continuwuity/Tuwunel light (single process); Synapse (Python + Postgres) moderate to heavy.
- **Go clients:** [mautrix-go](https://github.com/mautrix/go/blob/main/CHANGELOG.md) v0.31.0 released 2026-09-16 with a monthly release train ([Sept 2026](https://mau.fi/blog/2026-09-mautrix-release/)); pre-1.0 with regular breaking changes, but heavily used in production by the mautrix bridges.
- **Bridge design:** one Go daemon, either an **appservice** (single registration, receives all room events by HTTP push, can puppet agent users) or one `/sync` long-poll per agent. Maps presence from tb-sessiond; enqueues notices. Very natural.
- **Risks:** protocol surface (sync filters, txn ids, state resolution) is large for what we need; bot presence is often disabled or noisy on busy servers; another source of truth next to the board; server upgrade churn (conduwuit fork history is a warning sign).

### 3.3 Zulip

| Req | Mapping |
| --- | --- |
| R1 | **N (best model).** Channel (stream) + topic is first-class; topics can be renamed, moved, marked resolved. |
| R2 | **N.** Postgres; edit history retained (policy-dependent). |
| R3 | **N/P.** Presence `active/idle` ([update-presence](https://zulip.com/api/update-presence)); bot presence behavior unverified. Cards via custom profile fields (P). |
| R4 | **P.** Outgoing webhooks / real-time event queues; no condition subscriptions. |
| R5 | **P.** Conventions; emoji reactions for acks; polls widget. |
| R6 | **N.** |
| R7 | **N for posting.** Plain curl with bot email + API key over Basic auth ([send-message](https://zulip.com/api/send-message)); event queue register + long-poll get-events ([REST API](https://zulip.com/api/rest)). Go client is community-only ([wakumaku/go-zulip](https://pkg.go.dev/github.com/wakumaku/go-zulip)). |
| R8 | **Heavy.** Production support only on Ubuntu 22.04/24.04/26.04 and Debian 12/13; x86-64 and aarch64; ≥2 GB RAM (+2 GB swap); components Postgres, RabbitMQ, Redis, memcached, nginx, Django/Tornado ([requirements](https://zulip.readthedocs.io/en/stable/production/requirements.html)). macOS only via Docker ([docker-zulip](https://github.com/zulip/docker-zulip)). |
| R9 | **N (excellent).** Web, desktop, mobile, terminal client. |
| R10 | **N.** Per-bot API keys, channel permissions. |

- **Bridge design:** one daemon registers an event queue per agent (or one admin queue) and long-polls; natural.
- **Risks:** weight on a workstation; Python/Django ops; community Go client; Zulip Cloud would move data off-site.

### 3.4 IRC with IRCv3 (Ergo)

| Req | Mapping |
| --- | --- |
| R1 | **P.** Channel per topic. Replies only through the client tag `+reply` ([spec](https://ircv3.net/specs/client-tags/reply)); "threads" are a client rendering choice. |
| R2 | **P.** `draft/chathistory` is still a **work-in-progress** spec ([spec](https://ircv3.net/specs/extensions/chathistory)); Ergo implements it with persistent history (MySQL optional). |
| R3 | **P.** `AWAY` + `away-notify` only; no structured cards (metadata extension is a draft; unverified in Ergo). |
| R4 | **M.** |
| R5 | **P.** Text conventions only. |
| R6 | **N.** |
| R7 | **P.** Line protocol is scriptable; Go libs exist (ergochat/irc-go, lrstanley/girc; activity unverified). |
| R8 | **N (lightest chat server).** Ergo is a single Go binary with macOS arm64/x86_64 builds; v2.19.1 on 2026-08-05 ([releases](https://github.com/ergochat/ergo/releases)); always-on clients replay missed messages per device ([manual](https://github.com/ergochat/ergo/blob/master/docs/MANUAL.md)). |
| R9 | **N.** Any IRC client; history-aware ones (Goguma, gamja, Kiwi). |
| R10 | **P.** SASL accounts + TLS; channel modes are coarse. |

- **Bridge design:** bridge keeps an always-on account per agent; Ergo's per-device replay makes "catch up on wake" easy.
- **Risks:** weak structure (everything is a text line, 512-byte limits without the multiline draft), no cards, no subscriptions.

### 3.5 NATS with JetStream

| Req | Mapping |
| --- | --- |
| R1 | **P.** Subjects `a2a.<board>.topic.<topic>.msg`; a thread is a field (`re`), or a subject token (`...topic.<topic>.thread.<root>`). Wildcards give "all topics" views. |
| R2 | **N.** JetStream file streams with retention, sequence numbers, per-subject last-value. Tamper evidence must be added (hash chain in payload). |
| R3 | **N.** KV bucket `agents` with per-key TTL for heartbeats, watch for live updates, CAS by revision for card updates ([KV docs](https://docs.nats.io/nats-concepts/jetstream/key-value-store)). |
| R4 | **N as routing.** Durable consumers with subject filters are exactly "deliver matching messages to A even if A was away". Condition evaluation still external. |
| R5 | **P.** Payload schema; acks as messages; decisions as a KV entry with ack set (CAS). |
| R6 | **N.** |
| R7 | **N.** Official [nats.go](https://docs.nats.io/using-nats/developer/develop_jetstream/kv) and the `nats` CLI can publish/read from shell. |
| R8 | **N (lightest).** Single Go binary, Homebrew core `nats-server` 2.15.0 (local `brew info`; [release 2026-09-17](https://github.com/nats-io/nats-server/releases)); leaf nodes/clustering for cross-machine. JetStream memory grows with subject and dedup cardinality ([issue #8485](https://github.com/nats-io/nats-server/issues/8485), [Synadia](https://www.synadia.com/blog/nats-jetstream-high-ram-usage)). |
| R9 | **M.** No chat client. Humans would use `nats sub` or a custom TUI. |
| R10 | **N (strong).** NKeys/JWT decentralized auth, per-user publish/subscribe subject permissions, auth callout ([JWT auth](https://docs.nats.io/running-a-nats-service/configuration/securing_nats/auth_intro/jwt), [auth callout](https://docs.nats.io/running-a-nats-service/configuration/securing_nats/auth_callout)). |

- **Bridge design:** trivially natural: one durable consumer per agent inside the bridge daemon; ack on successful notice enqueue gives at-least-once delivery.
- **Risks:** we would build the whole message model, cards, and human client anyway; it becomes a second store next to the board.

### 3.6 Slack (SaaS baseline)

- R1 N (threads), R2 N (retention per plan), R3 P (bots cannot really express active/idle; presence API is limited; unverified detail), R4 M, R5 P (reactions, Block Kit buttons), R6 N, R7 P (Web API via curl; Go lib `slack-go/slack`), R8 **M** (not self-hostable), R9 N (best), R10 N (per-app tokens, scopes) but all content leaves the machine.
- Rate limits: since 2025-05-29 / 2025-09-02, `conversations.history` and `conversations.replies` for *commercially distributed non-Marketplace* apps are limited to 1 request/minute and 15 objects; **internal customer-built apps are exempt** ([changelog](https://docs.slack.dev/changelog/2025/05/29/rate-limit-changes-for-non-marketplace-apps/), [clarification](https://docs.slack.dev/changelog/2025/06/03/rate-limits-clarity/)).
- Bridge: Socket Mode daemon (outbound websocket, no public endpoint) → notices. Natural.
- Risks: data residency, secrets and code excerpts in a third-party SaaS, vendor API churn, cost.

### 3.7 Native task-board topics

Design assumed here: per-topic append-only JSONL (`topics/<topic>/log.jsonl`), one record per line with `seq`, `id`, `ts`, `from`, `to[]`, `kind`, `re`, `thread`, `body`, `needs_ack[]`, `prev_hash`, `hash`; appends serialized by a lock; CLI `task-board topic create|post|read|wait|list`, `task-board agent card set|get`, `agent presence beat`, `topic subscribe`.

| Req | Mapping |
| --- | --- |
| R1 | **N (by design).** Topic = file; `re` for replies, `thread` for sub-threads. |
| R2 | **N (strongest).** Append-only, same store as the board, hash chain gives cheap tamper evidence; can reference board elements and runs by id. |
| R3 | **N/P.** Cards are board records (goal from the goal plane, claims from element ownership/leases, owner from config) - most fields can be *derived*, not self-reported. Presence: tb-sessiond already knows managed session liveness and attachment; free sessions heartbeat with TTL. |
| R4 | **N (only option that evaluates conditions).** The board can evaluate "element E reached status S", "run R terminal", "claim released", "message in topic T with kind K addressed to me" itself, and deliver through the existing notice plane. Free-form conditions fall back to "the promising agent posts". |
| R5 | **N.** `kind` enum from protocol v0 extended with `offer/request/accept/decline/handover`; `decision` records carry `needs_ack`; `topic status` shows missing acks. |
| R6 | **N.** |
| R7 | **N (best).** The `task-board` CLI is already installed and used by every Claude and Codex session; no new credentials or binaries. |
| R8 | **P.** Same machine: native. Cross-machine: via the existing `board-server` remote backend (`tools/board-server`, token auth with per-board roles in `internal/auth/service.go`, `internal/remote` client) by adding topic routes plus a long-poll `wait` endpoint. No new server to host. |
| R9 | **P/M.** Terminal only at first (`task-board topic read --follow`). A read-only web view on board-server, or a later Matrix mirror, closes the gap. |
| R10 | **P.** Cross-machine: board-server tokens per agent. Same machine: all agents share one Unix user, so `from` is advisory unless the writer is attested (tb-sessiond knows its session ids) or records are signed (for example a per-agent non-extractable P-256 key, as `mac-keyvault` already provides). Secret redaction can run at `post` time. |

- **Self-host weight:** zero new services locally; board-server for cross-machine.
- **Bridge design:** tb-sessiond watches topic logs (kqueue/FSEvents or its existing 1 s tick), keeps a per-subscriber cursor, and enqueues a **doorbell only** (topic, seq, from, kind, sanitized one-line summary, `next_command: task-board topic read ...`) through a new notice contract kind. Codex managed sessions reuse the `thread/queue/add` path. Free Claude sessions use a background `task-board topic wait`. Cross-machine sessions long-poll board-server from their local tb-sessiond.
- **Risks:** we own the design (scope creep toward "building a chat app"); lock contention if appends take the global board mutation lock (use a per-topic lock, see §5); file growth (segment and rotate); human UX until a mirror exists.

### 3.8 Others considered

- **Agent2Agent (A2A) protocol** (Google, now a Linux Foundation project): JSON-RPC 2.0 over HTTP(S), SSE streaming, webhook push, signed Agent Cards, Go SDK `github.com/a2aproject/a2a-go` ([repo](https://github.com/a2aproject/A2A), [LF announcement](https://www.linuxfoundation.org/press/linux-foundation-launches-the-agent2agent-protocol-project-to-enable-secure-intelligent-communication-between-ai-agents)). It assumes each agent is an always-on HTTP server handling tasks point-to-point; no multi-party topics. Not a fit, but its **Agent Card** shape is worth mirroring for our cards. Version claims (v1.0 in 2026) are **unverified**.
- **mcp_agent_mail** ([Dicklesworthstone/mcp_agent_mail](https://github.com/Dicklesworthstone/mcp_agent_mail)): identities, inboxes, threads, advisory TTL file leases with a pre-commit guard, Git+Markdown for audit and SQLite FTS for search, served over MCP (Python; Rust port exists). Closest prior art to our need; not adopted because it duplicates the board's claims/leases and is MCP-first. Borrow: git-auditable message artifacts, advisory file reservations enforced by a hook.
- **ntfy** (single Go binary, HTTP publish, topic ACLs and tokens, CLI subscribe) ([docs](https://docs.ntfy.sh/subscribe/cli/), [config](https://docs.ntfy.sh/config/)): a good *human phone doorbell* (e.g. decision needs Alexis), not a thread store.

---

## 4. Summary matrix

| Req | Native topics | Matrix | XMPP | Zulip | NATS | IRC/Ergo | Slack |
| --- | --- | --- | --- | --- | --- | --- | --- |
| R1 topics/threads | N | N | P (0461 exp.) | N (best) | P | P | N |
| R2 durable/audit | N (hash chain) | N | N | N | N | P (draft) | N (plan) |
| R3 presence+cards | N/P (derived) | N/P | N (best presence) | N/P | N (KV) | P | P |
| R4 notify-me | N (evaluates X) | P | N routing | P | N routing | M | M |
| R5 negotiation/acks | N | P | P | P | P | P | P |
| R6 multi-party | N | N | N | N | N | N | N |
| R7 CLI/Go lib | N (existing CLI) | P (mautrix-go) | P (mellium pre-1.0) | N curl / P Go | N | P | P |
| R8 x-machine, light | P (board-server) | N (light server) | N | heavy | N (lightest) | N | M |
| R9 human client | P/M | N (best) | P | N (best) | M | N | N (best) |
| R10 security | P (attest/sign) | N | N | N | N (strongest) | P | N but SaaS |
| Bridge fit | existing plane | appservice/sync | holds sessions | event queue | durable consumer | always-on | Socket Mode |

---

## 5. Ranked recommendation and rationale

1. **Native task-board topics.** The only option that satisfies R4 for real (the board evaluates board conditions), R7 with zero new tooling, and R2 in the same store as the work it discusses. The wake-up path already exists for both providers; the change is a new notice kind, not a new system. Cross-machine rides the existing board-server auth. Design notes:
   - Do **not** append under the repo-wide `task-board.config.json.mutation.lock` for every message; that serializes chat with all board mutations of ~7 live runs. Use a per-topic lock (flock on `log.jsonl.lock`) for append + seq assignment, and take the board lock only for records that change board state (claims, handover acceptance that reassigns an element).
   - Deliver a **doorbell, not the body**. Message bodies from other agents are untrusted data; the doorbell line passes `ValidateSessionNoticeText` / sanitization (goal-ack markers) and a secret scrubber, and the agent reads the body with the CLI. This limits prompt injection and PTY-marker collisions.
   - Keep a transport seam (`TopicStore` interface: append, read-after-seq, wait) so a Matrix mirror or NATS backend can be added without changing the CLI.
2. **Matrix (Continuwuity + mautrix-go)** as the phase-2 human mirror: a one-way (then two-way) appservice that mirrors each topic to a room and each thread to `m.thread`, cards to state events. If humans must be first-class participants from day one, Matrix becomes rank 1 with the native store demoted to the audit log.
3. **XMPP** if presence semantics dominate; otherwise the Experimental replies XEP, weak thread UX, and a small pre-1.0 Go library make it riskier than Matrix.
4. **Zulip** only if it runs on the existing server host (not the workstation); its stream+topic model is the best UX to copy.
5. **NATS** as the cross-machine substrate if board-server long-poll proves insufficient (many machines, sub-second push). It would sit *under* native topics.
6. **IRC/Ergo**, 7. **Slack**: acceptable for humans, poor for structure, subscriptions, or self-hosting.

---

## 6. Minimal pilot plan for native topics (1-2 days)

### Components

1. **Store** (`pkg/board` or a new `internal/topics`): `topics/<topic>/log.jsonl`, record schema above, per-topic flock, `O_APPEND` + fsync, hash chain, reader that tolerates a truncated last line (reports it, never drops earlier records).
2. **CLI:**
   - `task-board topic create <topic> --title ... --members a,b,c`
   - `task-board topic post <topic> --kind proposal|question|answer|offer|request|accept|decline|handover|decision|ack|notice [--re <id>] [--to a,b] [--needs-ack a,b] --body-file -`
   - `task-board topic read <topic> [--after <seq>] [--thread <id>] [--format json|md]` and `--follow`
   - `task-board topic wait <topic|--mine> --after <seq> --timeout 30m` (exits 0 on new message: the free-session bridge)
   - `task-board topic status <topic>` (open decisions and missing acks)
   - `task-board agent card set|get` (derived fields from goal plane and element ownership; self-declared fields flagged as such), `task-board agent beat --status active|idle|away --ttl 10m`
   - `task-board topic subscribe --when 'element:<ID> status=done' | 'topic:<T> to=me' --notify <agent>` (pilot supports only these two predicate kinds)
3. **Bridge:** tb-sessiond watcher: on new records, per subscriber cursor, enqueue a `kind: topic` notice through a v2 notice contract (keeps run-centric v1 validation intact); Codex managed sessions through the existing delivery function. Cursor advances only after successful enqueue (at-least-once; the agent dedups by `seq`).
4. **Seed:** import `orchestrator-channel.md` MSG blocks as records to prove the model on real data.

Day 1: store + post/read/wait + card/beat + tests. Day 2: bridge + subscribe (two predicates) + decision/ack status + import + measurements.

### What to measure

| Metric | Target / question |
| --- | --- |
| post → wake latency, managed Claude / managed Codex / free Claude (`wait`) | p50 and p95; expect ~1-2 s for managed (pump tick) |
| lost or duplicate deliveries | 3 concurrent writers x 500 posts, 3 subscribers: zero lost, duplicates only on crash-restart and deduped by seq |
| board mutation latency impact | unchanged vs baseline when topic appends use per-topic lock |
| doorbell token cost per wake | compare with current SendMessage doorbell and full-file resource rewrite |
| human followability | Alexis can follow and reply with `topic read --follow` / `post` without help |
| store growth | bytes per message; rotation threshold |

### Negative tests (required, not optional)

- forged `from` on a managed session is rejected or flagged (attestation mismatch); positive control: genuine post accepted
- body containing a goal-ack marker (`Goal set:`) never reaches a PTY verbatim; doorbell still delivered in sanitized form
- body containing a token-like secret is refused or redacted at `post`; a normal body with similar-looking non-secret text passes
- truncated last line: reader reports corruption and still returns all earlier records; next append does not corrupt further
- stale or future cursor: `wait --after <seq beyond head>` errors clearly instead of blocking forever
- decision without all acks is reported as open; an ack from a non-listed agent does not close it
- subscription for an element the subscriber cannot see (remote board without access) is refused

---

## 7. Open questions (for the owner)

1. Is a terminal-only human view acceptable for the pilot, or must a normal chat client exist from day one (which would move Matrix to rank 1)?
2. Where should topic logs live: inside the board data directory (versioned with the board) or in the session-manager state directory (not versioned)? Versioning gives audit but bloats git.
3. Is per-agent signing (keychain-held P-256 keys) wanted now, or is attestation by tb-sessiond enough on one machine?
4. Should a free Codex TUI session be supported, or are Codex sessions always managed (app-server)? No verified wake path exists for free Codex TUIs.

## 8. Unverified items

- Continuwuity/Tuwunel native macOS binaries; mellium `pubsub` completeness and maintainer count; Monal XEP-0461 support; Zulip bot presence; IRC metadata extension in Ergo; exact RAM footprints of Prosody, ejabberd, nats-server, Continuwuity; A2A spec version numbers and dates; free Codex TUI wake-up path.
