# a2ad service, external sweep and rescue accounting (C6)

This repository provides service rendering and explicit local service management.
Host provisioning, enabling these units on a real host, and native boot/wake
qualification belong to the operator and host keepers. Unit tests use rendered
files and temporary directories with metadata/supervisor seams. A rendered
fixture is not evidence of a native boot or of installed Muse support.

## Public plan and immutable owner policy

`a2a daemon service install|uninstall|status --plan ABSOLUTE_FILE` requires root.
The plan must be a root-owned, singly linked regular 0644 file in a root-owned
0755 directory with root-controlled ancestors. Unknown fields, duplicate keys,
case aliases, nulls and trailing input refuse. No command escalates privileges.
All supervisor subprocesses are confined to these three explicit operations.
Raw supervisor output and endpoint contents are never printed. Successful
operations output only unit names and closed state tokens.

The complete plan shape is:

```json
{
  "version": 1,
  "platform": "darwin",
  "image": "/opt/a2a/image/a2a",
  "image_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "home": "/private/var/a2a",
  "owner_pin": "/Library/ApplicationSupport/a2a/owner.json",
  "owner_pin_sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "uid": 401,
  "gid": 401,
  "sweep_seconds": 300,
  "soak_days": 3
}
```

These hashes are inert examples. Infra's reviewed provisioning script supplies
real paths and SHA-256 values, `_a2a` uid/gid 401 on macOS, and the equivalent
`a2a` system account on Linux. Linux plans use `platform: "linux"` and that
account's actual uid/gid. The script also provisions the migrated private daemon
home; install does not create users, migrate identities or write owner policy.
Paths use a restricted absolute grammar without whitespace or shell/XML/systemd
metacharacters. They must have no symlink components. The executable must be
root-owned 0755, singly linked, in a root-owned 0755 directory. Its SHA-256 is
verified at install and at startup by the image itself.

The owner-policy file shape is:

```json
{"version":1,"owner":"@ivan:relux.works","identities":["local-identity"]}
```

Infra chooses its location; C6 does not impose an alternative writable layout.
The file must be root:service-group 0440, regular and singly linked, in a
root-owned 0755 directory. Ancestor symlinks and agent-writable ancestors refuse.
At non-root startup the effective-access check also rejects ACL write grants
to the service account on the policy, image or their parent directories.
The separate descriptor ACL check also runs as root. Darwin accepts read-only
permits and deny entries, and rejects any write-capable permit; malformed or
unreadable ACL data refuses. Linux conservatively requires no extended/default
POSIX ACL on trust paths (including no read-only extended ACL) and uses kernel
`faccessat2` without the ACL-blind emulation fallback. Unsupported access checks
fail closed. Native ACL syscall behaviour still needs host precheck evidence.
Missing files, bad ownership/mode, duplicate identities and hash mismatches
refuse with the closed `daemon: not_allowed` diagnostic before socket creation
or heartbeat publication. No service command writes the policy file.

The verified snapshot is the service's owner and allowed-identity authority.
Authenticated registry entries outside its allowed set cannot execute commands.
Daemon request path resolution carries the pinned owner through local and named
connection resolution. The mutable per-identity owner pin and principals files
cannot replace that owner. Local non-daemon commands retain their own existing
pin behaviour. Changing the root policy requires a new reviewed hash and a
restart; no station command can edit it or supply a replacement policy.

## Rendered units and bounded root preparation

The only unit grammar is `internal/daemonsvc.Render`; static validation requires
byte equality with that grammar. Checked-in rendered examples live in
`internal/daemonsvc/testdata/{darwin,linux}`. Their hashes intentionally cannot
install a real image. No environment, credential, capability, arbitrary argv or
additional socket can enter through the public plan. The daemon exposes the
fixed station socket; C7's admin socket is absent. The station's existing native
notice websocket remains station-owned and is not a service endpoint.

macOS installs three LaunchDaemons in `/Library/LaunchDaemons`:

| Label | User | Scheduling |
| --- | --- | --- |
| `works.relux.a2a.a2ad` | `_a2a` | RunAtLoad, KeepAlive, ThrottleInterval 30 |
| `works.relux.a2a.a2ad.prepare` | root | RunAtLoad, retry failed preparation at throttle 30 |
| `works.relux.a2a.a2ad.sweep` | `_a2a` | RunAtLoad, StartInterval from the explicit plan (normally 300 s) |

C6 uses the brief's bounded preparation fallback. The existing C5 server owns
bind, permission setting and inode-pinned cleanup; directly adopting a
launchd-created socket would bypass those invariants. A Sockets declaration
also supplies no implementation of the mandatory exact parent-directory
ownership/refusal check. No unqualified inherited-fd/cgo path is introduced.
The fallback preserves the C5 transport and performs only root run-dir setup.

