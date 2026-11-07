package voiceaudio

import (
	"bytes"
	"context"
	"encoding/binary"
)

// A deliberately strict RFC 3533/7845 subset: one uncropped logical stream,
// mapping family 0, no holes/recapture/chaining/multiplexing/trailing bytes.
// The native library independently validates and decodes every audio packet.
type oggInfo struct {
	samples48 int64
	channels  int
}

var oggCRCTable = func() (table [256]uint32) {
	for i := range table {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		table[i] = r
	}
	return
}()

func pageCRC(page []byte) uint32 {
	var sum uint32
	for i, b := range page {
		if i >= 22 && i < 26 {
			b = 0
		}
		sum = sum<<8 ^ oggCRCTable[byte(sum>>24)^b]
	}
	return sum
}

func inspectOgg(ctx context.Context, data []byte) (oggInfo, Reason) {
	bad := func(r Reason) (oggInfo, Reason) { return oggInfo{}, r }
	if len(data) < 4 || !bytes.Equal(data[:4], []byte("OggS")) {
		return bad(Unsupported)
	}
	var serial, seq uint32
	var packets int
	var pending []byte
	var total, previousGP, lastDuration int64
	var preskip int64
	var info oggInfo
	eos := false
	for off := 0; off < len(data); {
		if r := contextReason(ctx); r != "" {
			return bad(r)
		}
		if eos {
			return bad(Ambiguous)
		}
		if len(data)-off < 27 {
			return bad(Malformed)
		}
		h := data[off:]
		if !bytes.Equal(h[:4], []byte("OggS")) || h[4] != 0 || h[5]&^byte(7) != 0 {
			return bad(Malformed)
		}
		nseg := int(h[26])
		if nseg == 0 || len(h) < 27+nseg {
			return bad(Malformed)
		}
		size := 27 + nseg
		for _, n := range h[27 : 27+nseg] {
			size += int(n)
		}
		if size > len(h) {
			return bad(Malformed)
		}
		page := h[:size]
		if pageCRC(page) != binary.LittleEndian.Uint32(h[22:]) {
			return bad(Malformed)
		}
		flags := h[5]
		gp := int64(binary.LittleEndian.Uint64(h[6:]))
		if off == 0 {
			serial = binary.LittleEndian.Uint32(h[14:])
			if flags != 2 || binary.LittleEndian.Uint32(h[18:]) != 0 || gp != 0 {
				return bad(Malformed)
			}
		} else if binary.LittleEndian.Uint32(h[14:]) != serial || flags&2 != 0 {
			return bad(Ambiguous)
		}
		if binary.LittleEndian.Uint32(h[18:]) != seq {
			return bad(Malformed)
		}
		seq++
		if (flags&1 != 0) != (len(pending) != 0) {
			return bad(Malformed)
		}
		body := 27 + nseg
		completed := 0
		pageStartPackets := packets
		for i, lace := range h[27 : 27+nseg] {
			pending = append(pending, h[body:body+int(lace)]...)
			body += int(lace)
			if len(pending) > 65536 {
				return bad(Malformed)
			}
			if lace == 255 {
				continue
			}
			completed++
			switch packets {
			case 0:
				if !bytes.HasPrefix(pending, []byte("OpusHead")) {
					return bad(Unsupported)
				}
				if len(pending) != 19 {
					return bad(Unsupported)
				}
				if pending[8] != 1 || pending[18] != 0 || (pending[9] != 1 && pending[9] != 2) {
					return bad(Unsupported)
				}
				if off != 0 || nseg != 1 || i != 0 {
					return bad(Malformed)
				}
				info.channels = int(pending[9])
				preskip = int64(binary.LittleEndian.Uint16(pending[10:]))
			case 1:
				if !validTags(pending) || i != nseg-1 {
					return bad(Malformed)
				}
			default:
				duration := packetSamples48(pending)
				if duration <= 0 {
					return bad(Malformed)
				}
				lastDuration = int64(duration)
				total += lastDuration
			}
			packets++
			pending = pending[:0]
		}
		if packets <= 2 {
			if flags&4 != 0 || (completed == 0 && gp != -1) || (completed > 0 && gp != 0) {
				return bad(Malformed)
			}
		} else {
			if pageStartPackets < 2 {
				return bad(Malformed)
			}
			if completed == 0 {
				if gp != -1 || flags&4 != 0 {
					return bad(Malformed)
				}
			} else {
				if gp < previousGP {
					return bad(Malformed)
				}
				if flags&4 != 0 {
					if len(pending) != 0 || gp <= preskip || gp > total || gp < total-lastDuration {
						return bad(Malformed)
					}
					info.samples48 = gp - preskip
					eos = true
				} else if gp != total {
					return bad(Malformed)
				}
				previousGP = gp
			}
		}
		off += size
	}
	if !eos || packets < 3 || len(pending) != 0 || info.samples48 < 3 {
		return bad(Malformed)
	}
	return info, ""
}

func validTags(p []byte) bool {
	if len(p) < 16 || !bytes.Equal(p[:8], []byte("OpusTags")) {
		return false
	}
	vendor := uint64(binary.LittleEndian.Uint32(p[8:]))
	if vendor > uint64(len(p)-16) {
		return false
	}
	off := 12 + int(vendor)
	count := uint64(binary.LittleEndian.Uint32(p[off:]))
	off += 4
	if count > uint64((len(p)-off)/4) {
		return false
	}
	for ; count > 0; count-- {
		if len(p)-off < 4 {
			return false
		}
		n := uint64(binary.LittleEndian.Uint32(p[off:]))
		off += 4
		if n > uint64(len(p)-off) {
			return false
		}
		off += int(n)
	}
	return true // RFC 7845 permits trailing padding; never interpreted as paths.
}

// TOC timing only (RFC 6716 §3.1); not a substitute for libopus decoding.
func packetSamples48(p []byte) int {
	if len(p) == 0 {
		return 0
	}
	toc := int(p[0])
	frames := 0
	switch toc & 3 {
	case 0:
		frames = 1
	case 1, 2:
		frames = 2
	case 3:
		if len(p) < 2 {
			return 0
		}
		frames = int(p[1] & 63)
	}
	var size int
	if toc&128 != 0 {
		size = 120 << ((toc >> 3) & 3)
	} else if toc&96 == 96 {
		if toc&8 != 0 {
			size = 960
		} else {
			size = 480
		}
	} else {
		size = 480 << ((toc >> 3) & 3)
		if size > 2880 {
			size = 2880
		}
	}
	n := frames * size
	if frames == 0 || n > 5760 {
		return 0
	}
	return n
}
