# Local bounded Ogg/Opus to WAV decoder candidate

`internal/voiceaudio` supplies the missing decode boundary before inbound STT.
It takes already fetched bytes and returns strict 16 kHz mono PCM16LE WAV.
It does not implement inbox/fetch, inference, outbound voice, or issue #237
acceptance. Existing outbound voice's PATH ffmpeg is not a provisioned decoder.
No existing files or Go module dependencies are changed by this component.

## API and caller obligations

```go
audio, refusal := voiceaudio.Decode(ctx, voiceaudio.Request{
    Bytes: fetchedBytes,
    Encrypted: encrypted,
    TMPDIR: explicitPrivateTempRoot,
    Profile: voiceaudio.Profile{
        ID: voiceaudio.ProfileID,
        Executable: provisionedAbsoluteExecutable,
        SHA256: admittedExecutableSHA256,
        Platform: "linux/amd64",
    },
})
```

The profile is trusted local configuration. Peer content cannot select tools,
paths, arguments, codecs, URLs or limits. The caller reads at most 8 MiB + 1
before constructing the bounded byte slice and must not mutate it concurrently.
No event/MIME/duration field is consulted. On **every** refusal Audio is zero;
outer composition retains the original event and fetch handle. A successful
Audio has `WAV`, `Samples` and measured `Duration`; it has no transcript,
confidence, model or ASR claim. Pass only WAV to the separate inference seam.

WAV is exactly 44 header bytes followed by signed little-endian 16-bit mono
samples: RIFF size 36+PCM bytes, WAVE, fmt size 16, PCM code 1, channels 1,
rate 16000, byte rate 32000, block align 2, bits 16, data size PCM bytes.
No extra chunks, metadata, trailing bytes or partially successful output.

Closed `Refusal.Reason` values are encrypted_media, input_limit,
malformed_audio, unsupported_codec, ambiguous_stream, missing_decoder_profile,
decoder_profile_mismatch, platform_unqualified, unsafe_tmpdir, decoder_busy,
cancelled, timeout, decode_failed, invalid_decoder_output, decoder_output_limit, duration_limit,
cleanup_refused. No raw stderr, stdout, paths or media contents enter errors.
Exit 71 means native malformed input, 72 an unclassified processing failure,
73 inability to install the declared resource profile, 74 duration overflow.
Signals/nonzero statuses without that protocol remain decode_failed.

## Actual decode and bounds

Go checks every Ogg CRC, version, serial, sequence, BOS/EOS, continuation,
packet and header boundary. It accepts a deliberately strict RFC 3533/7845
subset: one uncropped stream, OpusHead v1, mapping family 0, 1/2 channels,
tags at most 64 KiB, no recapture/holes/chains/multiplexing/trailing data.
Other valid Opus features, such as multichannel or cropped initial granules,
refuse; supporting them requires a new contract. Tags remain opaque bytes.
EOS may trim the final packet, not hide arbitrary prior packets. TOC-derived
timing and final granules are checked; these never replace actual decoding.