`a2a daemon prepare-run-dir --image-sha256 HASH` is macOS/root-only and targets
only `/private/var/run/a2a`. It checks the pinned image and provisioned account,
then opens the root-controlled parent without following symlinks. If absent,
it creates a private sibling, sets only that new inode to 401:401/0711 and
publishes with exclusive rename. Existing symlinks, files, wrong owner/group or
any mode other than 0711 or an ACL write grant refuse without changes. A correct existing directory
keeps its inode and metadata. No existing object is adopted or chmodded.

The named platform policy applies only to this run-dir parent, not arbitrary
policy/image directories or their ancestors:

| Platform / parent | UID | GID | Exact modes |
| --- | --- | --- | --- |
| macOS `/private/var/run` | 0 | 0 (`wheel`) or 1 (`daemon`) | 0755 or 0775 |
| Linux `/run` | 0 | Any; group write forbidden | 0755 |

Source's read-only native evidence on ivmbp, macOS 27.2 build 26B123, is
`root:daemon 0775`, uid 0/gid 1, with no ACL; the daemon group is gid 1.
World write, unlisted macOS groups, non-root ownership, ancestor group write,
symlinks and ACL write grants refuse. No system parent is chmodded or chowned.
Linux preserves the earlier uid-0/0755 assumption; Linux creation is normally
owned by systemd RuntimeDirectory, not this macOS-only CLI command.

The daemon refuses an absent run directory before readiness and never creates
it as `_a2a`. Launchd boot ordering can start it before preparation; KeepAlive
and throttle retry after root preparation. Removing the boot-volatile directory
must not be interpreted as a provisioning-manifest mismatch by Infra's `--check`;
that operator-script change remains with Infra.

Linux installs `works.relux.a2a.a2ad.service`, `.sweep.service` and `.sweep.timer`
in `/etc/systemd/system`. The main service has User/Group `a2a`,
`Restart=on-failure`, `RestartSec=30`, `RuntimeDirectory=a2a`, mode 0711 and
`RuntimeDirectoryPreserve=yes`. Its timer uses the explicit plan cadence.
Both platforms retain a 10-second supervisor exit timeout as a final OS bound;
C5's 5-second cancellation/lock-release contract remains the application bound.

Service restarts hold `.daemon.lock` before checking the fixed socket. Only an
owned, singly linked 0666 socket that returns `ECONNREFUSED` from a bounded
connect probe is reclaimed, with an inode recheck through the held directory.
Live sockets, unknown connect errors, wrong metadata, files and symlinks refuse.
The library's unsupervised C1/C5 endpoint retains its strict no-adoption rule.

## Install, uninstall and status

Install checks account, image, policy, every existing unit and every supervisor
state before writing. A partial unit set or different unit bytes refuse.
Missing files are created exclusively at root-owned 0644. Install registers
and starts matching units, starts a matching stopped unit, and waits up to
40 seconds for the main service (and Linux timer) to become running. It reports
resulting states rather than claiming registration proves readiness. Failures
leave inspection/recovery evidence; install does not perform a speculative
rollback that might destroy a live job.

Uninstall audits the entire set first. Unknown/uncertain state, foreign loaded
argv, user/group, fragment path, stale systemd configuration or mismatching
files return `daemon_unavailable; service unresolved` without deleting files.
A matching running or stopped unit is stopped and joined. The Linux timer is
stopped before its sweep worker. Launchd bootout additionally joins the captured
PID without signalling arbitrary processes. Files are removed only after every
job is known quiescent and their bytes are rechecked. I/O failure during final
removal can leave a partial set and remains unresolved; no success is fabricated.

Supervisor parsing is deliberately conservative. Launchd requires the expected
path, program, username, argv and stable state from `launchctl print`.
Only its explicit service-not-found result proves absence. Systemd requires
loaded fragment, exact argv, service account, `NeedDaemonReload=no` and stable
active/dead/failed substates. During post-install readiness only, known matching
`activating` start substates are tolerated and main/timer units continue to be
polled until running. This does not allow transitional uninstall. Permission
failures, timeouts and unfamiliar
responses refuse; there is no "force" or "assume stopped" flag. These native
output shapes still need host precheck recording; synthetic parser fixtures do
not qualify an installed service manager version.

## Heartbeat and external read-only sweep

Service readiness opens `<home>/heartbeat` as an owned 0600, no-follow, singly
linked regular file, holds an exclusive lifetime flock and pulses its mtime
immediately and every 30 seconds. Readiness is after the serve lock and socket
bind. Heartbeat I/O failure cancels the service; a failed startup cannot publish
a fresh heartbeat. Shutdown cancels and joins the heartbeat loop.
Pulse timestamps are floored to microseconds before the OS timeval conversion.
Opening the lifetime lock retries only EAGAIN/EWOULDBLOCK, at 10ms intervals
for at most 100ms, so the sweep's short probe cannot spuriously fail startup
when it releases within that bound. Persistent contention and other errors
refuse. Unit file create/remove and first rescue-log creation sync their parent
directory before reporting success; fsync failures remain unresolved/refused.

