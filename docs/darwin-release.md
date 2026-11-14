# Darwin release source contract

This is a source implementation for Darwin arm64 `a2a` and the real Swift
`a2a-keychain-status`, compiled from one exact reviewed main commit. It does not
record an existing Developer ID image, native success, credential provisioning,
tag, published release or installed adoption. `release/VERSION` proposes **0.1.0**
for the first release; changing that file never creates a tag. The generic portable
build descriptor, installed runtime, item backend, daemon and ACL contracts remain
unchanged. There is no optional unsigned tagged-release path or manual bypass.

## Entry points and dependencies

| Entry point | Purpose | Output |
| --- | --- | --- |
| `.scripts/release-config.sh` | Require all five nonempty fields without displaying values | Closed presence verdict; no native action |
| `go run ./cmd/a2a-release admit v0.1.0` | Read signed Git objects and legitimate current GitHub policy/check/review records | Admission JSON on stdout |
| Compiled `a2a-release release v0.1.0` | Owner-admitted tagged pipeline only; requires `RELEASE_ADMITTED_TAG_OBJECT` from admission | One archive, evidence, checksums and cosign signature/certificate in `$RUNNER_TEMP/a2a-release-output`; then qualified release assets |
| `go run ./cmd/a2a-release verify PAYLOAD_DIR NEW_EVIDENCE_DIR` | Verify both final signatures/external predicates and exact full compiled DR | Complete extracted requirement text/binary and identities JSON; no CLI/item execution |
| `go run ./cmd/a2a-release compare OLD_PAYLOAD NEW_PAYLOAD NEW_EVIDENCE_DIR` | Independently verify two pairs of actual images, then compare complete requirements and distinct hashes | Raw requirements for each image and comparison JSON; not item-access qualification |
| `go test -race -count=1 -timeout=3m ./internal/darwinrelease` | Narrow nonsecret controls; native cases compile only on Darwin arm64/cgo | Actual test verdicts, not a release |
| `.scripts/release-cleanup.sh` | Always-step cleanup of only this runner job's ephemeral signing directory/keychain | Nonzero if keychain deletion fails |
| `.scripts/signing-keychain-fixture.sh DIR` then `A2A_SIGNING_KEYCHAIN_CONTROL_DIR=DIR go test -run '^TestSigningKeychainAccessControl$' ./internal/darwinrelease` | CI job `signing-keychain-control`: a throwaway self-signed identity with no trust change, imported through the release import path and signed with the release codesign argv and a fixture requirement, then strictly verified. Arms vary only the fixture keychain's presence in the user search list (restored after each arm) and a widened key partition list; an informational timestamped pair runs too. `--cleanup DIR` removes the files | `SIGNING-CONTROL` lines with an identity class, exit status and a bounded error class (never raw output) and one VERDICT for H6 (search list) and H1 (partition); INCONCLUSIVE fails; says nothing about the Developer ID chain or the timestamp service |

Go 1.25.12, native `CGO_ENABLED=1`, Security.framework/CoreFoundation, Xcode16.4,
Swift6.1.2 and cosign2.6.1 are explicit release pins. GitHub-hosted `macos-15`
provides the arm64 runner; the workflow explicitly selects Xcode16.4. The controller
checks versions and records the actual OS/SDK/clang/notary toolchain. Hosted images
are mutable: version mismatch fails closed rather than silently selecting another
compiler. Action revisions are immutable and checkout never persists credentials.
The controller is compiled in a nonsecret step before credential exposure; no
DISTRIBUTED executable is built before the all-five presence preflight succeeds.
No GoReleaser setting is assumed to fix identifiers or Swift packaging; the explicit
stage owns the final signatures and never strips/resigns them later.

## Admission and owner authority

