# Darwin arm64 archive

This archive contains `a2a` and its real Swift `a2a-keychain-status` companion as
siblings, plus LICENSE and NOTICE. Keep both executables in the same directory,
including when invoking a2a through a symlink. No daemon, tap or package installer
is included. A release does not authorize installing or switching a live consumer.

Before extraction, verify the signed checksum file with cosign. Replace TAG with
the exact stable release tag, including its `v` prefix:

```sh
cosign verify-blob --signature checksums.txt.sig --certificate checksums.txt.pem \
  --certificate-identity 'https://github.com/relux-works/a2a-swarm/.github/workflows/darwin-release.yml@refs/tags/TAG' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
shasum -a 256 -c checksums.txt
```

Inspect archive members before extracting into an empty private directory. The
payload has exactly `a2a`, `a2a-keychain-status`, LICENSE, NOTICE and INSTALL.md.
`release-evidence.json` and `evidence.tar.gz` bind the source commit/tag object,
version/toolchain, full extracted and compiled requirements, signatures and accepted
notary submission. The exact SHA256 of each signed executable is recorded there.
Never replace only one sibling with an unverified build.

Both executable signatures must verify with strict validation and the approved
external requirement, not merely a TeamIdentifier substring. From a reviewed
source checkout on Darwin arm64 with cgo, the repository verifier also requires the
entire compiled designated requirement to equal policy:

```sh
go run ./cmd/a2a-release verify /absolute/extracted/payload /absolute/new/evidence
```

No downloaded CLI or companion needs to run during signature verification. Bare
executables/ZIPs cannot carry a stapled ticket; the service registers their code
hashes for online notarization checks. Offline Gatekeeper/no-UI/item-access behavior
is not certified by this archive or a checksum match.

Installation is a separate owner-admitted operation into an explicit destination.
Before adopting a successor containing the Keychain migration change, the operator
must inspect the host's actual configured roots for pending Keychain Daemon
migration journals and finish/rollback any pending transaction with the CURRENT
binary before upgrading. Unknown roots are not clearance. This archive performs
no inventory, journal cleanup, credential migration, ACL conversion or adoption.

Real Keychain/no-UI, legacy-item consent/refusal, rollback and N-to-N+1 item access
require their own owner-controlled native fixture. A second distinct admitted image
with the same full requirements is needed for N-to-N+1 evidence; signing/notarization
alone is not that evidence. See the source `docs/darwin-release.md` for custody and
qualification holds.
