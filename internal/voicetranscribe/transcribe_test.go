package voicetranscribe

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fixtureExecutable string

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "stt-test-engine-")
	if err != nil {
		return 80
	}
	defer os.RemoveAll(dir)
	fixtureExecutable = filepath.Join(dir, "engine")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-p=2", "-trimpath", "-buildvcs=false", "-o", fixtureExecutable, "./testdata/engine")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if cmd.Run() != nil {
		return 81
	}
	return m.Run()
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal("private fixture operation failed")
	}
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func wav(pcm []byte) []byte {
	b := make([]byte, 44+len(pcm))
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 16000)
	binary.LittleEndian.PutUint32(b[28:], 32000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(len(pcm)))
	copy(b[44:], pcm)
	return b
}

func request(mode uint16) Request {
	pcm := make([]byte, 66)
	binary.LittleEndian.PutUint16(pcm, mode)
	return Request{Original: EventRef{EventID: "$opaque\n\x00event", FetchHandle: "!room/$event?x=\"opaque\""}, Audio: wav(pcm), Codec: CodecPCM16WAV}
}

// privateTempDir meets the adapter's explicit physical-path and 0700 contract.
// testing.T.TempDir uses the process umask for its leaf, and the system temp
// directory can have symlink ancestors (for example on macOS).
func privateTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	must(t, os.Chmod(dir, 0700))
	return dir
}

func fixture(t *testing.T) (*Adapter, string) {
	t.Helper()
	source := privateTempDir(t)
	tmp := privateTempDir(t)
	for _, dir := range []string{"bin", "lib", "model"} {
		must(t, os.Mkdir(filepath.Join(source, dir), 0700))
	}
	a := New(PinnedProfile(source), tmp)
	exe, err := os.ReadFile(fixtureExecutable)
	must(t, err)
	files := map[string][]byte{"bin/sherpa-onnx-offline": exe, "lib/libonnxruntime.so": {1, 2, 3},
		"model/tiny-encoder.int8.onnx": {4, 5}, "model/tiny-decoder.int8.onnx": {6, 7}, "model/tiny-tokens.txt": {8, 9}}
	a.native, a.model = nil, nil
	for _, name := range []string{"model/tiny-encoder.int8.onnx", "model/tiny-decoder.int8.onnx", "model/tiny-tokens.txt", "bin/sherpa-onnx-offline", "lib/libonnxruntime.so"} {
		data := files[name]
		must(t, os.WriteFile(filepath.Join(source, name), data, 0400))
		pin := Artifact{name, int64(len(data)), digest(data)}
		if strings.HasPrefix(name, "model/") {
			a.model = append(a.model, pin)
		} else {
			a.native = append(a.native, pin)
		}
	}
	return a, tmp
}

func emptyScratch(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	must(t, err)
	if len(entries) != 0 {
		t.Fatal("operation scratch survived return")
	}
}

func assertRefusal(t *testing.T, out Outcome, req Request, code Code, stage Stage) {
	t.Helper()
	if out.Original != req.Original || out.Transcript != nil || out.Refusal == nil || out.Refusal.Code != code || out.Refusal.Stage != stage {
		t.Errorf("expected typed refusal %s/%s preserving exact event reference", stage, code)
	}
}

func assertSuccess(t *testing.T, out Outcome, req Request) {
	t.Helper()
	if out.Original != req.Original || out.Refusal != nil || out.Transcript == nil {
		t.Fatal("valid neighbor failed or lost exact event reference")
	}
	v := out.Transcript
	if v.Text == "" || !v.Untrusted || v.Confidence != nil || v.Model != ModelID || v.Engine != EngineID || v.ConfidenceReason == "" {
		t.Fatal("untrusted transcript provenance/confidence contract changed")
	}
	b, err := json.Marshal(out)
	must(t, err)
	if !bytes.Contains(b, []byte(`"confidence":null`)) {
		t.Fatal("unknown confidence omitted or fabricated")
	}
}

