# Keychain credentials for a2a: findings and a staged proposal

Author: ivan-tb-keeper, 2026-09-29. Status: proposal, for #a2a-swarm governance.

Source: an earlier research run (Codex, gpt-6-astra, 2026-09-22) on the Keychain backend of a Go CLI in another project. It was measured on synthetic keychains and synthetic credentials only. This note keeps the findings that transfer to `a2a`; the project itself is out of scope.

## 1. Three guarantees that are easy to conflate

| Guarantee | What provides it | Does a Keychain adapter alone give it? |
| --- | --- | --- |
| G1. The secret never reaches the agent's context, environment, argv, stdout or logs | CLI discipline: private memory and pipes, redaction | yes |
| G2. The Keychain trusts one specific program, not "anything the user runs" | Native Security API plus a stable code identity of that program | only with native access and a stable signature |
| G3. Arbitrary agent code cannot extract or use the credential | A separate OS principal (or host), or a broker that owns the credential and exposes operations | no |

`a2a` already delivers G1. The MVP (reads through `/usr/bin/security`) does not deliver G2, and no Keychain design delivers G3.

## 2. What was measured

1. **Delegating to `/usr/bin/security` authorizes the helper, not the caller.** The Keychain sees `security` as the accessing application. Once an item trusts it, any process of the same user can read the item through the same helper, with no prompt. The helper has a stable Apple signature, so rebuilds of the caller cause no prompts. That is its advantage, and also the reason it is not G2.
2. **Native access binds the item to the calling executable.** The native backend used SecItem on the file-based keychain, with an ACL of `SecAccessCreate(label, [SecTrustedApplicationCreateFromPath(NULL)])`, meaning "this process". The item was approved once. After that, new processes of the same executable read it with no dialog while the keychain stayed unlocked. Tested operations: write, re-read from new processes, update, lock and unlock.
3. **Unlocked and authorized are independent.** An unlocked keychain does not authorize every application. An authorized application still cannot read a locked keychain. "Once after reboot" means both conditions hold: someone unlocks the keychain, and the executable's authorization survives.
4. **The ACL-reset trap.** Re-writing an item and setting its access object again raised a permission dialog. On some macOS versions it does so even with user interaction disabled. The measured fix:
   - Mark items created with the native ACL (for example a versioned `kSecAttrComment`).
   - On ordinary writes (a token refresh, a re-login), `SecItemUpdate` only `kSecValueData` and never touch the ACL.
   - Migrate legacy items, created by `security` with a broad ACL, only inside an explicit interactive command.
5. **Non-interactive by default.**
   - Ordinary commands disable Keychain UI (`SecKeychainSetUserInteractionAllowed(false)`) and fail closed with `errSecInteractionNotAllowed`, so a headless agent never hangs on a dialog.
   - The setting is process-wide: serialize Keychain calls and restore the previous value.
   - Only explicit commands such as `login`, `unlock` or `migrate` run interactively, with a human at a TTY.
6. **Identity across updates is a signing question.**
   - A stable designated requirement (identifier plus Team ID, from a Developer ID signature) keeps the authorization across releases.
   - An ad-hoc or changed code identity may need approval again.
   - Git commit signatures do not code-sign executables.
7. **The data-protection keychain is not an option here.** It needs access-group entitlements authorized by a provisioning profile inside an app-like bundle, and it exists only in a user login context. It is unavailable to `launchd` daemons and SSH sessions. For a standalone CLI and headless hosts, the file-based keychain (login, or a dedicated keychain) is the practical choice (Apple TN3137).
8. **Native access does not strictly need cgo.**
   - A probe built with `purego` v0.11.0 under `CGO_ENABLED=0`, internal linking and `-libgcc=none` loaded Security.framework and called a public function.
   - Closed build policies may still reject it. Curator's manager profile rejects `HFiles` and `go:cgo_import_dynamic` outside `x/sys`, so purego there needs an explicit, versioned admission change.
   - cgo is the conventional route when the builder is a trusted release pipeline.
9. **Other platforms, if ever needed.**
   - Windows Credential Manager passed read, write and delete in a desktop session. Over SSH it returned `ERROR_NO_SUCH_LOGON_SESSION`, because the credential set is bound to the logon session.
   - Linux Secret Service needs the user's D-Bus session, and may need an unlock prompt.
   - The general rule on every platform: a CLI cannot hold no bootstrap secret and still unlock a protected store by itself. Someone unlocks once per boot, or a broker running in a proper session owns the secret.

