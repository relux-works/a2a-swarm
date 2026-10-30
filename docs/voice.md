# Optional local voice replies

Owner decision, 2026-10-23 (confirmed 2026-10-28): Piper ru_RU
irina-medium is the single local voice. Pronunciation defects remain.

## Use

The existing text post is authoritative and unchanged. Add `--voice` to one
post for a local spoken copy replying to that text in the same topic. There
is no sticky mode or implicit enablement.

```sh
go build -o bin/a2a ./cmd/a2a
go run ./cmd/a2a-voice-release -out .temp/voice/release
# Read the worker digest from release/MANIFEST.sha256 and the manifest digest
# printed by the recipe. After copying the directory to its installation path:
bin/a2a voice config --enable \
  --worker /absolute/install/release/a2a-voice-worker \
  --worker-sha256 WORKER_SHA256 --manifest-sha256 MANIFEST_SHA256
bin/a2a voice install
bin/a2a post ROOM --topic TOPIC --voice 'Complete original text'
```

Config/install require no Matrix account. `<config>` means `A2A_CONFIG_DIR`,
otherwise `~/.config/a2a-swarm`. Config is `<config>/voice/config.json`, with
`enabled: true`, `max_seconds: 180`, an absolute `worker` path ending in
`a2a-voice-worker`, and lowercase 64-hex `worker_sha256` and `manifest_sha256`.
All three helper fields must be supplied together. Old configs containing only
enablement and the cap remain readable; voice skips until a helper is pinned.
`voice config --enable` without helper flags writes that text-only configuration.
Enabling is explicit, with no engine/voice picker. These commands change no
binding, profile, hook or subscription. Shared-station installation and activation
require a separate maintenance window; this recipe never installs globally.

## Helper release and installation

The shared `a2a` has no Sherpa or CGO dependency, even if accidentally built
with `-tags sherpa`. Native inference exists only in `cmd/a2a-voice-worker`.
A missing or damaged helper cannot prevent the shared CLI from starting.
The release recipe requires darwin/arm64, Go as pinned by `go.mod`, CGO,
Xcode command-line tools (`install_name_tool`, `codesign`) and the pinned
module below. It refuses an existing output directory and publishes only after
auditing and signing every payload file.

```text
release/
  a2a-voice-worker
  lib/
    libonnxruntime.dylib
    libsherpa-onnx-c-api.dylib
  MANIFEST.sha256
```

`MANIFEST.sha256` contains sorted `SHA256  relative/path` lines for every
payload file. It excludes itself; the recipe prints its digest for the separate
config pin. The worker has exactly one `LC_RPATH`, `@executable_path/lib`.
Libraries have no rpaths. `otool -l` and `otool -L` must show only that relative
rpath, the two bundled library references, and macOS system libraries/frameworks.
No module cache, GOPATH or build-machine path is needed at runtime. The system
libraries remain supplied by macOS. The unused C++ Sherpa API is not bundled.

The recipe uses trimpath, no VCS/build ID, deterministic ad-hoc signatures and
no signing timestamp. Repeating it with identical source, toolchain, module
bytes and build environment must produce byte-identical manifests:

```sh
go run ./cmd/a2a-voice-release -out .temp/voice/release-1
go run ./cmd/a2a-voice-release -out .temp/voice/release-2
diff .temp/voice/release-1/MANIFEST.sha256 .temp/voice/release-2/MANIFEST.sha256
otool -l .temp/voice/release-1/a2a-voice-worker
otool -L .temp/voice/release-1/a2a-voice-worker .temp/voice/release-1/lib/*.dylib
```

The installer copies the complete directory, preserving the worker's execute
permission, to a new versioned location. It records the previous config bytes
and previous release directory before setting the three pins above. Verify the
copied manifest against the recipe's printed digest and every payload hash.
Do not mutate an installed directory in place. Rollback restores the previous
config bytes and intact previous directory; the shared CLI does not change.
The model and ffmpeg are separate dependencies, not redistributed in this payload.

Before **every** worker invocation, including the truncation notice, `a2a`
hashes the manifest, worker and both dylibs. Missing/changed files, links,
unlisted entries, malformed manifests and mismatched pins refuse before exec.
These checks detect corruption against locally trusted config pins; they do
not defend against a same-UID attacker rewriting config or changing files
between verification and use. The installer must keep release directories stable.

## Dependencies and pins

The single route currently requires macOS: sherpa-onnx through the native Go
binding `github.com/k2-fsa/sherpa-onnx-go-macos v1.13.8`, CGO, CPU, four threads
and the Piper ru_RU Irina model. Only the helper is built with `-tags sherpa`.
The recipe copies the two exact libraries from that pinned module, removes
module-cache rpaths and ad-hoc signs the result. Other platforms retain normal
text operation. There is no say engine or voice picker.

`voice install` alone downloads:

- URL: `https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/vits-piper-ru_RU-irina-medium.tar.bz2`
- Archive SHA256: `1fc0f54e5e084fe287c07909f2f6e0ba6d857864cf800e3ab80286a4e8233008`
- File-manifest SHA256: `e7c154c19f233349ff4c3578316b43cb144f594af9411aab4814dbfd69a63392`
- Cache: `<config>/voice/models/<archive-sha256>/`, private files/directories.