func TestInputRefusalsAndValidNeighbors(t *testing.T) {
	a, tmp := fixture(t)
	for _, name := range []string{"encrypted", "input-cap", "duration", "corrupt", "truncated", "trailing", "format", "codec", "empty", "odd"} {
		t.Run(name, func(t *testing.T) {
			r := request(0)
			want := CorruptAudio
			switch name {
			case "encrypted":
				r.Encrypted = true
				want = EncryptedMedia
			case "input-cap":
				r.Audio = make([]byte, MaxInputBytes+1)
				want = Oversize
			case "duration":
				r.Audio = wav(make([]byte, MaxPCMBytes+2))
				want = Oversize
			case "corrupt":
				r.Audio[0] = 0
			case "truncated":
				r.Audio = r.Audio[:len(r.Audio)-1]
			case "trailing":
				r.Audio = append(r.Audio, 0)
			case "format":
				r.Audio[22] = 2
				want = UnsupportedCodec
			case "codec":
				r.Codec = "ogg-opus"
				want = UnsupportedCodec
			case "empty":
				r.Audio = wav(nil)
			case "odd":
				r.Audio = wav([]byte{1})
			}
			assertRefusal(t, a.Transcribe(context.Background(), r), r, want, InputStage)
			emptyScratch(t, tmp)
			r = request(0)
			assertSuccess(t, a.Transcribe(context.Background(), r), r)
			emptyScratch(t, tmp)
		})
	}
}

func TestWAVBoundary(t *testing.T) {
	for _, n := range []int{2, MaxPCMBytes, MaxPCMBytes + 2} {
		count, code := validateWAV(wav(make([]byte, n)))
		if n > MaxPCMBytes {
			if code != Oversize {
				t.Fatal("30 second measured PCM cap failed")
			}
		} else if code != "" || count != n {
			t.Fatal("valid bounded PCM neighbor refused")
		}
	}
	for _, offset := range []int{4, 16, 20, 22, 24, 28, 32, 34, 36, 40} {
		b := request(0).Audio
		b[offset] ^= 0x7f
		if _, code := validateWAV(b); code == "" {
			t.Fatal("invalid WAV structure/format admitted")
		}
	}
}

func TestModelBackendRefusalsAndRestore(t *testing.T) {
	for _, name := range []string{"missing-model", "changed-model", "model-link", "missing-engine", "changed-engine", "missing-library", "profile-pin", "profile-platform"} {
		t.Run(name, func(t *testing.T) {
			a, tmp := fixture(t)
			path := filepath.Join(a.profile.Directory, "model/tiny-encoder.int8.onnx")
			code, stage := ModelIntegrity, ModelStage
			switch name {
			case "missing-model":
				code = MissingModel
			case "missing-engine", "changed-engine":
				path = filepath.Join(a.profile.Directory, a.native[0].Path)
				stage = BackendStage
				code = BackendIntegrity
				if name == "missing-engine" {
					code = BackendUnavailable
				}
			case "missing-library":
				path = filepath.Join(a.profile.Directory, a.native[1].Path)
				stage = BackendStage
				code = BackendUnavailable
			case "profile-pin", "profile-platform":
				p := PinnedProfile(a.profile.Directory)
				if name == "profile-pin" {
					p.Executable.SHA256 = strings.Repeat("0", 64)
				} else {
					p.GOARCH = "unexpected"
				}
				bad := New(p, tmp)
				r := request(0)
				assertRefusal(t, bad.Transcribe(context.Background(), r), r, BackendUnavailable, BackendStage)
				assertSuccess(t, a.Transcribe(context.Background(), r), r)
				emptyScratch(t, tmp)
				return
			}
			old, err := os.ReadFile(path)
			must(t, err)
			must(t, os.Rename(path, path+".held"))
			if name == "model-link" {
				must(t, os.Symlink(path+".held", path))
			} else if strings.HasPrefix(name, "changed-") {
				changed := append([]byte(nil), old...)
				changed[len(changed)-1] ^= 1
				must(t, os.WriteFile(path, changed, 0400))
			}
			r := request(0)
			assertRefusal(t, a.Transcribe(context.Background(), r), r, code, stage)
			if _, err := os.Lstat(path); err == nil {
				must(t, os.Remove(path))
			}
			must(t, os.Rename(path+".held", path))
			restored, err := os.ReadFile(path)
			must(t, err)
			if !bytes.Equal(restored, old) {
				t.Fatal("fixture source restoration not byte exact")
			}
			assertSuccess(t, a.Transcribe(context.Background(), r), r)
			emptyScratch(t, tmp)
		})
	}
}

