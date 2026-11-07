# Stored own terminal association

`a2a bind association --private-intent-fd=3 --json` compares one own stored
binding with an expectation supplied by an admitted cooperating launcher.
This candidate requires its own independent source review and receiving
consumer acceptance. A match establishes stored equality only: it supplies no
native observation, attachment authority, Host custody, readiness, process
liveness, thread currentness, absence, family exit or permission to launch.

The selected local identity name is `source_identity`. `source_connection` is
`legacy` in the legacy config namespace, or the selected public
`connection_id` in named mode. They are equality assertions, never selectors.
Global identity/profile guards still apply. Named resolution reads only bounded
public `connections.json`, derives the selected own paths, and fences the
operational binding fingerprint across the snapshot. It does not read
`matrix.env`, identity metadata, credentials, Keychain or other secret stores.
An unavailable effect-free context is a closed refusal. Global parsing or
noninteractive human-selection failures for this command also produce a closed
`input_invalid` result, without stderr or context resolution.

The launcher checks the expected tuple and the SHA-256 of the **complete
original launch intent bytes**, including every option, against its admitted
private policy before handing off the descriptor. The source cannot verify
that policy correspondence or descriptor origin. An identity name, digest,
file mode, seals or descriptor possession is not authentication. Same-UID
hostile launchers and writers bypassing the cooperative lock are outside this
boundary. The consumer must admit that launcher/handoff independently.

The request is one four-byte unsigned big-endian length followed by exactly one
UTF-8 JSON object, without a suffix. Length is 1..8192, file size at most 8196.
Only inherited descriptor 3 is admitted: read-only, regular, effective-UID
owned, exactly 0600 and unlinked. Linux additionally requires all four
WRITE/GROW/SHRINK/SEAL seals. The implementation checks metadata before and
after positional reading from zero and closes the descriptor on all parsed
operation paths. Pipes, sockets, named files and writable descriptors refuse.
On macOS, closing every writer is the admitted launcher's cooperative duty;
this is weaker custody, without Linux's kernel immutability guarantee.

The [private schema](../internal/association/testdata/private-input.schema.json)
is closed at every request level. All fields are required, with no nulls,
duplicate or case-alias keys, BOM, invalid UTF-8, unknown keys or trailing JSON.
Depth is at most 5 and token count at most 512. IDs and times use unsigned
canonical decimal integer tokens, with no fraction/exponent or rounding. PIDs
are 1..2147483647; time values are 0..4102444800000 Unix milliseconds, with
0 < expiry - issue <= 30000. Require issue <= now < expiry at admission and
again before publication. Rollback or unavailable time refuses. The request
may be compared again while valid, without adding a replay registry or effects.

Only the selected own state is read, once, under its **existing**
`state/NAME.lock` flock. The lock is opened read-only without O_CREATE and
attempted exclusively/nonblocking for at most 100 ms. No directory, lock or
state file is created. The own namespace and state directory must already be
0700; state and lock must be own regular 0600 files, with no symlink components.
The copy/decode limit is 100 ms while locked, state size 1048576 bytes, depth
32 and 65536 tokens. Duplicate/case-alias keys and partial JSON refuse. Unknown
and unrelated source state fields, including the newer delivery fields, remain
readable and are never treated as association authority. HostEnrollment is not
interpreted or used as custody. No Runtime record or second read fills a gap.

The whole operation checks a 250 ms monotonic budget before publishing one
complete buffer. These are cooperative regular-file limits; kernel scheduling
and output delivery are not hard real-time promises. A long-held writer lock
normally yields `snapshot_unavailable`, no evidence. Retry later requires a
new intent. No runtime control lock or lifetime consumer lock is taken.

A usable binding requires the complete stored generation, SessionID,
consumer PID/exact birth, owner PID/exact birth, stopped flag and Target.
All eight Target fields are compared: socket, socket_id, pane, pane_pid,
pane_birth, owner_pid, owner_birth and runtime. Target owner/runtime fields
must be internally consistent. The lease holder must equal the exact SessionID
and expire strictly after observation and publication. A `pid:` holder remains
stale even if other lease operations would accept it. Stored binding Host must
exactly equal the binding writer's `machineLabel()`: the sanitized OS hostname,
with its existing `unknown` fallback. Case, punctuation and printable Unicode
are retained. The separate identity naming slug is not read or compared, and
changing that slug cannot change the machine label. Host equality is exact;
the stored Host is never normalized, inferred or repaired. Context revalidation
also fences a changed local machine label before publication.
None of these declarations establishes current process identity across hosts.

