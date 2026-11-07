package voiceaudio

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".ogg"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func privateTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}
func nativeProfile(t *testing.T) Profile {
	t.Helper()
	if os.Getenv("A2A_VOICEAUDIO_NATIVE") != "1" {
		t.Skip("native witness requires explicit hash-bound offline provisioning; no Opus qualification claimed")
	}
	p := Profile{ProfileID, os.Getenv("A2A_VOICEAUDIO_EXECUTABLE"), os.Getenv("A2A_VOICEAUDIO_SHA256"), "linux/amd64"}
	if p.Executable == "" || p.SHA256 == "" {
		t.Fatal("opted-in native fixture lacks executable/hash")
	}
	return p
}
func request(t *testing.T) Request {
	t.Helper()
	return Request{Bytes: fixture(t, "mono-1s"), TMPDIR: privateTemp(t), Profile: nativeProfile(t)}
}
func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("scratch not empty: entries=%v err=%v", entries, err)
	}
}
func refuse(t *testing.T, r Request, reason Reason) {
	t.Helper()
	audio, err := Decode(context.Background(), r)
	if err == nil || err.Reason != reason || audio.WAV != nil || audio.Samples != 0 || audio.Duration != 0 {
		t.Fatalf("wanted closed refusal %s and zero audio; got WAV-bytes=%d samples=%d duration=%s refusal=%v", reason, len(audio.WAV), audio.Samples, audio.Duration, err)
	}
}
func neighbor(t *testing.T, p Profile) {
	t.Helper()
	tmp := privateTemp(t)
	a, e := Decode(context.Background(), Request{Bytes: fixture(t, "mono-1s"), TMPDIR: tmp, Profile: p})
	if e != nil || a.Samples != 16000 {
		t.Fatalf("valid neighbor failed: %v samples=%d", e, a.Samples)
	}
	assertEmpty(t, tmp)
}

func assertWAV(t *testing.T, a Audio, samples int) {
	t.Helper()
	b := a.WAV
	if len(b) != 44+samples*2 || a.Samples != samples || a.Duration != time.Duration(samples)*time.Second/16000 {
		t.Fatalf("wrong length/count/duration: %d/%d/%s", len(b), a.Samples, a.Duration)
	}
	if string(b[:4]) != "RIFF" || string(b[8:16]) != "WAVEfmt " || string(b[36:40]) != "data" {
		t.Fatal("noncanonical chunk layout")
	}
	for _, field := range []struct {
		off  int
		want uint32
	}{{4, uint32(36 + 2*samples)}, {16, 16}, {24, 16000}, {28, 32000}, {40, uint32(2 * samples)}} {
		if got := binary.LittleEndian.Uint32(b[field.off:]); got != field.want {
			t.Fatalf("WAV uint32 at %d: %d != %d", field.off, got, field.want)
		}
	}
	for _, field := range []struct {
		off  int
		want uint16
	}{{20, 1}, {22, 1}, {32, 2}, {34, 16}} {
		if got := binary.LittleEndian.Uint16(b[field.off:]); got != field.want {
			t.Fatalf("WAV uint16 at %d: %d != %d", field.off, got, field.want)
		}
	}
}

func TestNativeSyntheticWAV(t *testing.T) {
	p := nativeProfile(t)
	for _, tc := range []struct {
		name    string
		samples int
		tones   []float64
	}{{"mono-1s", 16000, []float64{440}}, {"stereo-1s", 16000, []float64{440, 660}}, {"mono-30s", 480000, []float64{440}}} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := privateTemp(t)
			a, e := Decode(context.Background(), Request{Bytes: fixture(t, tc.name), TMPDIR: tmp, Profile: p})
			if e != nil {
				t.Fatal(e)
			}
			assertWAV(t, a, tc.samples)
			assertEmpty(t, tmp)
			// Explicit offline test-artifact export, never part of Decode.
			if dir := os.Getenv("A2A_VOICEAUDIO_WAV_EVIDENCE"); dir != "" {
				if !filepath.IsAbs(dir) {
					t.Fatal("evidence directory must be explicit absolute")
				}
				if err := os.WriteFile(filepath.Join(dir, tc.name+".wav"), a.WAV, 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Known synthetic source frequencies independently check rate and
			// stereo downmix. Shape/count tests alone would miss a wrong rate.
			for _, hz := range tc.tones {
				var re, im float64
				for i := 800; i < min(tc.samples, 15200); i++ {
					x := float64(int16(binary.LittleEndian.Uint16(a.WAV[44+2*i:])))
					a := 2 * math.Pi * hz * float64(i) / 16000
					re += x * math.Cos(a)
					im += x * math.Sin(a)
				}
				if math.Hypot(re, im)/14400 < 900 {
					t.Fatalf("missing synthetic %.0f Hz tone at 16 kHz", hz)
				}
			}
		})
	}
}