func TestEngineOutputRefusalsAndPrivacy(t *testing.T) {
	a, tmp := fixture(t)
	for _, tc := range []struct {
		mode  uint16
		code  Code
		stage Stage
	}{
		{1, EngineFailed, InferenceStage}, {2, OutputLimit, OutputStage}, {3, OutputLimit, OutputStage},
		{4, InvalidOutput, OutputStage}, {5, InvalidOutput, OutputStage}, {9, InvalidOutput, OutputStage}, {10, InvalidOutput, OutputStage},
	} {
		t.Run(string(rune('A'+tc.mode)), func(t *testing.T) {
			r := request(tc.mode)
			_, err := rand.Read(r.Audio[46:])
			must(t, err)
			sentinel := hex.EncodeToString(r.Audio[46:])
			out := a.Transcribe(context.Background(), r)
			assertRefusal(t, out, r, tc.code, tc.stage)
			data, err := json.Marshal(out)
			must(t, err)
			if bytes.Contains(data, []byte(sentinel)) || bytes.Contains(data, []byte(a.profile.Directory)) {
				t.Fatal("private diagnostics escaped into outcome")
			}
			emptyScratch(t, tmp)
			r = request(0)
			assertSuccess(t, a.Transcribe(context.Background(), r), r)
			emptyScratch(t, tmp)
		})
	}
	r := request(6)
	out := a.Transcribe(context.Background(), r)
	assertSuccess(t, out, r)
	if out.Transcript.Language != "unknown" {
		t.Fatal("unrecognized language was trusted")
	}
}

func TestOutputStrictness(t *testing.T) {
	for _, b := range [][]byte{
		[]byte(`{"text":"ok","Text":"other"}`), []byte(`{"text":null}`), []byte(`{"text":"ok","lang":7}`),
		[]byte(`{"text":"ok","lang":"en"} []`), []byte(`{"text":"\ud800"}`), {0xff}, []byte(`[]`), []byte(`{"lang":"ru"}`),
	} {
		if _, _, code := parseOutput(b); code != InvalidOutput {
			t.Fatal("ambiguous/malformed output admitted")
		}
	}
	text, language, code := parseOutput([]byte(`{"text":"ok","lang":"ru","ys_log_probs":[0.9]}`))
	if code != "" || text != "ok" || language != "ru" {
		t.Fatal("valid output neighbor refused")
	}
}

func TestPrivateTemporaryRoot(t *testing.T) {
	a, tmp := fixture(t)
	for _, mode := range []os.FileMode{0755, 0777, 0710, os.ModeSticky | 0700} {
		must(t, os.Chmod(tmp, mode))
		r := request(0)
		assertRefusal(t, a.Transcribe(context.Background(), r), r, TemporaryRoot, ScratchStage)
		must(t, os.Chmod(tmp, 0700))
		assertSuccess(t, a.Transcribe(context.Background(), r), r)
		emptyScratch(t, tmp)
	}
	for _, path := range []string{"", ".", tmp + "/../" + filepath.Base(tmp), tmp + "/", filepath.Join(t.TempDir(), "missing")} {
		a.tempDir = path
		r := request(0)
		assertRefusal(t, a.Transcribe(context.Background(), r), r, TemporaryRoot, ScratchStage)
	}
	link := filepath.Join(t.TempDir(), "link")
	must(t, os.Symlink(tmp, link))
	a.tempDir = link
	r := request(0)
	assertRefusal(t, a.Transcribe(context.Background(), r), r, TemporaryRoot, ScratchStage)
	a.tempDir = tmp
	assertSuccess(t, a.Transcribe(context.Background(), r), r)
	emptyScratch(t, tmp)
}

