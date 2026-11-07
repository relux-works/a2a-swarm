# Isolated local inbound transcription candidate

`internal/voicetranscribe` consumes already-fetched audio and invokes the
official offline sherpa-onnx CLI. It adds no inbox trigger, media download,
receipt, outbound voice change, installer, shell, remote ASR or Go module.
This first phase accepts canonical WAV only. Element Ogg/Opus decoding and
Part1 inbox/fetch composition require a later authorized candidate. This
package alone does not close inbound Element voice acceptance.

The following numerical limits and profile are proposals for Meta/source
adoption. Creating this source candidate installs no global policy or model.

## Composition API

```go
adapter := voicetranscribe.New(
    voicetranscribe.PinnedProfile("/absolute/physical/local-asr-profile"),
    callerTMPDIR, // explicit existing absolute owned 0700 physical directory
)
out := adapter.Transcribe(ctx, voicetranscribe.Request{
    Original: voicetranscribe.EventRef{EventID: eventID, FetchHandle: handle},
    Audio: fetchedWAV, // immutable until Transcribe returns
    Codec: voicetranscribe.CodecPCM16WAV,
    Encrypted: encrypted,
})
```

`Transcribe(context.Context, Request) Outcome` always copies `Original`
exactly. `Outcome` contains either `Transcript` or `Refusal`, never both.
There is no package-global default profile, environment cache discovery or
implicit enablement. A caller must choose this operation explicitly.

The caller owns authenticated sender/room checks, ingress encryption
classification, fetch permission and untrusted presentation. An event or
model output must never select a profile, path, command or engine option.
`EventRef` is opaque context, not evidence of authority. Speech never becomes
trusted instructions or a source of new actions.

The later decoder must emit exactly a 44-byte little-endian RIFF/WAVE header:
`RIFF`, exact file-size-minus-8, `WAVE`, `fmt ` with size 16, PCM tag 1,
one channel, 16000 Hz, byte rate 32000, block alignment 2, 16 bits, `data`,
exact positive even PCM byte count, followed only by those PCM bytes.
Extra chunks, metadata, extensible/compressed formats, trailing bytes and
truncation refuse. Relabelling Opus as WAV cannot satisfy the byte checks.
The decoder must preserve the original event reference and encryption
refusal and enforce its own bounded encoded input/decode work before this
call. Pass a common earlier context deadline to bound the composed operation.

| Exported bound | Proposed value |
| --- | --- |
| `MaxInputBytes` | 8 MiB; ingress reads at most cap+1 and refuses oversize |
| `SampleRate`, `Channels`, `BitsPerSample` | 16000, 1, 16 |
| `MaxPCMBytes`, `MaxDuration` | 960000 bytes, 30 seconds measured from PCM |
| Canonical accepted WAV bytes | 44 + positive even PCM bytes, at most 960044 |
| `ValidationDeadline` | 15 seconds for model/native validation and snapshot |
| `InferenceDeadline` | 60 seconds for one native subprocess |
| `TotalDeadline` | 90 seconds, clamped by any earlier caller deadline |
| Process-wide gate | One operation, no queue, shared across Adapter instances |
| `MaxStdoutBytes`, `MaxTranscriptBytes` | 65536, 16384 UTF-8 bytes; refuse excess |

The gate remains held through validation, process join and cleanup. Hash/copy
loops check context between bounded reads of regular local files; these are
not guarantees against an unresponsive filesystem or kernel. Provision only
on a local filesystem with normal bounded I/O. Native memory is not capped by
this package: input, model and concurrency bounds constrain work, but an OS
memory ceiling would require a separate deployment recipe. No host memory
limit or universal ASR accuracy is claimed.

Transcript text is untrusted machine output. `model` and `engine` are fixed
identities below; `language` is `en`, `ru`, or `unknown`. Unknown or absent
engine labels do not become arbitrary display text. `confidence` is always
JSON null with `confidence_reason="no calibrated confidence established"`;
token probabilities are not calibrated confidence. `untrusted` is always
true. `duration_ms` comes from admitted PCM bytes, not event metadata.
Malformed/ambiguous JSON, duplicate or aliased output keys, invalid Unicode,
empty text and excess output refuse; no partial transcript is returned.

Closed refusals are `encrypted_media`, `oversize`, `corrupt_audio`,
`unsupported_codec`, `missing_model`, `model_integrity`,
`backend_unavailable`, `backend_integrity`, `busy`, `timeout`, `cancelled`,
`engine_failed`, `invalid_output`, `output_limit`, `temporary_root`, and
`cleanup_failed`. Stages identify input, gate, scratch, model, backend,
inference, output or cleanup. A nonzero engine exit is only `engine_failed`;
the adapter does not infer a model cause or parse stderr. A timeout records
the context observation, not its underlying cause. Refusals retain no raw
error, argv, path or diagnostic. The event reference remains available for
the caller to present the refusal and keep explicit fetch available.