func TestRefusalInputAndDurationBounds(t *testing.T) {
	p := nativeProfile(t)
	for _, tc := range []struct {
		name      string
		data      []byte
		encrypted bool
		reason    Reason
	}{
		{"oversize", make([]byte, MaxInputBytes+1), false, InputLimit},
		{"encrypted", fixture(t, "mono-1s"), true, Encrypted},
		{"one-sample-over", fixture(t, "mono-over-30s"), false, DurationLimit},
		{"31-seconds", fixture(t, "mono-31s"), false, DurationLimit},
		{"unsupported-wave", []byte("RIFFxxxxWAVE"), false, Unsupported},
		{"playlist", []byte("#EXTM3U\nfile:///host/private\nhttps://example.invalid/audio"), false, Unsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := privateTemp(t)
			refuse(t, Request{tc.data, tc.encrypted, tmp, p}, tc.reason)
			assertEmpty(t, tmp)
			neighbor(t, p)
		})
	}
}

func TestMalformedAndAmbiguousNeighbors(t *testing.T) {
	p := nativeProfile(t)
	valid := fixture(t, "mono-1s")
	corrupt := bytes.Clone(valid)
	corrupt[len(corrupt)-5] ^= 128
	badSeq := bytes.Clone(valid)
	first := pageSize(badSeq)
	binary.LittleEndian.PutUint32(badSeq[first+18:], 9)
	fixCRC(badSeq[first : first+pageSize(badSeq[first:])])
	multi := bytes.Clone(valid)
	binary.LittleEndian.PutUint32(multi[first+14:], 123)
	fixCRC(multi[first : first+pageSize(multi[first:])])
	missingEOS := bytes.Clone(valid)
	last := lastPage(missingEOS)
	missingEOS[last+5] &^= 4
	fixCRC(missingEOS[last:])
	for _, tc := range []struct {
		name   string
		data   []byte
		reason Reason
	}{
		{"crc", corrupt, Malformed}, {"sequence", badSeq, Malformed}, {"truncated", valid[:len(valid)-1], Malformed},
		{"missing-eos", missingEOS, Malformed}, {"chained", append(bytes.Clone(valid), valid...), Ambiguous},
		{"multiplexed", multi, Ambiguous}, {"trailing", append(bytes.Clone(valid), 1), Ambiguous},
		{"ogg-vorbis", oggPage([]byte("\x01vorbis"), 2, 0, 0), Unsupported},
		{"malformed-native-packet", badNativePacket(t), Malformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := privateTemp(t)
			refuse(t, Request{Bytes: tc.data, TMPDIR: tmp, Profile: p}, tc.reason)
			assertEmpty(t, tmp)
			neighbor(t, p)
		})
	}
}

func TestInformationalRateIsNotAuthority(t *testing.T) {
	p := nativeProfile(t)
	for _, rate := range []uint32{0, 8000, 12345, 192000} {
		data := fixture(t, "mono-1s")
		binary.LittleEndian.PutUint32(data[28+12:], rate)
		fixCRC(data[:pageSize(data)])
		tmp := privateTemp(t)
		a, e := Decode(context.Background(), Request{Bytes: data, TMPDIR: tmp, Profile: p})
		if e != nil {
			t.Fatal(rate, e)
		}
		assertWAV(t, a, 16000)
		assertEmpty(t, tmp)
	}
}

func TestUnsupportedOpusHeadsKeepValidNeighbor(t *testing.T) {
	p := nativeProfile(t)
	for _, tc := range []struct {
		name   string
		offset int
		value  byte
	}{{"version", 8, 2}, {"zero-channels", 9, 0}, {"multichannel", 9, 3}, {"mapping-family", 18, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			data := fixture(t, "mono-1s")
			data[28+tc.offset] = tc.value
			fixCRC(data[:pageSize(data)])
			tmp := privateTemp(t)
			refuse(t, Request{Bytes: data, TMPDIR: tmp, Profile: p}, Unsupported)
			assertEmpty(t, tmp)
			neighbor(t, p)
		})
	}
}

