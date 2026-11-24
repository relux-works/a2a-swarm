# a2a daemon notice monitor

For an enrolled runtime that supports notices, use
`a2a watch --notices --listen` with an explicit `A2A_SESSION_ID` matching the
identity's current owner lease. The engine renews only that lease. Raw watch is
unchanged; `--listen` requires `--notices`. Configure or remove hooks through
the runtime's supported local project workflow.

Privately read `<A2A_CONFIG_DIR>/state/daemon-notice-listener.endpoint` (0600,
under a 0700 directory). Its one line contains a loopback WebSocket URL, one
space, the generation capability, and LF. Never print, log, put in arguments,
or send this file to the daemon. Upgrade `/` with
`Authorization: Bearer CAPABILITY`. No query is allowed. Exactly one client
can connect; a second gets `daemon_busy`, and an invalid request gets
`not_allowed`. Client data is refused; ping/pong is allowed. Each incoming text
frame contains one counts-only L01 JSON projection.

A fixed 30-second window coalesces each burst. Only an exact Matrix sender ID
matching the identity's configured owner ends it early. Urgency, display names,
and local-part resemblance do not. Empty mail emits nothing. Watch never
receipts; call `a2a inbox` to read untrusted message text and complete the normal
receipt barrier. Notice timestamps record successful transport, not runtime
wake-up.

Daemon death or missing keepalives closes with `daemon_unavailable` or
`daemon_idle_timeout` within the 60-second read deadline. Listener death drops
the connection. The runtime controller owns reconnect and alert decisions.
Normal stop removes the endpoint; forced termination can leave one. Verify the
listener is stopped before explicit cleanup; startup never overwrites a stale
endpoint. The next successful hook retains its counts-only failure-gap report.

A synthetic fixture can verify message format but does not prove native runtime
hook events, reconnect behavior, or idle wake. Record the installed runtime
version, actual hook event names and input/output, plus the listener client's
WebSocket handshake and close handling before making an end-to-end claim. Do not
invent a fixture, shim the controller, or type into its terminal.