## 3. Proposal

### Stage 0: the MVP (as accepted by a2a-swarm-ac)
Per-host `A2A_SECRET_STORE=keychain|file`, with no default. Reads go through `/usr/bin/security`, the secret travels on stdout into a pipe and never in argv. Writes use `security -i` with the command on stdin, and the ACL is `-T /usr/bin/security`. Missing or locked means fail closed.

Two additions:
- **Never pass `-A`** (allow all applications), and test for it.
- **State the limit** in the protocol: Stage 0 gives G1 only. Any same-user process can read every a2a item through `security`.

### Stage 1: the proper design (operator direction, Ivan, 2026-09-29)
1. **Build.** `a2a` is a released artifact, signed with the Developer ID of our Apple Developer account and notarized.
   - A release pipeline builds and signs it; the certificate lives in a dedicated signing keychain on that runner.
   - Agents install the release. They never `go build` the binary they authenticate with.
   - The Keychain code uses cgo in that pipeline. purego only if `a2a` must also pass a closed builder such as Curator's.
2. **Access.** Native SecItem on the file-based keychain: the login keychain on workstations, the dedicated `a2a.keychain-db` on the headless Mac mini. The item ACL trusts the signed `a2a` only, through its designated requirement. `/usr/bin/security` is not in any ACL.
3. **Interaction.**
   - `wait`, `post`, `read`, `card` and the projector run with Keychain UI disabled and fail closed with a typed error.
   - `a2a creds migrate`, `a2a keychain unlock` and `a2a login` are the only interactive commands.
4. **Updates.** A token refresh or re-login updates data only, and a comment marker identifies native-ACL items. The migration from Stage 0 items (security-trusted) happens once, inside `creds migrate`, interactively.
5. **Headless.** The Mac mini keeps the dedicated keychain, unlocked by the operator once per reboot, with the projector's blocker notice until then. This is already in the accepted design.
6. **Exit condition, with negative tests:**
   - no a2a item lists `/usr/bin/security`;
   - `security find-generic-password -w` from another process is refused or prompts, and never returns the secret silently;
   - an updated signed release reads with no prompt;
   - no command except the three interactive ones can raise a dialog.

Stage 1 gives G1 and G2. It still does not give G3: any same-user process can run `a2a --as <name>` and act as that agent, even though it cannot extract the token.

### Stage 2: the Apiary target, where isolation actually comes from
The agent session host (agent-session-host, and waggle §4.2's "the session host holds the session key and signs for the agent") becomes the credential-owning broker:
- The Matrix token and the waggle keys stay in the host.
- Agents send typed operations over its local socket: post, wait, read, sign. The host's interface is operations with limits (rooms, kinds, rates). Never a `getSecret`.
- Agents run under a separate OS account (curator-trust v0.4 §12). The host runs under the operator's account.
- `a2a` becomes a thin client of the host.

This is the only stage that gives G3.

## 4. Decisions and human-only steps
- **Ivan (human-only, rule 14):** the Developer ID certificate and a signing keychain on a release runner. Also the switch of his workstation from `file` to `keychain`; his standing rule is that agents do not open the Keychain until he says so.
- **a2a-swarm-ac:** cgo in the release pipeline or purego, and the release pipeline itself (tag → build → sign → notarize → publish).
- **Joint:** is `a2a` distributed as a standalone signed release declared as a system requirement, or through Curator? Curator today has no post-build signer profile. Its future signer profile would have to bind receipts to the final signed bytes, and must never re-sign a cached artifact in place.

## References
- Apple TN3137, On Mac keychain APIs and implementations: https://developer.apple.com/documentation/technotes/tn3137-on-mac-keychains
- Apple, Code Signing Guide (code identity, designated requirements): https://developer.apple.com/library/archive/documentation/Security/Conceptual/CodeSigningGuide/AboutCS/AboutCS.html
- Microsoft, CredReadW (credential sets bound to logon sessions): https://learn.microsoft.com/en-us/windows/win32/api/wincred/nf-wincred-credreadw
- freedesktop Secret Service, unlocking: https://specifications.freedesktop.org/secret-service/latest/unlocking.html
- purego v0.11.0: https://github.com/ebitengine/purego/releases/tag/v0.11.0
- Curator spec, manager profile §2.2–2.3 (closed Go toolchain) and decision 0005 (signing boundary): https://github.com/relux-works/curator-spec
