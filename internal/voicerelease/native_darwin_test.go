package voicerelease

import (
	"bytes"
	"context"
	"debug/macho"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/voice"
)

func copyRelease(t *testing.T, source string) voice.Config {
	t.Helper()
	root := filepath.Join(t.TempDir(), "relocated")
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range append(voice.ReleaseFiles(), voice.ManifestName) {
		b, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0600)
		if name == voice.WorkerName {
			mode = 0700
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), b, mode); err != nil {
			t.Fatal(err)
		}
	}
	c := voice.Config{Enabled: true, MaxSeconds: 180, Worker: filepath.Join(root, voice.WorkerName)}
	var err error
	c.WorkerSHA256, err = voice.HashReleaseFile(context.Background(), c.Worker)
	if err != nil {
		t.Fatal(err)
	}
	c.ManifestSHA256, err = voice.HashReleaseFile(context.Background(), filepath.Join(root, voice.ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A real signed release synthesizes and encodes after relocation with an empty
// module cache; missing/one-byte-corrupt dylibs fail natively AND before parent exec.
func TestNativeReleaseRelocationAndTampering(t *testing.T) {
	source, base := os.Getenv("A2A_VOICE_RELEASE_DIR"), os.Getenv("A2A_VOICE_IT_BASE")
	if source == "" || base == "" {
		t.Skip("explicit isolated release and model required")
	}
	if !filepath.IsAbs(source) || !filepath.IsAbs(base) {
		t.Fatal("absolute isolated paths required")
	}
	t.Setenv("GOMODCACHE", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := voice.ValidateModel(ctx, base); err != nil {
		t.Fatal(err)
	}
	t.Run("relocated", func(t *testing.T) {
		c := copyRelease(t, source)
		if _, err := voice.VerifyWorker(ctx, c); err != nil {
			t.Fatal(err)
		}
		for _, name := range voice.ReleaseFiles() {
			if err := Audit(filepath.Join(filepath.Dir(c.Worker), filepath.FromSlash(name)), name == voice.WorkerName); err != nil {
				t.Fatal(err)
			}
		}
		a, err := (voice.LocalRenderer{}).Render(ctx, base, c, "Проверка голосового ответа. Полный текст сохраняется в сообщении.")
		if err != nil {
			t.Fatal(err)
		}
		nonzero := false
		for _, v := range a.Waveform {
			nonzero = nonzero || v > 0
		}
		if !nonzero || a.Truncated || a.DurationMS < 1000 || a.DurationMS > 180000 {
			t.Fatalf("invalid audio duration=%d truncated=%v nonzero=%v", a.DurationMS, a.Truncated, nonzero)
		}
		out := filepath.Join(t.TempDir(), "voice.ogg")
		if err := os.WriteFile(out, a.OGG, 0600); err != nil {
			t.Fatal(err)
		}
		b, err := command(ctx, "ffprobe", "-v", "error", "-show_entries", "stream=codec_name,sample_rate,channels", "-of", "json", out)
		if err != nil {
			t.Fatal(err)
		}
		var probe struct {
			Streams []struct {
				CodecName  string `json:"codec_name"`
				SampleRate string `json:"sample_rate"`
				Channels   int    `json:"channels"`
			} `json:"streams"`
		}
		if err := json.Unmarshal(b, &probe); err != nil {
			t.Fatal(err)
		}
		if len(probe.Streams) != 1 || probe.Streams[0].CodecName != "opus" || probe.Streams[0].SampleRate != "48000" || probe.Streams[0].Channels != 1 {
			t.Fatalf("ffprobe: %s", b)
		}
		t.Logf("relocated release; GOMODCACHE empty; duration_ms=%d bytes=%d ffprobe=%s", a.DurationMS, len(a.OGG), bytes.TrimSpace(b))
	})
	for _, variant := range []string{"missing", "one-byte-changed"} {
		t.Run(variant, func(t *testing.T) {
			c := copyRelease(t, source)
			lib := filepath.Join(filepath.Dir(c.Worker), "lib", "libsherpa-onnx-c-api.dylib")
			if variant == "missing" {
				if err := os.Remove(lib); err != nil {
					t.Fatal(err)
				}
			} else {
				f, err := macho.Open(lib)
				if err != nil {
					t.Fatal(err)
				}
				section := f.Section("__text")
				if section == nil {
					f.Close()
					t.Fatal("native code section missing")
				}
				offset := int(section.Offset)
				f.Close()
				b, err := os.ReadFile(lib)
				if err != nil {
					t.Fatal(err)
				}
				b[offset] ^= 1
				if err := os.WriteFile(lib, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := voice.VerifyWorker(ctx, c); err == nil {
				t.Fatal("parent admitted damaged library")
			}
			if _, err := (voice.LocalRenderer{}).Render(ctx, base, c, "Проверка"); err == nil {
				t.Fatal("parent rendered damaged release")
			}
			childCtx, stop := context.WithTimeout(ctx, 30*time.Second)
			defer stop()
			cmd := exec.CommandContext(childCtx, c.Worker, base, filepath.Join(t.TempDir(), "native.wav"))
			cmd.Stdin = strings.NewReader("Проверка")
			// Native diagnostics are discarded, including on loader failure.
			if err := cmd.Run(); err == nil || childCtx.Err() != nil {
				t.Fatalf("native damaged release verdict: %v deadline=%v", err, childCtx.Err())
			}
		})
	}
}
