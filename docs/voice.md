# Optional local voice replies

Local voice scope requested 2026-10-23T12:22:54Z.

## Use

The existing text post is authoritative and unchanged. Add `--voice` to one
post for a local spoken copy replying to that text in the same topic. There
is no sticky mode or implicit enablement.
Irina is the operational interpretation of the requested “second distinct
Go voice”, with the original ambiguity retained. The owner
did not literally name a model. Stress/pronunciation defects remain.

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

## Read-only status

`a2a voice status [--json]` inspects local configuration without enabling voice,
installing assets, running a process, looking up credentials or contacting Matrix.
It works without an account. With named connections, public `connections.json`
and the selected identity/profile determine the same config directory used by
voice configuration; unrelated credential and owner-DM state is not consulted.

The closed status fields are:

- `config`: `enabled`, `missing`, `invalid` or `unavailable`.
- `native`: `compiled` for a Darwin/CGO/Sherpa build, otherwise `unavailable`.
  Compiled support does not prove a native engine will initialize successfully.
- `model`: `missing`, `invalid`, `unavailable` or `present_unverified`.
  A directory's presence does not authenticate its manifest or assets.
- `encoder`: `found_unverified` or `unavailable`, from PATH lookup only.
  This does not run ffmpeg or establish its Opus support.
- `max_seconds`: the fixed audio cap, 180.
- `synthesis`: always `not_checked`; `last_attempt`: always `not_recorded`.

Status exits zero after reporting local observations, including missing or invalid
voice configuration; usage and routing failures remain nonzero. It never reports
readiness or a previous attempt's cause from these observations. Attempt failures
remain the safe categories in the original `[VOICE_TEXT_ONLY]` warning, with the
text retained. No history is written and no automatic retry is introduced.

## Dependencies and pins

The single route currently requires macOS: sherpa-onnx through the native Go
binding `github.com/k2-fsa/sherpa-onnx-go-macos v1.13.8`, CGO, CPU, four threads
and the Piper ru_RU Irina model. Build with `-tags sherpa`. Normal builds retain
text operation without inference linkage; requesting voice there retains text
and warns. Other platforms retain normal text operation. The tagged executable
depends on pinned native dylibs at its Go module-cache path; retain that
dependency directory while using it. There is no say engine or voice picker.

The `go 1.25.5` directive in `go.mod` is a minimum Go version, not an exact
toolchain pin. Reproducing a worker binary's digest requires the same exact
toolchain and build inputs; the #185 Linux qualification uses Go 1.25.5.

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

For #185, only the dedicated `--voice-worker` process opens the installed model
directory, checks its identity against the non-symlink directory it inspected,
and enters it through that held descriptor. It revalidates the pinned manifest
and every asset through cwd before constructing the native engine. Model and
token filenames and the 14-byte `espeak-ng-data` are relative to that directory,
independent of config-root length. The output filename becomes absolute before
chdir. The worker holds cwd through generation, saving and native destruction;
the parent CLI never changes cwd. Concurrent renders and truncation notices
use separate reapable workers. No model alias, copy, cache migration or new
temporary artifact is needed.

Descriptor binding survives rename/replacement of the source directory pathname;
it does not make contents immutable against a same-UID writer after validation.
Model-root symlinks and symlinked asset components are rejected; parent-path
symlinks may resolve to an authenticated model. The existing parent deadlines
bound validation/native failure. Public failure text and `[VOICE_TEXT_ONLY]`
remain unchanged. A future dedicated-helper release must preserve this path
preparation alongside its separate executable/library authentication.

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
The synthesis deadline is fixed: it does not scale with host load. Neither
`voice config --enable` nor `voice/config.json` offers a timeout setting;
the strict config accepts only `enabled` and the fixed `max_seconds: 180`.
These are also the affected bb71277 source's limits. A short failed attempt,
reported load, or earlier successful synthesis does not establish its cause.

Synthesis warnings carry only parent-observed categories: model/config
validation refused; executable resolution or spawn refused; context cancelled;
context deadline exceeded; worker exited (status 1..255, or status unavailable);
or unclassified failure. A signal or context observation does not explain an
engine failure. Worker exit status is not interpreted as a model/engine reason;
there is no worker diagnostic protocol or stderr parsing. Decode, encode and
invalid-audio failures retain their separate stages. Unknown runner/renderer
errors and enclosing error messages contribute no diagnostic text. Every
failure still emits only one `[VOICE_TEXT_ONLY]` line after the text receipt,
with no automatic retry. Failed or ambiguous audio sends retain the existing
acknowledgement limits described above.

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
# For #185, the authorized Mac operator runs this existing witness twice on
# the exact candidate: separately prepared short and long absolute bases with
# original DataDir lengths <257 and >=257 bytes, and A2A_VOICE_IT_LONG=1 in both.
# The witness logs original_data_dir_bytes; it never installs/copies a model.
```

The gated witness synthesizes and encodes Irina, renders Opus back to
PCM and checks non-silence/duration/waveform. It does not publish to Matrix,
play on the user's speakers or establish client UI acceptance. Missing tools
are failures, never passing Opus results. Unit tests cover text identity and
failure verdicts, event/reply/metadata and integrity ordering, cap/notice,
foreign/cloud option rejection, private scratch cleanup, wrong pins and unsafe archives.

The #185 Linux tests use tiny synthetic pinned fixtures and actual inert child
processes at the production path boundary. They establish relative spellings,
asset validation, output placement, rename isolation, cancellation and concurrent
worker lifetimes. They do not establish Sherpa/espeak accepting relative paths
or whether the native library canonicalizes them back to a long physical path.
Real macOS synthesis and decoded non-silent bounded Opus, including cap/notice,
remain an external exact-candidate delivery gate. Darwin compilation without
Sherpa is not native synthesis. No new live opt-in or workflow change is needed:
the existing full-package CI suites already include the path controls.

Source delivery requires own complete and keeper exact-head review, real
hosted CI for normal/native variants, landing window and signed plain FF.
Then prepare activation for keeper review and Meta admission: exact landed
head; separate pinned executable/path/SHA/invocation; module/native dylib,
model and encoder hashes/paths; config/prior state; rollback restoring prior
invocation/config and deleting only task-owned artifacts. Never replace
shared `~/.local/bin/a2a` or change profiles/bindings/fleet for this slice.
