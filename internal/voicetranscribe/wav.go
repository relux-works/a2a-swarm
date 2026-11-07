package voicetranscribe

import (
	"bytes"
	"encoding/binary"
)

// Admit only the canonical 44-byte PCM header followed by one complete data
// chunk. No metadata, alternate chunks, extensions, padding or trailing bytes.
// A future decoder must emit this representation, never just change the label.
func validateWAV(b []byte) (int, Code) {
	if len(b) < 44 || !bytes.Equal(b[:4], []byte("RIFF")) ||
		!bytes.Equal(b[8:16], []byte("WAVEfmt ")) ||
		binary.LittleEndian.Uint32(b[16:20]) != 16 ||
		!bytes.Equal(b[36:40], []byte("data")) ||
		uint64(binary.LittleEndian.Uint32(b[4:8]))+8 != uint64(len(b)) ||
		uint64(binary.LittleEndian.Uint32(b[40:44]))+44 != uint64(len(b)) {
		return 0, CorruptAudio
	}
	if binary.LittleEndian.Uint16(b[20:22]) != 1 ||
		binary.LittleEndian.Uint16(b[22:24]) != Channels ||
		binary.LittleEndian.Uint32(b[24:28]) != SampleRate ||
		binary.LittleEndian.Uint32(b[28:32]) != SampleRate*2 ||
		binary.LittleEndian.Uint16(b[32:34]) != 2 ||
		binary.LittleEndian.Uint16(b[34:36]) != BitsPerSample {
		return 0, UnsupportedCodec
	}
	n := len(b) - 44
	if n == 0 || n%2 != 0 {
		return 0, CorruptAudio
	}
	if n > MaxPCMBytes {
		return 0, Oversize
	}
	return n, ""
}