Direct owner approval of the proposed custody, two identifiers/full requirements,
bot release tags and temporary-OS-user qualification has been recorded. The broader
approved delivery also includes GitHub Releases, Homebrew on macOS/Linux and deb/rpm.
Approval is not evidence of provisioning, signed/notarized images, a created tap,
publication or native success. Source implementation alone performs none of these.
Before the first tag, the owner must provision the approved repository material.
A deployment environment is not a first-release prerequisite; any future protected
environment is separately owned optional hardening.
The five repository-level names are MACOS_SIGN_P12,
MACOS_SIGN_P12_PASSWORD, ASC_ISSUER_ID, ASC_KEY_ID and ASC_KEY_P8. Never request values
through chat or copy them into a working clone. No secret endpoint inventory is
part of this workflow. The Actions secrets context proves field presence, not
repository-versus-organization provenance; the owner must verify the approved
repository provisioning before the first tag. Zero, partial, empty or
whitespace-only configuration refuses.
No approval toggle, workflow_dispatch or unsigned snapshot can bypass tagged gates.

`.github/release-signers` contains only the approved bot public Ed25519 key under
`release-maintainer`, pinned to SHA256:63rp3DJz29F8BZCMoD8HbG+fgPaIat2wBRse0vRp34E.
It alone authorizes release tags. Separate `.github/source-signers` entries use the
exact generic `source-maintainer` principal and the three existing approved public
source keys. Their fingerprints are pinned in both policy and the controller;
unknown, unsigned, extra-key, wildcard-principal or ambient-trust source refuses.
A source-only maintainer cannot sign a release tag. Admission refuses graft or
shallow metadata at Git-resolved and common paths, including linked worktrees;
even an empty metadata file refuses. This prerequisite does not protect against
arbitrary concurrent filesystem mutation. All controlled admission Git reads pin
`core.commitGraph=false`, ignoring ordinary, split, common and alternate ancestry
caches while preserving valid signed cache-bearing admission. Inherited Git
overrides, including the test override, remain filtered. Ordinary-cache fixture
acceptance is separate from the effective reader-policy assertion; no forged-cache
bypass reproduction is claimed. No private key or installed
trust file is read or changed by this policy.

Stable annotated signed `vX.Y.Z` must match VERSION and the checked-out commit,
which must equal current remote main. The controller forces SSH verification
against each distinct reviewed allowlist. `release/policy.json` retains the exact
accepted history base; EVERY introduced object in that base..candidate range must
cryptographically verify and report G with an approved SOURCE fingerprint,
including previously accepted maintainer commits. The historical anchor is not an
introduced object; its trust status is not relabelled or rewritten. Changing the
anchor or allowlists is an explicit reviewed policy change, never automatic
history forgiveness. No force, retag or source rewrite.

The GitHub client requires a merged PR whose head is the exact commit and a current
accepted review from the recorded public bot. A real APPROVED review is accepted.
A COMMENTED review can carry the exact standalone transport marker
`Release-Review: ACCEPT FULL_COMMIT_SHA` after independently accepted review; this
supports the platform's author-approval restriction without inventing another
identity or an APPROVED state. The latest review from that reviewer on that head
controls admission. The producer never emits that marker as a self-review verdict.

Required checks are the union of the three reviewed policy names (`test-linux`,
`release-controls`, `release-native-controls`) and current ref protection/ruleset requirements. The legacy rule is read through
the documented GraphQL ref field, with active rules read through the metadata REST
endpoint; administration-only REST protection APIs are not used.
The public GitHub Actions app ID is used unless policy explicitly binds another
positive integration ID. Actions job/run provenance is authenticated before event
eligibility and uniqueness: exact check/job/run IDs, name, SHA, repository and head
repository, check-run URL association and source workflow path must match. Proven
pull-request runs are ineligible; their success, failure, pending or skipped jobs
cannot satisfy or shadow a required main check. Unknown or unreadable provenance
refuses. Exactly one eligible main-push record per required name/app must remain,
with completed successful check, job and run. Two eligible main records remain
ambiguous, and any non-success main record blocks even beside a green one. No
first/newest/greenest selection is used. Other integrations retain exact SHA/app
uniqueness and completed-success requirements. A fabricated check API write with
the Actions app ID is insufficient.
The jobs/runs association uses only the additional actions:read permission. Missing,
skipped, pending, failed or unknown eligible main checks refuse, as do malformed,
ambiguous or incomplete reads. Unknown rule types and unimplemented
required-workflow/scanning rule semantics refuse rather than disappear from policy. Legacy status-only contexts cannot establish
the required check-run provenance and remain blocking. Unreadable protection/rules
APIs refuse; this token's access must be verified independently before a release,
not repaired by silently dropping a policy query or using another credential.