## Explicit offline profile

Provision a separate local directory before using the API:

```text
profile/
  bin/sherpa-onnx-offline
  lib/libonnxruntime.so       # Linux amd64
  # lib/libonnxruntime.dylib  # proposed macOS arm64 counterpart
  model/tiny-encoder.int8.onnx
  model/tiny-decoder.int8.onnx
  model/tiny-tokens.txt
```

`Profile` carries the exact executable/runtime paths, byte counts, SHA256s
and OS/architecture. `PinnedProfile(directory)` describes those required
bytes for the current supported platform; it does not create or qualify
them. `New` checks the supplied description against the pinned profile.
Missing, changed or redirected files refuse on each call. No shared existing
TTS cache is consulted, and a downloadable model is not an installed model.

Use [sherpa-onnx v1.13.8](https://github.com/k2-fsa/sherpa-onnx/releases/tag/v1.13.8)
official shared-no-tts archives. Publisher release-API archive digests:

| Platform/archive | Bytes | Publisher SHA256 |
| --- | ---: | --- |
| `sherpa-onnx-v1.13.8-linux-x64-shared-no-tts.tar.bz2` | 24802494 | `d0f96c8b65c6cd0974fada22737e337de81bc8cd2abbec2e39caf358b1eec5fc` |
| `sherpa-onnx-v1.13.8-osx-arm64-shared-no-tts.tar.bz2` | 18252168 | `91b96512c4fa1960f8a9ed5360a6c8dda53a4b5015d0590244f14086a234557a` |

Extract only expected safe regular members after offline archive verification.
The following member SHA256s are **local measurements of verified archive
bytes**, not separately published member digests:

| Member | Bytes | Local SHA256 |
| --- | ---: | --- |
| Linux `bin/sherpa-onnx-offline` | 2509672 | `95fb2157ba97830a57197129b19fbd93416b5c24be0ce5a23c47c834d8766aba` |
| Linux `lib/libonnxruntime.so` | 27026609 | `4b3607aebd1784b26b6f9b20e4bd974c7ab8287043e4d095cb7d2cb40b5e566e` |
| Mac `bin/sherpa-onnx-offline` | 2051152 | `23888c34222b5b47f4fe59d81ebd1bff1b1a683a01dcdc5ce4152892a23233b1` |
| Mac `lib/libonnxruntime.dylib` | 28775120 | `3567d114f7299d559993e536d605a6f46d7bc9d2542004accc80ee9bf5457f0b` |

Linux CLI `DT_NEEDED` lists `libonnxruntime.so` plus libpthread, libm,
libdl, libstdc++, libgcc_s, libc and the system loader. ONNX Runtime also
needs librt. The inspected CLI rpath starts with `$ORIGIN` and
`$ORIGIN/../lib`; its verified sibling is present in each snapshot. These
system libraries/loader remain a trusted OS dependency, not downloaded
profile files. Observed symbol requirements reach GLIBC 2.17 and GLIBCXX
3.4.19. Neither Sherpa C API nor C++ API library is needed by this CLI.
No inherited loader variables, PATH or parent working directory are used.

The Mac members are arm64 with inspected macOS 11.0 minimums. Static load
commands name only the sibling ONNX Runtime plus `/usr/lib/dyld`, libSystem,
libc++, libiconv, libobjc and the CoreML/Foundation/CoreFoundation system
frameworks. The CLI searches `@loader_path` and `@loader_path/../lib` before
its package-relative fallback; ONNX Runtime uses `@loader_path`. These
constraints and hashes do not qualify the complete OS runtime closure or
actual inference on a Mac. That host deployment audit and native execution
remain required. Native macOS is NOT_RUN on the Linux preparation seat.

Select the multilingual
[publisher export](https://huggingface.co/csukuangfj/sherpa-onnx-whisper-tiny/tree/65176e2deb88badc814a94058666cadccc29b61c),
revision `65176e2deb88badc814a94058666cadccc29b61c`, tiny int8, not tiny.en:

| File | Bytes | SHA256 / source |
| --- | ---: | --- |
| `tiny-encoder.int8.onnx` | 12937772 | `d24fb083ae3b1041fc24e97971d60e280c9342201fbb67b0ab428a8b4a51a434`, publisher LFS OID |
| `tiny-decoder.int8.onnx` | 89855401 | `d2fece8dd42771f1df975c6c0445770d0c292bf7547c2cae04a6c0cc57540925`, publisher LFS OID |
| `tiny-tokens.txt` | 816730 | `b34b360dbb493e781e479794586d661700670d65564001f23024971d1f2fa126`, local SHA256 of immutable Git blob `a4edf0b719c10d28cd83f8c294449f99ff5d5dc0` |

Model total: 103609903 bytes. Linux native payload: 29536281 bytes; complete
Linux profile payload: 133146184 bytes. One operation temporarily copies
that payload plus its WAV, with private directories; it does not multiply
profiles by queueing. Disk metadata and OS/process memory are additional.

[Whisper upstream](https://github.com/openai/whisper) lists Russian and
English and licenses its original code/weights under MIT. The official
[sherpa Whisper documentation](https://k2-fsa.github.io/sherpa/onnx/pretrained_models/whisper/index.html)
links the exporter. The converted repository has no separate model card or
license statement: retain the upstream license, and keep redistribution
clearance for this conversion as an unresolved release gate. Sherpa engine
code uses Apache-2.0. Tiny int8 trades accuracy for size; language support
and short public controls are not accuracy/WER qualification. SenseVoice's
documented language set does not establish Russian support.

Acquisition is a separate, explicitly authorized preparation step: anonymous
data-only HTTPS curl of exact pinned public URLs, offline digest verification,
safe extraction and native closure inspection. Never execute helpers or
installers in the acquisition namespace. Runtime does not acquire artifacts,
download models or contact any provider. The host must qualify the native
profile and OS runtime before adoption; no installer is supplied here.

## Private execution and checks

The caller's existing TMPDIR must be absolute, physically spelled, owned by
euid and exactly 0700. Every component is opened and checked through held
directory descriptors; symlink components and unsafe ownership/write modes
refuse. Root/euid ancestors may be sticky if writable. On macOS pass the
physical path (for example `/private/...`), not a `/tmp` alias.

Each operation creates a random 0700 directory and 0600 WAV. It copies and
hash-verifies opened regular no-follow/nonblocking model and native files,
checks their identity before/after copying, and runs only the private copy.
The executable is 0500; model/runtime files are 0400. Child cwd, 0700 HOME,
TMPDIR and XDG cache are inside that directory. The only other child
environment values bound OpenMP/OpenBLAS threads to two. CPU inference also
uses two threads and greedy decoding, task `transcribe`, tail padding 300.
Stdin is closed; bounded stdout exists only in memory; stderr is discarded.
There are no diagnostic files or fallback directories.

Cancellation kills the subprocess group and joins the direct subprocess and
stdout copier before removing anchored task-owned scratch. The official CLI
does not spawn child processes. Group cancellation also kills synthetic
descendants in tests; grandchildren may briefly be zombies awaiting the
namespace reaper, which is not an active inference process. Removal errors
replace any transcript with `cleanup_failed`, preserving the event reference.
An explicit cleanup refusal can leave recognizable private scratch for the
caller to remove; normal returns leave no operation files. Abrupt process or
host death cannot promise Go deferred cleanup. Never sweep unrelated paths.

Physical roots, descriptor checks and private copies protect against other
UIDs and source renames. A same-UID process can still rewrite trusted profile
configuration or chmod/mutate a private snapshot; no same-UID isolation is
claimed. No live credentials, private audio or secret files are required by
the package or its tests.

Run affected-package ordinary/race/vet with the exact named Go 1.25.5
toolchain, GOMAXPROCS=2, `-p=2`, offline and with private HOME/TMP/cache:

```sh
go test -p=2 -count=1 -timeout=100s ./internal/voicetranscribe
go test -p=2 -race -count=1 -timeout=100s ./internal/voicetranscribe
go vet -p=2 ./internal/voicetranscribe
```

`A2A_STT_IT_PROFILE` and `A2A_STT_IT_FIXTURES` opt into the real engine tests
using the explicit directory above and publisher public `en-english.wav` /
`ru-russian.wav` fixtures. Without them those tests skip explicitly.
Synthetic executable tests prove process, privacy, bounds and parsing only;
they do no ASR. Real tests check nonempty English/Russian transcripts and
language labels, including Cyrillic for Russian, without printing text.
Linux native process negatives deliberately SIGSTOP only the observed
operation-owned native child to hold the inference boundary for busy,
deadline/cancel/join and cleanup controls; every case has a real valid
neighbor. No public audio bytes or native stdout/stderr belong in receipts.

Actual Element delivery, Ogg/Opus decoder composition, native Mac inference,
independent review and signed source landing remain separate acceptance gates.
