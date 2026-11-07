# Synthetic Ogg/Opus controls

These are locally generated sine waves, not Element captures or human speech.
`native/synthetic_encode.c` uses official libopus 1.6.1 and libogg 1.3.6,
48000 Hz input, 960-sample frames, 24 kbps, complexity 4, application AUDIO,
mapping family 0. Mono is 440 Hz; stereo is 440 Hz left, 660 Hz right.
Encoder lookahead from OPUS_GET_LOOKAHEAD is written as pre-skip; EOS granule
is original sample count + lookahead. libogg writes pages and checksums.

| File | Input channels | Input samples at 48 kHz | Expected mono samples at 16 kHz | Result |
|---|---:|---:|---:|---|
| mono-1s.ogg | 1 | 48000 | 16000 | canonical WAV, 32044 bytes |
| stereo-1s.ogg | 2 | 48000 | 16000 | canonical WAV, 32044 bytes |
| mono-30s.ogg | 1 | 1440000 | 480000 | canonical WAV, 960044 bytes |
| mono-over-30s.ogg | 1 | 1440003 | 480001 | duration_limit, no audio |
| mono-31s.ogg | 1 | 1488000 | 496000 | duration_limit, no audio |

Generate offline using the explicitly built `synthetic-encode`, arguments
`CHANNELS INPUT_SAMPLES_AT_48K OUTPUT_FILE`. SHA256SUMS binds fixture bytes.
The encoder is test preparation only and is absent from the runtime profile.
Adapter `native/control.c` probes are explicitly inert/non-codec processes;
they never establish Opus or ASR qualification. Their static binaries have
separate fixed pins in process_linux_amd64_test.go. Ordinary repository tests
skip opt-in native witnesses unless A2A_VOICEAUDIO_NATIVE=1; if opted in,
missing binaries or pins fail instead of skipping.