func TestOggContinuedTagsValidNeighbor(t *testing.T) {
	// A comment spans two pages; this tests actual continuation framing.
	p := nativeProfile(t)
	valid := fixture(t, "mono-1s")
	head := bytes.Clone(valid[:pageSize(valid)])
	tags := make([]byte, 316)
	copy(tags, "OpusTags")
	binary.LittleEndian.PutUint32(tags[8:], 300)
	part := make([]byte, 28+255)
	copy(part, "OggS")
	part[26] = 1
	part[27] = 255
	binary.LittleEndian.PutUint64(part[6:], ^uint64(0))
	copy(part[14:18], head[14:18])
	binary.LittleEndian.PutUint32(part[18:], 1)
	copy(part[28:], tags[:255])
	fixCRC(part)
	end := oggPage(tags[255:], 1, 0, 2)
	copy(end[14:18], head[14:18])
	fixCRC(end)
	data := append(append(head, part...), end...)
	off := pageSize(valid)
	off += pageSize(valid[off:])
	for off < len(valid) {
		page := bytes.Clone(valid[off : off+pageSize(valid[off:])])
		binary.LittleEndian.PutUint32(page[18:], binary.LittleEndian.Uint32(page[18:])+1)
		fixCRC(page)
		data = append(data, page...)
		off += len(page)
	}
	tmp := privateTemp(t)
	a, e := Decode(context.Background(), Request{Bytes: data, TMPDIR: tmp, Profile: p})
	if e != nil {
		t.Fatal(e)
	}
	assertWAV(t, a, 16000)
	assertEmpty(t, tmp)
}

func TestPurePreflightBoundaries(t *testing.T) {
	data := fixture(t, "mono-1s")
	info, r := inspectOgg(context.Background(), data)
	if r != "" || info.samples48 != 48000 || info.channels != 1 {
		t.Fatalf("preflight %+v %s", info, r)
	}
	bad := badNativePacket(t)
	if _, r := inspectOgg(context.Background(), bad); r != "" {
		t.Fatalf("native negative must reach decoder, got %s", r)
	}
	for _, p := range [][]byte{nil, {3}, {3, 0}, {3, 63}} {
		if packetSamples48(p) != 0 {
			t.Fatalf("bad TOC accepted %x", p)
		}
	}
	if validTags([]byte("OpusTags")) {
		t.Fatal("short tags")
	}
	tags := make([]byte, 16)
	copy(tags, "OpusTags")
	binary.LittleEndian.PutUint32(tags[8:], ^uint32(0))
	if validTags(tags) {
		t.Fatal("tag integer overflow")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, r = inspectOgg(ctx, data); r != Cancelled {
		t.Fatal(r)
	}
}

func TestClosedErrors(t *testing.T) {
	r := Request{Bytes: fixture(t, "mono-1s"), TMPDIR: privateTemp(t)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a, e := Decode(ctx, r)
	if e == nil || e.Reason != Cancelled || a.WAV != nil || a.Samples != 0 || a.Duration != 0 {
		t.Fatal(a, e)
	}
	assertEmpty(t, r.TMPDIR)
	if strings.Contains((&Refusal{DecodeFailed}).Error(), "/") {
		t.Fatal("unexpected path in refusal")
	}
}

func pageSize(data []byte) int {
	n := 27 + int(data[26])
	for _, l := range data[27:n] {
		n += int(l)
	}
	return n
}
func lastPage(data []byte) int {
	off := 0
	for off+pageSize(data[off:]) < len(data) {
		off += pageSize(data[off:])
	}
	return off
}
func fixCRC(page []byte) { binary.LittleEndian.PutUint32(page[22:], pageCRC(page)) }
func oggPage(packet []byte, flags byte, gp int64, seq uint32) []byte {
	nseg := len(packet)/255 + 1
	b := make([]byte, 27+nseg+len(packet))
	copy(b, "OggS")
	b[5] = flags
	binary.LittleEndian.PutUint64(b[6:], uint64(gp))
	binary.LittleEndian.PutUint32(b[14:], 0x56413032)
	binary.LittleEndian.PutUint32(b[18:], seq)
	b[26] = byte(nseg)
	for i := 0; i < nseg-1; i++ {
		b[27+i] = 255
	}
	b[27+nseg-1] = byte(len(packet) % 255)
	copy(b[27+nseg:], packet)
	fixCRC(b)
	return b
}
func badNativePacket(t *testing.T) []byte {
	valid := fixture(t, "mono-1s")
	head := bytes.Clone(valid[28:47])
	binary.LittleEndian.PutUint16(head[10:], 0)
	tags := make([]byte, 16)
	copy(tags, "OpusTags")
	// Two CBR frames but an odd payload length: the TOC duration is valid;
	// libopus must reject packet framing, with no partial-output success.
	return append(append(oggPage(head, 2, 0, 0), oggPage(tags, 0, 0, 1)...), oggPage([]byte{0x81, 0}, 4, 240, 2)...)
}