func TestAlreadyCancelled(t *testing.T) {
	a, tmp := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := request(0)
	assertRefusal(t, a.Transcribe(ctx, r), r, Cancelled, InputStage)
	emptyScratch(t, tmp)
	assertSuccess(t, a.Transcribe(context.Background(), r), r)
	emptyScratch(t, tmp)
}

// No synthetic executable is used here. Explicit public artifact preparation
// is a separate opt-in; absence is a recorded skip, never an ASR pass.
func realFixture(t *testing.T) (*Adapter, string, map[string][]byte) {
	t.Helper()
	profile, fixtures := os.Getenv("A2A_STT_IT_PROFILE"), os.Getenv("A2A_STT_IT_FIXTURES")
	if profile == "" || fixtures == "" {
		t.Skip("real offline ASR requires explicit local profile and public fixtures")
	}
	if !filepath.IsAbs(profile) || !filepath.IsAbs(fixtures) {
		t.Fatal("real ASR requires explicit absolute fixture paths")
	}
	tmp := privateTempDir(t)
	a := New(PinnedProfile(profile), tmp)
	wavs := make(map[string][]byte)
	for lang, name := range map[string]string{"en": "en-english.wav", "ru": "ru-russian.wav"} {
		b, err := os.ReadFile(filepath.Join(fixtures, name))
		must(t, err)
		wavs[lang] = b
	}
	return a, tmp, wavs
}

func realSuccess(t *testing.T, a *Adapter, audio []byte, language string) {
	t.Helper()
	r := request(0)
	r.Audio = audio
	out := a.Transcribe(context.Background(), r)
	assertSuccess(t, out, r)
	if out.Transcript.Language != language {
		t.Fatal("real ASR language control did not match")
	}
	if language == "ru" && !strings.ContainsFunc(out.Transcript.Text, func(r rune) bool { return r >= 0x400 && r <= 0x4ff }) {
		t.Fatal("real Russian control contains no Cyrillic")
	}
}

func TestRealOfflineLanguagesAndRefusals(t *testing.T) {
	a, tmp, wavs := realFixture(t)
	for _, lang := range []string{"en", "ru"} {
		t.Run(lang, func(t *testing.T) { realSuccess(t, a, wavs[lang], lang); emptyScratch(t, tmp) })
	}
	for _, name := range []string{"corrupt", "oversize", "duration", "unsupported", "encrypted", "missing-model"} {
		t.Run(name, func(t *testing.T) {
			r := request(0)
			r.Audio = append([]byte(nil), wavs["en"]...)
			code, stage := CorruptAudio, InputStage
			switch name {
			case "corrupt":
				r.Audio[0] = 0
			case "oversize":
				r.Audio = make([]byte, MaxInputBytes+1)
				code = Oversize
			case "duration":
				r.Audio = wav(make([]byte, MaxPCMBytes+2))
				code = Oversize
			case "unsupported":
				r.Codec = "ogg-opus"
				code = UnsupportedCodec
			case "encrypted":
				r.Encrypted = true
				code = EncryptedMedia
			case "missing-model":
				path := filepath.Join(a.profile.Directory, "model/tiny-encoder.int8.onnx")
				before, err := os.ReadFile(path)
				must(t, err)
				must(t, os.Rename(path, path+".withheld"))
				defer func() {
					must(t, os.Rename(path+".withheld", path))
					after, err := os.ReadFile(path)
					must(t, err)
					if !bytes.Equal(before, after) {
						t.Fatal("real model fixture restore changed bytes")
					}
					realSuccess(t, a, wavs["en"], "en")
					emptyScratch(t, tmp)
				}()
				code, stage = MissingModel, ModelStage
			}
			assertRefusal(t, a.Transcribe(context.Background(), r), r, code, stage)
			emptyScratch(t, tmp)
			if name != "missing-model" {
				t.Run("valid-neighbor", func(t *testing.T) {
					realSuccess(t, a, wavs["en"], "en")
					emptyScratch(t, tmp)
				})
			}
		})
	}
}