The [public schema](../internal/association/testdata/public-output.schema.json)
is closed. Output is one JSON object plus LF, at most 2048 bytes, without stderr
or raw errors. `match/stored_tuple_equal` exits 0; generation or tuple mismatch
exits 1; all unavailable refusals exit 2. Output-write failure exits 3, without
retry or diagnostics; complete delivery cannot then be guaranteed.

Every result retains `stored-own-binding`, `selected-own-state-snapshot` and
`not-observed-by-this-interface`. The projection contains only generation,
consumer/owner PID, SHA-256 of their exact **stored** birth declarations,
runtime, stopped and fixed stopped reason. Birth hashes are low-entropy public
correlations, not confidentiality or native identity. A stopped record always
says `recorded-stop-cause-ambiguous`, regardless of its free-form outcome.
Consumer DaemonPID is never relabeled as native owner.

Variable public scalars are checked against private strings and numeric pane
PID declarations from the request and own snapshot, using canonical integer
text. Public consumer/owner PID roles remain explicit. Collision with raw
SessionID, births, socket, socket ID, pane or pane birth conservatively yields
`unavailable/privacy_collision`, with stored data, correlation **and observation
time null**. This reconciles the historical schema's timestamp conflict:
clock_unavailable and privacy_collision require null observation time; other
reasons require an integer. No unadmitted digest becomes trusted correlation.
No foreign state is enumerated or read for collision checking.

Refusal precedence is clock, descriptor trust, request validity, own context,
future/expired input, whole budget, unavailable snapshot, missing/invalid
binding, stale lease/Host, stopped binding, generation mismatch, tuple mismatch,
then equality. The final budget check applies only after request, context and
time admission, so it cannot replace an earlier descriptor, validity or context
refusal. Final clock, expiry, lease and budget checks suppress a stale
projection. Privacy suppression applies to every publishable variable scalar,
including refusal timestamps, without revealing which value collided.

No path changes binding, generation, leases, hooks, notifications, mailbox,
receipts, UNKNOWN attempts, reservations, provider or profile state. There is
no connection preflight or owner-DM warning, native probe, registry runtime
read, bind/unbind, wake, hook, network call or launch. Existing `bind status`
and `runtime status` behavior is unchanged.

| Source clause | Implementation and controlled evidence |
| --- | --- |
| Launcher/FD origin | Metadata/seal checks only; admitted-launcher trust and weaker macOS custody remain explicit. Common Unix fixtures exercise shape/refusals; Linux uses sealed memfds and non-Linux Unix uses cooperating unlinked read-only files. Neither qualifies installed launchers. |
| Effect-free context/no-create lock | Early CLI dispatch; public routing only; no credential/session loader; existing read-only lock. Synthetic poison-FIFO subprocess controls detect ordinary preflight or credential reads. |
| Session lease and Host | Exact SessionID holder, strict expiry and sanitized OS hostname; hostname differs from slug in positive controls, while a foreign Host matching the slug refuses. Case/whitespace differences refuse; absent/poison slug cannot affect the result. HostEnrollment is uninterpreted. |
| Lock and deadlines | Existing flock, 100 ms acquisition, 100 ms copy/decode, 250 ms operation; synthetic contention and publication guards. No runtime lock. |
| Birth/privacy | Stored declaration hashes, lossless integer parsing, pane-PID and observation-time collision controls; closed timestamp suppression. |
| Command/schema/current state | Separate early operation including global-error output; all eight Target fields, unrelated/new top-level fields tolerated. Tests load both schemas and check fixtures/output plus planted invalid-schema controls, independently of implementation struct decoding. |

Checks use controlled synthetic state and inherited synthetic descriptors only.
They establish comparator behavior, refusal sensitivity and no state writes;
they do not qualify installed launchers, real identity state, native process
birth/thread custody, provider/Keychain operations or macOS behavior. An
independent exact-candidate Muse review, new marker, checks and source-owned
landing remain separate from this local candidate. The Router consumer bridge
and its integrated acceptance are separate work.
