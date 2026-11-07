/* Offline test-fixture generator. Synthetic sine waves, never human audio.
 * Uses the pinned official libopus encoder and libogg page writer. Not runtime. */
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <math.h>
#include <opus.h>
#include <ogg/ogg.h>

static FILE *out;
static void flush(ogg_stream_state *stream, int force) {
  ogg_page page;
  while (force ? ogg_stream_flush(stream, &page) : ogg_stream_pageout(stream, &page)) {
    if (fwrite(page.header, 1, page.header_len, out) != (size_t)page.header_len ||
        fwrite(page.body, 1, page.body_len, out) != (size_t)page.body_len) exit(2);
  }
}
static void put16(unsigned char *b, unsigned n) { b[0]=n; b[1]=n>>8; }
static void put32(unsigned char *b, unsigned n) {
  for (int i=0; i<4; ++i) b[i] = n >> (8*i);
}
int main(int argc, char **argv) {
  if (argc != 4) return 2;
  int channels = atoi(argv[1]), samples = atoi(argv[2]);
  if ((channels != 1 && channels != 2) || samples <= 0 || samples > 48000*31) return 2;
  out = fopen(argv[3], "wb");
  if (!out) return 2;
  int error, lookahead;
  OpusEncoder *encoder = opus_encoder_create(48000, channels, OPUS_APPLICATION_AUDIO, &error);
  if (!encoder || error) return 2;
  if (opus_encoder_ctl(encoder, OPUS_SET_BITRATE(24000)) ||
      opus_encoder_ctl(encoder, OPUS_SET_COMPLEXITY(4)) ||
      opus_encoder_ctl(encoder, OPUS_GET_LOOKAHEAD(&lookahead))) return 2;
  ogg_stream_state stream;
  if (ogg_stream_init(&stream, 0x56413031 + channels)) return 2;
  unsigned char head[19] = "OpusHead";
  head[8]=1; head[9]=channels; put16(head+10, lookahead); put32(head+12, 48000);
  unsigned char tags[64] = "OpusTags";
  const char vendor[] = "voiceaudio-synthetic";
  put32(tags+8, sizeof(vendor)-1);
  memcpy(tags+12, vendor, sizeof(vendor)-1);
  put32(tags+12+sizeof(vendor)-1, 0);
  ogg_packet packet = {0};
  packet.packet=head; packet.bytes=19; packet.b_o_s=1;
  if (ogg_stream_packetin(&stream, &packet)) return 2;
  flush(&stream, 1);
  packet.packet=tags; packet.bytes=12+sizeof(vendor)-1+4;
  packet.b_o_s=0; packet.packetno=1;
  if (ogg_stream_packetin(&stream, &packet)) return 2;
  flush(&stream, 1);
  int16_t pcm[960*2];
  unsigned char encoded[4000];
  int64_t total=0;
  while (total < samples+lookahead) {
    for (int i=0; i<960; ++i) {
      for (int c=0; c<channels; ++c) {
        int64_t t=total+i;
        pcm[i*channels+c] = t < samples ?
          (int16_t)(6000*sin(2*3.141592653589793*(440+220*c)*t/48000.0)) : 0;
      }
    }
    int n = opus_encode(encoder, pcm, 960, encoded, sizeof(encoded));
    if (n <= 0) return 2;
    total+=960;
    packet.packet=encoded; packet.bytes=n; ++packet.packetno;
    packet.e_o_s=total >= samples+lookahead;
    packet.granulepos=packet.e_o_s ? samples+lookahead : total;
    if (ogg_stream_packetin(&stream, &packet)) return 2;
    flush(&stream, packet.e_o_s);
  }
  ogg_stream_clear(&stream);
  opus_encoder_destroy(encoder);
  return fclose(out) ? 2 : 0;
}