Admission is repeated after checkout, before publication and after private draft
uploads. Its tag-object pin prevents a moved-ref substitution between jobs. GitHub
has no atomic transaction combining tag/check reads and publication: owner controls
must keep admitted tags immutable. A failed/unknown API read is never green.

## Full requirements and the native stage

For each executable the exact requirement is in `release/requirements/NAME.txt`:
Apple generic anchor, exact `works.relux.a2a` or
`works.relux.a2a-keychain-status` identifier, issuer extension
1.2.840.113635.100.6.2.6, Developer ID Application leaf extension
1.2.840.113635.100.6.1.13 and leaf OU262RZ595FP, all ANDed. No App Store alternative,
cdhash pin, filesystem path, ad-hoc mode or TeamIdentifier-only oracle is admitted.

The runner-local keychain is created/imported through Security APIs, with a random
password held in memory. Only codesign is admitted to its signing-key access list;
this is release-key setup, not a change to user item ACLs. No P12/password/private
key bytes are passed in argv. The native import/ACL mechanics require later admitted
real execution; source does not claim they have worked already. Both final Mach-O
files must be arm64 executables. Codesign uses explicit identifiers, full designated
requirements, hardened runtime and timestamp. Separate strict codesign verification
and Security.framework external-requirement evaluation must succeed. The native gate
also rejects ad-hoc flags, missing runtime/timestamp and any difference between the
COMPLETE extracted compiled requirement and independently compiled approved predicate.
Full requirement text/binary and raw nonsecret codesign output are retained.

MACOS_SIGN_P12 is the base64 PKCS#12 container; its password is a nonempty
string. ASC_KEY_P8 is the literal private PEM, never a local operator path.
The P8 is a0600 private runner file. Apple TN3147's supported `notarytool` interface
receives its PATH plus the public issuer/key IDs; real IDs and argv are never logged
or retained. Native process failures report closed stage errors without stderr or
argv. All five fields and Git tokens are removed from compiler/native child
environments. The controller passes OIDC fields only to keyless cosign signing. Temporary private content
is absent from caches and public assets. The output guard checks literal supplied
values and decoded P12 bytes before archiving; its bounded control is not a claim
of secure erasure of every process-memory copy.

Both signed siblings are submitted in one private ZIP. Successful wait, matching
accepted submission ID/ZIP SHA256 and both arm64 code-directory-hash tickets for
the exact signed binaries are required. Submission ID and log jobId must both be
valid UUIDs and match by identity regardless of alphabetic case; raw fields and
evidence remain unchanged. Malformed or genuinely different IDs refuse;
API/JSON/notary failure aborts. The native signatures/full requirements and exact
signed hashes are rechecked after notarization. The final archive is constructed
ONLY then, with exactly both siblings, LICENSE, NOTICE and INSTALL.md. Archive
readback compares exact executable bytes and validates the complete gzip stream
through its checksum/trailer EOF. Extra uncompressed data, trailing bytes and
additional gzip members refuse. Bare executables/ZIPs are not stapled;
Gatekeeper uses registered online tickets. Offline/no-UI behavior is a separate gate.

Driver cleanup and the workflow's always step remove the private runner keychain,
P8 and temporary directory, including error paths. Keychain deletion failure blocks
progress. Hard runner termination is additionally bounded by the hosted VM lifecycle;
it is not a guarantee that an in-process defer runs after SIGKILL.

