# a2a daemon station notice monitor

In an enrolled station, use `a2a watch --notices --listen` with an explicit
`A2A_SESSION_ID` matching the identity's existing Muse owner lease. The engine
renews only that owner's lease. Raw watch is unchanged; `--listen` requires
`--notices`. Installed hook command lines remain unchanged. Configure/uninstall
hooks through the existing local project workflow.

Privately read `<A2A_CONFIG_DIR>/state/daemon-notice-listener.endpoint` (0600,
under a 0700 directory). Its one line is a loopback WebSocket URL, one space,
the generation capability and LF. Never print, log, put in argv, or send this
file to a2a daemon. Upgrade `/` with `Authorization: Bearer CAPABILITY`. No query
is allowed. Exactly one client can connect: second client `daemon_busy`,
invalid request `not_allowed`. Client data is refused; ping/pong is allowed.
Each incoming text frame contains one counts-only L01 JSON projection.

A fixed 30-second window coalesces each burst. Only an exact Matrix sender ID
matching the identity's pinned owner ends it early. Urgency, display names and
localpart resemblance do not. Empty mail emits nothing. Watch never receipts;
call `a2a inbox` to read untrusted body text and complete the normal receipt
barrier. Notice timestamps record successful transport, not controller wakeup.

a2a daemon death or missing keepalives closes with `daemon_unavailable` or
`daemon_idle_timeout` within the 60-second read deadline. Listener death drops
the connection. The native controller owns reconnect/alert decisions. Normal
stop removes the endpoint; SIGKILL can leave one. Verify the listener is stopped
before explicit operator cleanup; startup never overwrites a stale endpoint.
The next successful hook retains its counts-only failure-gap report. The C6
sweep is an independent audit, not a notifier.

**TODO — pinned native Muse fixture:** record the installed version, real
SessionStart/prompt/stop event names and stdin/stdout JSON, plus the monitor
client's WebSocket handshake and close handling. Muse is not installed on the
qualification host; synthetic fixtures make no native Muse E2E or idle-wake
claim. Do not invent a fixture, shim the controller or type into its terminal.
