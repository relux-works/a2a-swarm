# Optional local voice replies

TASK-261023-m8mo6f under STORY-261023-6cr0bz. Owner request2026-10-23T12:22:54Z
`$P5pexOsC7w_MX-qZLi8-9hpRUWoCxAOHoU09jwovqVE`, independently verified by Meta
`$9UWwffQWwQskuxnEiusR7zY0k0a1RNFtVLnyLAQaTCU`; keeper scope
`$ag-AJvPpWv0Bv2nL-AczgDFU-75OKTRc7S6DPWtnKzA` and adopted invariants
`$_mvJw0mASKba6gxyTlHHgXG4KdrxL7aCFHafXCGq9K4`.

## Use

The existing text post is authoritative and unchanged. Add `--voice` to one
post for a local spoken copy replying to that text in the same topic. There
is no sticky mode or implicit enablement.
Irina is the keeper's operational interpretation of the owner's “second distinct
Go voice”, acknowledged by Meta with the original ambiguity retained. The owner
did not literally name a model. Stress/pronunciation defects remain. Scope narrowing: `$I17bMT13_Bm0gg0A-H-PXapr8kTnl8Se3sgrxXGE-58`, provenance `$ViV6k1qL0k04KFFBy9cs6tCkG_yqO6u5lJfDTa6Lmig`.

```sh
go build -tags sherpa -o .temp/voice/a2a-voice ./cmd/a2a
.temp/voice/a2a-voice voice install
.temp/voice/a2a-voice voice config --enable
.temp/voice/a2a-voice post ROOM --topic TOPIC --voice 'Complete original text'
```

Config/install require no Matrix account. `<config>` means `A2A_CONFIG_DIR`,
otherwise `~/.config/a2a-swarm`. Config is `<config>/voice/config.json`, with
exactly `enabled: true` and `max_seconds: 180`. Enabling is explicit, and
there is no engine/voice picker or implicit enablement. These commands change
no binding/profile/hook or subscription. Shared-station activation requires separate admission after
source delivery; the commands document the interface.

## Dependencies and pins

The single route currently requires macOS: sherpa-onnx through the native Go
binding `github.com/k2-fsa/sherpa-onnx-go-macos v1.13.8`, CGO, CPU, four threads
and the Piper ru_RU Irina model. Build with `-tags sherpa`. Normal builds retain
text operation without inference linkage; requesting voice there retains text
and warns. Other platforms retain normal text operation. The tagged executable
depends on pinned native dylibs at its Go module-cache path; retain that
dependency directory while using it. There is no say engine or voice picker.

`voice install` alone downloads:

- URL: `https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/vits-piper-ru_RU-irina-medium.tar.bz2`
- Archive SHA256: `1fc0f54e5e084fe287c07909f2f6e0ba6d857864cf800e3ab80286a4e8233008`
- File-manifest SHA256: `e7c154c19f233349ff4c3578316b43cb144f594af9411aab4814dbfd69a63392`
- Cache: `<config>/voice/models/<archive-sha256>/`, private files/directories.

Install verifies the archive before extraction, rejects unsafe paths/links
and oversized data, checks the pinned manifest and publishes atomically.
Existing installs are verified instead of overwritten. Every sherpa render
verifies installed asset hashes. Posting never installs/downloads. The bundle
retains its upstream model card, which marks dataset license Unknown. This
feature asserts no model licensing clearance and does not redistribute it.

For #185, the dedicated `--voice-worker` process opens the installed model
directory, checks its identity against the non-symlink directory it inspected,
enters it through that descriptor, and revalidates the pinned manifest/assets
through the held working directory before native construction. Model and token
filenames and `espeak-ng-data` are relative to that directory. DataDir is always
the 14-byte `espeak-ng-data`, independent of config-root or TMPDIR length;
the output filename becomes absolute before entering the model directory.
The worker holds cwd through generation, saving and native object destruction.
The parent CLI never changes cwd. Concurrent renders and truncation notices
run in separate reapable workers. No model alias, short-root selection, resource
copy, cache migration or additional temporary artifact is involved.

This preserves manifest authentication and rejects redirected model roots/assets;
holding cwd also survives replacement of the source directory's pathname.
It does not make model contents immutable against a same-UID writer after
validation. The existing parent process deadlines still bound validation and
native failure. Public failure strings and `[VOICE_TEXT_ONLY]` are unchanged;
there is no new #171 error category. PR153 must carry this path preparation
into its dedicated helper while retaining its separate executable/library
authentication and snapshot lifecycle.

`ffmpeg` must be on PATH with libopus encode and Opus decode support. Output
is mono Ogg/Opus,48kHz,24kbps, voice application. There is no qualified
`afconvert` Opus fallback in v1: unavailable/failed encoding retains text and
warns. `ffprobe` is useful for independent checks, not required to post.

## Text, cap and failure contract

The CLI first uses the existing text-send path and prints its event ID.
Failed text retains its original nonzero exit and starts no voice work.
Without `--voice`, no renderer/encoder/installer/model download runs.