Install verifies the archive before extraction, rejects unsafe paths/links
and oversized data, checks the pinned manifest and publishes atomically.
Existing installs are verified instead of overwritten. Every render verifies
installed asset hashes; direct helper invocation verifies them as well.
Posting never installs/downloads. The model bundle
retains its upstream model card, which marks dataset license Unknown. This
feature asserts no model licensing clearance and does not redistribute it.

`ffmpeg` must be on PATH with libopus encode and Opus decode support. Output
is mono Ogg/Opus,48kHz,24kbps, voice application. There is no qualified
`afconvert` Opus fallback in v1: unavailable/failed encoding retains text and
warns. `ffprobe` is useful for independent checks, not required to post.

## Text, cap and failure contract

The CLI first uses the existing text-send path and prints its event ID.
Failed text retains its original nonzero exit and starts no voice work.
Without `--voice`, no renderer/encoder/installer/model download runs.

A single clip is capped at **180 seconds**. Input beyond 6000 Unicode runes
is shortened before synthesis to bound native work. If this bound or the
measured audio limit truncates speech, the clip reserves space for the spoken
notice: “Аудио сокращено. Полный текст сохранён в сообщении.” The caption
also identifies truncation; the original text stays complete. Chunking is
deferred. A 50 ms reserve covers Opus padding. Final decoded duration must not
exceed 180 s. Duration and waveform come from decoding encoded bytes to local
signed mono PCM, rather than estimating from text length.

After upload, authenticated fetch-back must match the exact byte count and
SHA256 before publishing any audio event. It uses `m.audio`, MSC3245 voice
and MSC1767 audio/file/text metadata, replies to the text and retains its
`m.thread` root. No second a2a decision/request header or duplicate mentions
are sent. See the [MSC3245 transitional format](https://github.com/matrix-org/matrix-spec-proposals/blob/83f6c5b469c1d78f714e335dcaa25354b255ffa5/proposals/3245-voice-messages.md).

Missing config/helper, changed helper/dylib/manifest, synthesis/encoding/upload/fetch failure, mismatched bytes,
or failed/ambiguous audio-send acknowledgement emit one stderr line marked
`[VOICE_TEXT_ONLY]` and exit 0: the text already succeeded. Helper verification,
model and synthesis failures share the fixed warning:

```text
a2a: warning [VOICE_TEXT_ONLY]: voice synthesis failed or timed out; check the local engine and installed model; text retained
```

An ambiguous audio
acknowledgement may mean audio reached Matrix; the CLI never retries text or
promises audio absence. Stdout remains only the text event ID. Retrying the
whole command creates another text post.

Deadlines: synthesis 120 s per process, each decode/encode 30 s, total local
render 4 min, upload/fetch/audio-send 60 s, explicit install 5 min. Native inference
runs in a reapable subprocess because CGO Generate has no context cancellation.
Text goes over stdin, never argv. Private 0700 scratch/0600 files are deleted
on return. Native stdout/stderr are discarded, never logged. No cloud/cloning
or private-content diagnostic logs are supported.

## Verification and delivery

```sh
gofmt -l .
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./internal/voice/... ./internal/cli/...
go vet -tags sherpa ./...
go test -tags sherpa -count=1 ./internal/voice ./internal/voiceworker ./internal/voicerelease ./internal/cli -run 'TestPostVoice|TestVoiceConfig|TestRender|TestCapPCM|TestWaveform|TestInstall|TestExtract|TestConfigStrict|TestModelCache|TestLocalRunner|TestVerifyWorker|TestWorker|TestAllowedLibrary|TestBuildPreserves'
# Explicitly install the pinned model under an isolated A2A_CONFIG_DIR first.
# Render, ffprobe, relocation and real damaged-library controls; never posts:
A2A_VOICE_IT_BASE=/absolute/isolated/config \
 A2A_VOICE_RELEASE_DIR=/absolute/release-1 \
 go test ./internal/voicerelease -run '^TestNativeRelease' -count=1 -v
# Existing optional long/capped witness now also requires both release pins:
A2A_VOICE_IT=1 A2A_VOICE_IT_LONG=1 \
 A2A_VOICE_IT_BASE=/absolute/isolated/config \
 A2A_VOICE_IT_EXECUTABLE=/absolute/release-1/a2a-voice-worker \
 A2A_VOICE_IT_WORKER_SHA256=WORKER_SHA256 \
 A2A_VOICE_IT_MANIFEST_SHA256=MANIFEST_SHA256 \
 go test ./internal/voice -run '^TestVoiceReal$' -count=1 -v
```

The gated witness copies the release to fresh temporary directories, points
GOMODCACHE at an empty directory, synthesizes and encodes Irina, renders Opus
back to PCM and checks non-silence/duration/waveform plus ffprobe's mono Opus
48 kHz result. Missing and single-byte-modified native libraries must fail both
direct execution and the parent's pre-exec hash check. It does not publish to Matrix,
play on the user's speakers or establish client UI acceptance. Missing tools
are failures, never passing Opus results. Unit tests cover text identity and
failure verdicts, event/reply/metadata and integrity ordering, cap/notice,
foreign/cloud option rejection, private scratch cleanup, wrong pins and unsafe archives.

Source delivery requires independent review of the exact signed candidate,
green CI for normal/native variants and a signed landing. Installation in a
shared location follows separately with the release digest, model/encoder
versions, prior config and byte-copy rollback recorded for the maintenance window.