The separate sweep runs as the service account, verifies the same pinned image
and root policy, and writes `<home>/sweep-report.json` at 0600 with atomic rename.
It sees a missing/unsafe heartbeat, released lifetime lock, future timestamp or
age of at least 90 seconds as `daemon_down` (a report token, never a wire class).
A killed process releases its lock, so even a fresh pulse cannot postpone death
detection beyond the next completed sweep. The mtime threshold also detects a
silent stall. Scheduling suspension or a failed sweep cannot provide a wall-clock
availability guarantee; status does not conceal either condition.

For each allowed identity the sweep resolves its own current named connection,
or its legacy config base, then opens a read-only mailbox snapshot. It reports
only identity selector, pending/shown counts and `unreadable`; it never persists
mail content, event IDs, credentials, endpoints or errors. It does not sync,
write State/leases/outbox/mailboxes, mark shown, create receipts, write owner
metadata, wake a session, or escalate. The controller reads findings and owns
any recovery delivery. The sweep report's `daemon_down` finding is detection,
not a rescue.

## Rescue counts and soak artifacts

After actual fallback delivery, the controller records the appropriate count
in `<home>/<identity>.rescue.jsonl` through the service-account-local command:

```
a2a daemon rescue --home HOME --owner-pin POLICY --owner-pin-sha256 HASH --image-sha256 IMAGE_HASH --service-gid GID --identity ID --layer sweep --count N
```

Layers are `sweep`, `watchdog`, and `manual`. `traffic` records the controller's
counts-only observation of real primary traffic for qualification. The file
contains only timestamp, identity, layer and count, at 0600 with no-follow open
and a nonblocking append lock. Zero/negative counts, unknown layers and foreign
identities refuse. No receipt or injection is made by this command. It records
actual recovered deliveries only; notices, detections and empty inbox reads do
not count. A manual inbox by itself cannot prove the primary missed anything;
the controller must associate it with a recovery before recording it.

These audit commands require the service UID; station callers cannot invoke
them through the execution allowlist. They do not gain root or expose a new
service endpoint. Host/controller integration must arrange execution in that
existing service context. C6 does not impersonate the account from an owner
session, type into tmux, or make daemon-private artifacts agent-writable.

Generate an artifact using the root plan's owner-selected N (example: 3):

```
a2a daemon soak --home HOME --owner-pin POLICY --owner-pin-sha256 HASH --image-sha256 IMAGE_HASH --service-gid GID --plan ROOT_PLAN --from RFC3339
```

It writes `<home>/soak-report.json` with interval, N, per-identity traffic/rescue
counts and a qualified boolean. Every allowed identity needs a complete readable
log, observed traffic, zero rescues, and the full elapsed interval. Unknown,
torn, oversized or inconsistent logs refuse. Qualification is conditional on
complete and truthful controller accounting; elapsed uptime alone is not proof.
The artifact never disables fallback hooks. Real-traffic soak, installed Muse
hook/websocket fixtures and native boot/wake evidence remain qualification gates
owned by Source/Meta and the host keepers.

## One owner, receipts and native consumers

A new service process starts with an empty in-memory owner epoch. Only the
successful committed CM SessionStart hook can publish fresh ownership in it;
a fail-open hook return is insufficient. Disk commit and epoch publication are
serialized per identity with cancellation. Watch/wait requests require that
fresh exact owner. A newer committed Start replaces it; old End cannot disarm
the replacement, and restart cannot renew using only persistent evidence.
CM retains atomic owner-only lease renewal, generation disarm and no takeover.

C6 preserves the station stdout-write-and-flush plus invocation/batch/identity
ACK receipt barrier. Watch, sweep, stop and heartbeat make no receipt. Existing
C5/CM tests retain the notice/empty-inbox, refused/stale idle-wake, stop, timeout,
Claude and Codex regression witnesses. The service adds no monitor websocket
shim or hook surface. Native installed Muse SessionStart/prompt/stop stdin and
stdout shapes, and websocket consumer fixtures, must be captured by qualification
owners; synthetic fixtures are explicitly not an installed Muse version pin.

## Exit codes and remaining native checks

CLI success is 0, operational/policy/unresolved refusal is 1, and usage is 2.
Sweep findings, including `daemon_down` and unreadable mailboxes, are data in a
successful report, not receipt/injection attempts. Artifact-write failure is 1.
A non-qualified valid soak artifact still exits 0 and carries `qualified:false`.

This bench permits gofmt, build, vet and Linux vet only. CI/Mac precheck must
execute the tests, including controls. A real removed-run-dir/reboot-equivalent
install followed by station authentication, supervisor stop/restart, killed
process detection and service-account write denial remain required host checks.
No native sign-off is inferred from static checks.
