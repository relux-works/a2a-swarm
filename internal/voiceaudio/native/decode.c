/* Fixed local voice decoder protocol VAP1. No URLs, argv paths, or installs.
 * Build only against the exact source manifest; see voice-audio-decoder.md.
 * This file is project code; bundled libraries retain their own licenses. */
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <math.h>
#include <sys/resource.h>
#include <unistd.h>
#include <opusfile.h>
#include "speex_resampler.h"

#define MAX_SAMPLES 480000
#define FRAME48 5760

static int cap(int resource, rlim_t n) {
  struct rlimit r = {n, n};
  return setrlimit(resource, &r);
}

static int written;
static float clipmem;

/* At most one extra byte is emitted on overflow. Parent refuses the prefix. */
static int emit(float *out, unsigned n, int64_t target) {
  if ((int64_t)n > target - written) n = (unsigned)(target - written);
  opus_pcm_soft_clip(out, (int)n, 1, &clipmem);
  for (unsigned i = 0; i < n; ++i) {
    if (written == MAX_SAMPLES) {
      if (putchar(0) == EOF || fflush(stdout)) return 72;
      return 74;
    }
    float v = fmaxf(-32768.f, fminf(32767.f, out[i] * 32768.f));
    int16_t s = (int16_t)lrintf(v);
    uint16_t u = (uint16_t)s;
    if (putchar(u & 255) == EOF || putchar(u >> 8) == EOF) return 72;
    ++written;
  }
  return 0;
}

static int resample(SpeexResamplerState *rs, float *mono, unsigned n,
                    int64_t target) {
  float out[4096];
  unsigned consumed = 0;
  while (consumed < n) {
    spx_uint32_t inlen = n - consumed, outlen = 4096;
    int e = speex_resampler_process_float(rs, 0, mono + consumed, &inlen,
                                         out, &outlen);
    if (e || (!inlen && !outlen)) return 72;
    consumed += inlen;
    int status = emit(out, outlen, target);
    if (status) return status;
  }
  return 0;
}

int main(int argc, char **argv) {
  (void)argv;
  if (argc != 1) return 70;
  /* Refuse before opening input if any promised cap cannot be installed. */
  if (cap(RLIMIT_CPU, 10) || cap(RLIMIT_AS, 256 * 1024 * 1024) ||
      cap(RLIMIT_FSIZE, 0) || cap(RLIMIT_CORE, 0) ||
      cap(RLIMIT_NOFILE, 16) || cap(RLIMIT_NPROC, 0) ||
      cap(RLIMIT_STACK, 8 * 1024 * 1024)) return 73;
  int error;
  OggOpusFile *of = op_open_file("input.ogg", &error);
  if (!of) return 71;
  const OpusHead *head = op_head(of, 0);
  if (!op_seekable(of) || op_link_count(of) != 1 || !head ||
      head->version != 1 || head->mapping_family != 0 ||
      (head->channel_count != 1 && head->channel_count != 2)) {
    op_free(of); return 71;
  }
  int channels = head->channel_count;
  ogg_int64_t expected = op_pcm_total(of, 0);
  if (expected <= 0) { op_free(of); return 71; }
  SpeexResamplerState *rs = speex_resampler_init(1, 48000, 16000, 5, &error);
  if (!rs || error) { op_free(of); return 72; }
  if (speex_resampler_skip_zeros(rs)) {
    speex_resampler_destroy(rs); op_free(of); return 72;
  }
  /* VAP1, 16000 Hz, one channel, signed PCM16LE. Not a WAV header. */
  static const unsigned char header[12] =
    {'V','A','P','1',0x80,0x3e,0,0,1,0,16,0};
  if (fwrite(header, 1, sizeof(header), stdout) != sizeof(header)) return 72;
  float pcm[FRAME48 * 2], mono[FRAME48];
  ogg_int64_t total = 0;
  int status = 0;
  for (;;) {
    int link = -1;
    int n = op_read_float(of, pcm, FRAME48 * channels, &link);
    if (n < 0) { status = 71; break; } /* Includes OP_HOLE: no recovery. */
    if (!n) break;
    if (link != 0 || op_head(of, link)->channel_count != channels) {
      status = 71; break;
    }
    total += n;
    for (int i = 0; i < n; ++i)
      mono[i] = channels == 1 ? pcm[i] :
        (float)(((double)pcm[2*i] + pcm[2*i+1]) * 0.5);
    status = resample(rs, mono, (unsigned)n, total / 3);
    if (status) break;
  }
  if (!status && total != expected) status = 71;
  if (!status) {
    unsigned latency = speex_resampler_get_input_latency(rs);
    /* A fixed finite drain; never an unbounded read or flush loop. */
    if (latency > FRAME48) status = 72;
    else {
      for (unsigned i = 0; i < latency; ++i) mono[i] = 0;
      status = resample(rs, mono, latency, total / 3);
    }
  }
  if (!status && written != total / 3) status = 71;
  if (fflush(stdout)) status = 72;
  speex_resampler_destroy(rs);
  op_free(of);
  return status;
}