Final archive/evidence hashes go into checksums.txt. Cosign's keyless signature and
certificate bind that exact checksum file to this workflow/tag and GitHub OIDC
issuer. Local byte checks and signature verification precede any draft. Uploads
must return matching SHA256/name/size; tag/check admission, checksums and cosign are
revalidated before the only public transition. Failure before the final transition can leave a private
draft for owner inspection; there is no automatic release/draft deletion. An
ambiguous publication response is UNKNOWN and must be inspected without retry,
retag or deletion of a possibly public release. All its gates had to pass before
that transition was attempted. No generic claim of remote transactional rollback.

## Controls and rollout holds

Controls use actual isolated signed Git objects and intended tag/signer/version
violations, with distinct accepted source and bot-tag fixture keys plus unknown/
unsigned source and source-only-tag refusals; actual HTTP client fixtures for
API/check/review errors; all32 presence masks and error/environment/private-output plants; exact companion/package/byte
controls; and the real publication-order helper. macOS controls compile the full
requirement and changed terms using Security.framework, and reject self-created
ad-hoc/wrong-architecture Mach-O fixtures. No real Developer ID, notary, login
keychain, live user, operator credentials or full unrelated suite is needed in PR
controls. Parser/compiled-requirement equality alone is not native identity success.
A real Developer ID positive is deliberately unavailable to these nonsecret controls
and requires separately admitted release execution. Source-only authoring results
must stay distinct from actual CI and native results.

N-to-N+1 requires a second distinct admitted image pair. `compare` verifies actual
images and full compiled requirements and records raw outputs. Run it in the approved
native fixture before adoption; it is not replaced by equality of TeamIdentifier or
by the struct-fixture comparison test. This first source slice does not claim a
previous release image exists or automate cross-version Keychain qualification.
Temporary OS user creation/login/consent/deletion, real HOME/login target, existing
item read/update, missing/locked/interaction/deadline/fence controls, no unexpected
UI, legacy ACL consent/refusal, rotation/rollback and signing-key custody remain
owner-controlled gates. Existing pending Keychain Daemon migration journals must
be resolved using the old binary before any successor adoption. No install is
performed by the release job. The approved full delivery additionally requires Linux release/package/tap
integration with the shared release standard and approved `relux-works/homebrew-tap`.
That concrete shared interface/source pin is still pending for this Darwin-specific
source slice. Do not create a duplicate template or guess a ref; do not call this
PR completion of the broader distribution. Shared integration is an explicit
follow-through before declaring full delivery. Apple notarization concerns supported
Darwin artifacts; ELF packages are not claimed to be Apple-notarized. This worker
creates no tap/repository or package publication.

Primary references: [Apple requirement language](https://developer.apple.com/library/archive/documentation/Security/Conceptual/CodeSigningGuide/RequirementLang/RequirementLang.html),
[Apple TN3127](https://developer.apple.com/documentation/technotes/tn3127-inside-code-signing-requirements),
[Apple TN3147](https://developer.apple.com/documentation/technotes/tn3147-migrating-to-the-latest-notarization-tool),
[notarization workflow](https://developer.apple.com/documentation/security/customizing-the-notarization-workflow),
[Security signing information](https://developer.apple.com/documentation/security/signing-information-dictionary-keys),
[GitHub hosted runner labels](https://docs.github.com/en/actions/reference/runners/github-hosted-runners),
[macOS15 arm64 image/toolchain inventory](https://github.com/actions/runner-images/blob/main/images/macos/macos-15-arm64-Readme.md).
The pinned launcher precedent is [13fe574c](https://github.com/relux-works/curator-agent-launcher/tree/13fe574c612bc6396fa4a596257f3cb85ba25932);
its optional unsigned mode is not inherited, and its signing settings are not a
fixed-identifier/full-DR or Swift-companion gate.