A single clip is capped at **180 seconds**. Input beyond6000 Unicode runes
is shortened before synthesis to bound native work. If this bound or the
measured audio limit truncates speech, the clip reserves space for the spoken
notice: “Аудио сокращено. Полный текст сохранён в сообщении.” The caption
also identifies truncation; the original text stays complete. Chunking is
deferred. A50ms reserve covers Opus padding. Final decoded duration must not
exceed180s. Duration and waveform come from decoding encoded bytes to local
signed mono PCM, rather than estimating from text length.

After upload, authenticated fetch-back must match the exact byte count and
SHA256 before publishing any audio event. It uses `m.audio`, MSC3245 voice
and MSC1767 audio/file/text metadata, replies to the text and retains its
`m.thread` root. No second a2a decision/request header or duplicate mentions
are sent. See the [MSC3245 transitional format](https://github.com/matrix-org/matrix-spec-proposals/blob/83f6c5b469c1d78f714e335dcaa25354b255ffa5/proposals/3245-voice-messages.md).

Missing config, synthesis/encoding/upload/fetch failure, mismatched bytes,
or failed/ambiguous audio-send acknowledgement emit one stderr line marked
`[VOICE_TEXT_ONLY]` and exit0: the text already succeeded. An ambiguous audio
acknowledgement may mean audio reached Matrix; the CLI never retries text or
promises audio absence. Stdout remains only the text event ID. Retrying the
whole command creates another text post.

Deadlines: synthesis120s per process, each decode/encode30s, total local
render4min, upload/fetch/audio-send60s, explicit install5min. Native inference
runs in a reapable subprocess because CGO Generate has no context cancellation.
Text goes over stdin, never argv. Private0700 scratch/0600 files are deleted
on return. Native stdout/stderr are discarded, never logged. No cloud/cloning
or private-content diagnostic logs are supported.

## Verification and delivery

```sh
go test ./internal/voice ./internal/cli -run 'TestPostVoice|TestVoiceConfig|TestRender|TestCapPCM|TestWaveform|TestInstall|TestExtract|TestConfigStrict'
go vet ./...
go test -race ./...
go vet -tags sherpa ./...
go test -tags sherpa -race ./...
# Actual local synthesis/encode/decode; explicitly prepared isolated base:
A2A_VOICE_IT=1 A2A_VOICE_IT_BASE=/absolute/isolated/config \
 A2A_VOICE_IT_EXECUTABLE=/absolute/candidate/a2a-voice \
 go test -tags sherpa ./internal/voice -run '^TestVoiceReal$' -v
# Add A2A_VOICE_IT_LONG=1 for the actual long/capped synthesis witness.
# Paired #185 witness, using separately prepared/authenticated installed models:
# A2A_VOICE_IT_BASE must give a DataDir shorter than257 bytes, and
# A2A_VOICE_IT_LONG_BASE must give one of at least257 bytes. No auto-install/copy.
A2A_VOICE_IT=1 A2A_VOICE_IT_BASE=/absolute/isolated/short-config \
 A2A_VOICE_IT_LONG_BASE=/absolute/isolated/long-per-connection-config \
 A2A_VOICE_IT_EXECUTABLE=/absolute/candidate/a2a-voice A2A_VOICE_IT_LONG=1 \
 go test -tags sherpa ./internal/voice -run '^TestVoiceReal$' -v
```

The gated witness synthesizes and encodes Irina, renders Opus back to
PCM and checks non-silence/duration/waveform. It does not publish to Matrix,
play on the user's speakers or establish client UI acceptance. Missing tools
are failures, never passing Opus results. Unit tests cover text identity and
failure verdicts, event/reply/metadata and integrity ordering, cap/notice,
foreign/cloud option rejection, private scratch cleanup, wrong pins and unsafe archives.

The #185 Linux controls use tiny synthetic pinned fixtures and an inert native
constructor callback at the production path boundary. They establish relative
path spelling, asset validation, output placement, source-rename isolation and
independent worker lifetimes; they do not establish Sherpa/espeak relative-path
handling or real speech/Opus. The paired gated witness above must be run on the
exact candidate head by the authorized Mac operator with pinned native assets
and encoder, including cap/notice. Both current CI jobs use
`[self-hosted, macOS, relux-a2a]`; configured jobs and Linux passes are not
evidence of a successful native run. The CI native job links/builds Sherpa and
runs affected unit contracts, but does not enable `TestVoiceReal` or install
the model. Native confirmation remains a delivery gate.

Source delivery requires own complete and keeper exact-head review, real
hosted CI for normal/native variants, landing window and signed plain FF.
Then prepare activation for keeper review and Meta admission: exact landed
head; separate pinned executable/path/SHA/invocation; module/native dylib,
model and encoder hashes/paths; config/prior state; rollback restoring prior
invocation/config and deleting only task-owned artifacts. Never replace
shared `~/.local/bin/a2a` or change profiles/bindings/fleet for this slice.