The native helper independently uses `op_open_file("input.ogg")`, validates
one seekable link and its channel/mapping header, and calls `op_read_float`.
It refuses **all** negative returns, including OP_HOLE. This official API
returns interleaved float samples at 48 kHz, independent of the informational
Opus input rate. Mono stays mono; stereo is averaged in floating point.
The bundled Speex resampler runs 48000->16000, quality 5, skips its initial
latency and drains a bounded latency tail. Output count is floor(actual
decoded 48k sample count / 3), compared to opusfile's count and Go granules.
Soft clipping and saturating round-to-nearest conversion produce PCM16LE.
Sources: [opusfile decoding API](https://opus-codec.org/docs/opusfile_api-0.12/group__stream__decoding.html),
[opusfile header/API](https://opus-codec.org/docs/opusfile_api-0.12/opusfile_8h_source.html),
[RFC 7845](https://www.rfc-editor.org/rfc/rfc7845.html),
[RFC 6716](https://www.rfc-editor.org/rfc/rfc6716.html).

Input limit is 8 MiB. Output limit is 480000 samples / 960000 PCM bytes /
30 seconds. Both helper and Go retain at most **one extra byte**, then refuse
instead of returning a prefix. The helper emits VAP1 (12 fixed bytes declaring
16000/mono/16-bit) then PCM; Go validates it and constructs the WAV itself.
Stderr is counted and discarded with a 16 KiB cap plus one overflow byte;
exceeding it kills the process group and refuses decoder_output_limit without
retaining diagnostic text.
One process-wide decode is admitted; concurrent work refuses busy with no queue.
Operation wall limit is 15 seconds (caller cancellation/deadline may shorten it).
Native hard+soft limits are CPU 10s, address space 256 MiB, file size 0, core 0,
open files 16, processes 0 additional forks, stack 8 MiB. The real Linux run
traced successful installation of all seven limits; no fork/connect occurred.

The helper is fixed code with no argv and no shell. It links opusfile, opus,
ogg and resampler, never opusurl, SSL, playback or playlist libraries. No
download, installer, model, remote provider or network API is called. Stock
`opusdec --rate 16000 --no-dither input -` would emit raw PCM at the input
channel count, not force mono; there is no documented --mono. Its 0.2 source
also breaks on some decode errors before returning 0. Therefore this small
helper uses the same official libraries/resampler with stricter error handling,
rather than treating opusdec exit status as proof. [Official CLI semantics](https://opus-codec.org/docs/opus-tools/opusdec.html)
and `opus-tools-0.2/src/opusdec.c:1009-1020,1207` ground this decision.

## Scratch, pins and supervision

TMPDIR is explicit, absolute, clean, owned by euid, 0700, and has no symlink
components. Every ancestor must be euid/root-owned and not group/other-writable
unless sticky. No implicit HOME or /tmp fallback. Descriptor-rooted operations
create a random 0700 operation directory, 0600 input, 0700 helper snapshot and
private HOME/cache/tmp. The child gets only those paths and C locale, no PATH
or inherited loader environment; cwd is the private operation directory.

The source executable is a bounded regular owned non-writable-by-others file,
opened with identity comparison. SHA256 authenticates the exact copied bytes.
Both source and copy must be static little-endian x86-64 ELF ET_EXEC with no
PT_INTERP/PT_DYNAMIC. This closes native loader dependencies inside one pin.
Profile ID is `xiph-opus-1.6.1-opusfile-0.12-ogg-1.3.6-resampler-0.2-vap1`.
The measured Linux artifact is 1133568 bytes, SHA256
`832899b89352acc68ed899f6e7a2f53eff1890e0ad5974e38102478b933ffc02`.
It is a build-specific pin, not a universal upstream binary digest.

Cancellation kills the owned process group, waits for the direct child and pipe
goroutines, then cleans scratch. The admitted native helper cannot fork after
RLIMIT_NPROC=0 and has no fork code. Separate inert controls exercise unexpected
descendant group killing/reaping in the isolated PID namespace. Cleanup uses
held roots and removes only recorded file identities and empty created dirs;
unexpected/replaced files survive and force cleanup_refused. It never recursively
deletes the caller root. Same-UID malicious mutation of private files, including
after final hash verification before exec, remains outside Unix mode protection.

## Exact source acquisition and Linux build

No ffmpeg or downloaded executable is required. Official sources, versions,
SHA256 and observed licenses:

| Source | SHA256 | License |
|---|---|---|
| libogg-1.3.6.tar.xz | 5c8253428e181840cd20d41f3ca16557a9cc04bad4a3d04cce84808677fa1061 | BSD-3-Clause |
| opus-1.6.1.tar.gz | 6ffcb593207be92584df15b32466ed64bbec99109f007c82205f0194572411a1 | BSD-3-Clause plus upstream patent notices |
| opusfile-0.12.tar.gz | 118d8601c12dd6a44f52423e68ca9083cc9f2bfe72da7a8c1acb22a80ae3550b | BSD-3-Clause |
| opus-tools-0.2.tar.gz | b4e56cb00d3e509acfba9a9b627ffd8273b876b4e2408642259f6da28fa0ff86 | bundled resampler BSD-3-Clause |

[Xiph downloads](https://opus-codec.org/downloads/),
[Opus publisher checksums](https://downloads.xiph.org/releases/opus/SHA256SUMS.txt),
[Ogg publisher checksums](https://downloads.xiph.org/releases/ogg/SHA256SUMS).
opusfile 0.12 and opus-tools 0.2 remain the stable versions published there;
their old dates are not a claim of recent releases. The official Opus stable
version checked here is 1.6.1. Preserve all archive COPYING files, Opus patent
notices and the resampler's source license when packaging. The Linux link also
contains provisioned glibc 2.43 (LGPL-2.1-or-later) and GCC support runtime
(GCC exception); static distribution/relinking obligations remain release work.
The evidence packet includes the source archives and project relink objects;
it is not a signed redistributable release.

In a separately admitted **DATA-only** curl namespace, anonymously acquire
exact URLs with direct curl, no online helpers or code execution:

```sh
/usr/bin/curl -q --noproxy '*' --proto '=https' --proto-redir '=https' \
  -fL --max-redirs 3 --connect-timeout 10 --max-time 110 \
  -o /data/libogg-1.3.6.tar.xz https://downloads.xiph.org/releases/ogg/libogg-1.3.6.tar.xz
# Same curl options, each explicit output/URL:
# /data/opus-1.6.1.tar.gz https://downloads.xiph.org/releases/opus/opus-1.6.1.tar.gz
# /data/opusfile-0.12.tar.gz https://downloads.xiph.org/releases/opus/opusfile-0.12.tar.gz
# /data/opus-tools-0.2.tar.gz https://downloads.xiph.org/releases/opus/opus-tools-0.2.tar.gz
```

The exact offline Linux implementation is
`internal/voiceaudio/native/build-linux.sh`. Bind this checkout at `/task/repo`
and acquired archives at `/task/data`, both read-only. Use a new private `/build`
and empty `/native`, private 0700 HOME/TMP/cache, read-only public `/usr`, `/bin`,
`/lib`, `/lib64`, `/sbin`, and the named Go1.25.5 directory at `/go`. Clear env,
unshare all namespaces, no network, GOMAXPROCS=2, one suite, timeout <=1800s.
Explicitly supply VOICEAUDIO_TOOLCHAIN_MANIFEST and its admitted SHA256;
the script refuses absent/wrong pins and checks every listed tool/input before
building. The exported manifest uses namespace-absolute paths and records the
measured real GCC/libc/build tools plus Go executables; changing tools requires
a new manifest admission, never trusting whatever PATH provides.

The build checks SOURCES.sha256 before extracting the four exact archives,
configures only static libraries, disables Opus intrinsics/RTCD/extra programs
and opusfile HTTP/examples/docs, and stages headers/libraries only inside its
private `/build/prefix` (no system/user install). It compiles the helper and
bundled resample.c, statically links only libopusfile/libopus/libogg/libm/libc,
exports relink objects, and prints ELF/versions/hashes. Real tools observed:
GCC 15.2.0 (Ubuntu 15.2.0-16ubuntu1), binutils 2.46, glibc 2.43-2ubuntu2.4,
Go1.25.5. ELF states x86-64 baseline and Linux ABI minimum 3.2.0; tested kernel
is 7.0.0-38-generic. ABI-note minimum is not execution proof on an old kernel.

## macOS arm64 recipe and explicit blocker

**NOT RUN / NOT ENABLED.** This Linux packet has no admitted Apple SDK/compiler
manifest, native Mach-O artifact/hash, loader closure or effective memory-cap
witness. Other platforms return platform_unqualified. Cross-compiling the Go
adapter is compile evidence only. A hash value is never invented for an
unbuilt Mac binary. Xiph's old OS X utility download is not an arm64 profile.

Use the **same four source bytes/checksums**. On a separately admitted native
arm64 Mac, first provision an exact Apple compiler/SDK and build-tool manifest,
including compiler, linker, archiver, ranlib, SDK headers/stubs and runtime OS
identity. No Homebrew, xcode-select installer or runtime download belongs in
this recipe. Reject an absent manifest. With those verified read-only tools,
`native/prepare-darwin-arm64.sh` specifies every source configure/compile/link
command and fails without manifest pins. It is preparation, not qualification.
Its proposed build delta from build-linux.sh is:

```sh
# Fixed namespace paths are externally provisioned and hash-verified first.
export CC=/toolchain/usr/bin/clang AR=/toolchain/usr/bin/ar
export RANLIB=/toolchain/usr/bin/ranlib LD=/toolchain/usr/bin/ld
export CFLAGS='-O2 -fstack-protector-strong -arch arm64 -mmacosx-version-min=12.0 -isysroot /sdk'
# For each library: identical configure options/prefix, additionally
# --build=aarch64-apple-darwin --host=aarch64-apple-darwin; make -j2.
# Compile decode.c/resample.c with the same include paths/defines as Linux.
# Link these static project/Xiph objects with $CC $CFLAGS, omit -static and
# GNU --build-id, permit only the SDK's system libSystem (not extra dylibs).
# Inspect using the provisioned, pinned otool:
/toolchain/usr/bin/otool -hv /native/voiceaudio-decode
/toolchain/usr/bin/otool -l /native/voiceaudio-decode
/toolchain/usr/bin/otool -L /native/voiceaudio-decode
```

Apple's arm64 floor 11.0 is grounded in its
[arm64 helper deployment guidance](https://developer.apple.com/documentation/xcode/embedding-a-helper-tool-in-a-sandboxed-app)
and [explicit target example](https://developer.apple.com/documentation/apple-silicon/building-a-universal-macos-binary).
The proposed helper target is **12.0**, because the Go1.25.5 adapter requires
macOS 12 or later per [Go 1.25 release notes](https://go.dev/doc/go1.25#darwin).
The first short design's 11.0 described the C/arm64 floor only; it was not the
whole adapter minimum. Require an arm64 Mach-O, macOS LC_BUILD_VERSION minOS
12.0, exact dylinker and
libSystem load-command allowlist, no rpaths/weak/reexport/plugin loads, and hash
the resulting artifact plus its measured OS/runtime closure. Then establish
actual CPU/address-space/process/file/output caps on that Mac; Apple's
[setrlimit documentation](https://developer.apple.com/library/archive/documentation/System/Conceptual/ManPages_iPhoneOS/man2/setrlimit.2.html)
does not establish equivalence to Linux RLIMIT_AS. If the 256 MiB hard address
space policy cannot be demonstrated, retain refusal and admit a separate bounded
native supervisor design. Do not enable the stub before those checks and the
native fixture/negative/race/vet run. This is an exact source/configuration
preparation recipe with unresolved tool/runtime pins, not a closed Mac artifact.

## Verification and composition limits

Tests use locally encoded public synthetic tones, never third-party speech or
live input. They assert WAV schema/sample count/duration, mono/stereo tones,
30s and one-sample-over behavior, CRC and actual native packet corruption,
unsupported/chained/encrypted media, profile/path pins, cancellation/timeout,
bounded output, clean environment, snapshot replacement, foreign preservation
and valid neighbors. Inert controls exercise adapter behavior only.

```sh
# In the offline namespace, after explicit provisioning:
# A2A_VOICEAUDIO_NATIVE=1
# A2A_VOICEAUDIO_EXECUTABLE=/native/voiceaudio-decode
# A2A_VOICEAUDIO_SHA256=832899b89352acc68ed899f6e7a2f53eff1890e0ad5974e38102478b933ffc02
# A2A_VOICEAUDIO_CONTROLS=/native
go test -p=1 -count=1 ./internal/voiceaudio
go test -p=1 -race -count=1 ./internal/voiceaudio
go vet -p=1 ./internal/voiceaudio
```

Without opt-in, native tests explicitly skip; opting in without provisioned
files/pins fails. No skip proves decoding. The packet records actual commands,
exits/times/counts, failures, causal checksum-guard plant, exact restoration and
final checks. Independent review, signatures, release, inbox/Element acceptance
and native Mac remain external. Root composes fetch -> decoder -> STT and must
retain event/fetch identity and untrusted-transcript presentation there.
