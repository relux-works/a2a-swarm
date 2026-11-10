# Local monitor owner association

The local hook path records `monitor_owner` in the selected identity's sealed
State. It requires explicit `--as`, the existing identity guard, explicit
`--runtime muse|claude|codex`, and stdin JSON containing an exact
`hook_event_name` and a `session_id` matching `[A-Za-z0-9._-]{1,128}`. Ambiguous
or case-aliased evidence fields cannot admit an owner. Legacy hook inputs
without an event name retain their delivery behavior but supply no owner proof.

`SessionStart` establishes the owner; a newer Start replaces it, including a
restart with the same session id. A Start whose processing time is equal to or
before the prior `last_hook_at` fails explicitly with `owner_unknown`, leaving
State unchanged. It never reports success while retaining the old association.
This conservative choice preserves timestamp-based same-session binding: after
a clock correction, an operator must wait for a strictly later processing time
before Start can re-admit. The record contains `runtime`, the SHA-256
`session_id_hash`, `first_seen`, and `last_hook_at`. An admitted prompt or normal
Stop from that owner refreshes `last_hook_at`. Hooks from an older session
cannot refresh or end its replacement. Hooks maintain association while
notifications are paused, without acquiring a lease or contacting a server.

`a2a --as NAME hook session-end --runtime RUNTIME` accepts stdin
`{"session_id":"SESSION","hook_event_name":"SessionEnd"}` and ends that
session's association. A Stop with explicit stdin `"stop_reason":"exit"`
also ends it. Normal Stop does not imply exit. The exit marker is a local input
contract; native runtime emission of that marker has not been qualified.
Existing installers continue installing their qualified Start/prompt/Stop
events; SessionEnd wiring requires the runtime's own supported lifecycle hook.

The first admitted Start materializes `monitor_config.owner_ttl` with a default
of 12 hours (`43200000000000` nanoseconds, the State duration encoding).
Local policy may set another positive duration. At the exact TTL boundary the
owner is unknown. A prompt or Stop cannot revive an expired association; only
the next Start re-admits it.

The internal monitor generation checks its pinned owner's runtime, session
hash, admission time and freshness before notice admission or renewal.
Unknown ownership disarms the handle, emits no notice, renews nothing, and
reports `owner_unknown` through its status API. Disarming preserves pending
mail and the existing lease. Status checks are also a disarm boundary for
invalid integrity, generations or ownership. A transient control/State lock
timeout instead returns `(false, "lock_timeout")` for that observation, retaining
the handle and its consumer lock; the next Status or effect rechecks State.
Effectful operations keep their fail-closed timeout behavior. It retains the monitor row under the existing
generation fence; admitting a new owner does not reactivate an old handle.
The conditional renewal transaction checks the sealed State and generation,
owner association, exact lease holder and host, and unexpired lease while
holding the existing control and State locks. It changes only lease expiry;
it never acquires, takes over, or force-releases a lease.

The CLI hook shim uses error unwrapping for non-Start `owner_unknown` refusals.
A wrapped malformed owner State falls through to legacy delivery, whose own
State and lease checks still apply and reject that malformed State. Start
returns the error. Neither path repairs bytes or grants monitor authority.

Watch, sweep, bind and service paths cannot write an owner through ordinary
State writer APIs. There is currently no production sweep command; the CLI
refuses both `sweep` and `hook sweep` without writing owner metadata. Environment session ids, PIDs, process birth, terminal
observations and stored bind associations supply no owner evidence. Existing
monitor metadata validation remains a separate generation constraint. This
record proves what the local runtime declared under the host account's trust;
it does not authenticate beyond that account or distinguish callers sharing
its UID. No a2a daemon station channel or live monitor command is enabled here.

Run local owner and fence tests with
`go test ./internal/config ./internal/cli -run 'TestMonitor' -count=1`.
These fixtures exercise the actual disk locks, sealed writers, hook admission,
pending mailbox, and sink guard. They do not prove native hook installation,
runtime exit emission, external notice delivery, or a2a daemon authentication.
