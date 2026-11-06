package cli

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/voice"
)

type fakeVoice struct {
	calls int
	audio voice.Audio
	err   error
	after func()
}

func (f *fakeVoice) Render(_ context.Context, _ string, _ voice.Config, _ string) (voice.Audio, error) {
	f.calls++
	if f.after != nil {
		f.after()
	}
	return f.audio, f.err
}
func voiceWorld(t *testing.T) (*env, string, *fakeVoice) {
	e, _ := world(t)
	e.now = func() time.Time { return time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC) }
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	f := &fakeVoice{audio: voice.Audio{OGG: []byte("OggS fixture OpusHead"), DurationMS: 1000, Waveform: []int{0, 1024}}}
	e.configure = func(a *App) { a.voiceRenderer = f }
	e.run("voice", "config", "--enable").ok(t)
	return e, root, f
}

// Proves the existing text content is byte-identical, voice replies to that
// exact text in its topic, and upload/fetch happen before the only audio send.
func TestPostVoiceShapeAndTextIdentity(t *testing.T) {
	e, root, f := voiceWorld(t)
	plain := e.as("alice", "post", "proj", "--topic", "alpha", "--kind", "decision", "--to", "bob", "Полный текст\nunchanged").ok(t)
	if f.calls != 0 {
		t.Fatal("text-only invoked renderer")
	}
	with := e.as("alice", "post", "proj", "--topic", "alpha", "--kind", "decision", "--to", "bob", "--voice", "Полный текст\nunchanged").ok(t)
	if with.stderr != "" {
		t.Fatal(with.stderr)
	}
	p, _ := e.hs.eventByID(strings.TrimSpace(plain.stdout))
	v, _ := e.hs.eventByID(strings.TrimSpace(with.stdout))
	if !bytes.Equal(p.Content, v.Content) {
		t.Fatalf("changed text content:\n%s\n%s", p.Content, v.Content)
	}
	e.hs.mu.Lock()
	events := append(e.hs.events[:0:0], e.hs.events...)
	e.hs.mu.Unlock()
	last := events[len(events)-1]
	c := decodeContent(t, last)
	if c["msgtype"] != "m.audio" || c[ContentKey] != nil || c["url"] == nil {
		t.Fatalf("audio content=%v", c)
	}
	rel := c["m.relates_to"].(map[string]any)
	if rel["event_id"] != root || rel["rel_type"] != "m.thread" || rel["is_falling_back"] == true || rel["m.in_reply_to"].(map[string]any)["event_id"] != v.EventID {
		t.Fatalf("relation=%v", rel)
	}
	if _, ok := c["org.matrix.msc3245.voice"].(map[string]any); !ok {
		t.Fatal("missing voice marker")
	}
	info := c["org.matrix.msc1767.audio"].(map[string]any)
	if info["duration"] != float64(1000) || !reflect.DeepEqual(info["waveform"], []any{float64(0), float64(1024)}) {
		t.Fatalf("audio info=%v", info)
	}
	if len(c["m.mentions"].(map[string]any)) != 0 {
		t.Fatal("duplicate mentions")
	}
	calls := e.hs.Calls()
	var upload, fetch, audioSend int
	for i, c := range calls {
		switch c.Method {
		case uploadRoute:
			upload = i
		case downloadRoute:
			fetch = i
		case sendRoute:
			audioSend = i
		}
	}
	if !(upload < fetch && fetch < audioSend) || f.calls != 1 {
		t.Fatalf("ordering %d,%d,%d calls=%d", upload, fetch, audioSend, f.calls)
	}
}

// Each rejected voice stage retains exactly the successful text and exit0,
// emits one marked warning, leaks no private diagnostics and publishes no audio.
func TestPostVoiceFailuresPreserveText(t *testing.T) {
	for _, stage := range []string{"config", "synth", "encode", "upload", "fetch", "hash", "oversize", "duration", "waveform", "audio-send"} {
		t.Run(stage, func(t *testing.T) {
			e, _, f := voiceWorld(t)
			switch stage {
			case "config":
				if err := voice.Save(e.dir, voice.Config{Enabled: true, MaxSeconds: 180}); err != nil {
					t.Fatal(err)
				}
				e.run("voice", "config", "--engine", "cloud").failsWith(t, ExitUsage, "flag provided but not defined") // rejected write keeps valid config
				// Remove only this fixture's own config to exercise missing configuration.
				removeVoiceConfig(t, e.dir)
			case "synth":
				f.err = voice.NewSynthesisError(voice.SynthesisUnknown, -1)
			case "encode":
				f.err = voice.EncodeFailure
			case "upload":
				e.hs.fail(uploadRoute, 403, "M_FORBIDDEN", "PRIVATE TEXT")
			case "fetch":
				e.hs.fail(downloadRoute, 403, "M_FORBIDDEN", "PRIVATE TEXT")
			case "hash":
				e.hs.uploadRewrite = func(b []byte) []byte { return append(b, 'x') }
			case "oversize":
				f.audio.OGG = make([]byte, voice.MaxAudioBytes+1)
			case "duration":
				f.audio.DurationMS = 180001
			case "waveform":
				f.audio.Waveform = []int{1025}
			case "audio-send":
				f.after = func() { e.hs.fail(sendRoute, 403, "M_FORBIDDEN", "PRIVATE TEXT") }
			}
			e.hs.mu.Lock()
			before := len(e.hs.events)
			e.hs.mu.Unlock()
			r := e.as("alice", "post", "proj", "--topic", "alpha", "--voice", "PRIVATE TEXT").ok(t)
			wantWarning := map[string]string{"config": "model/config validation refused", "synth": "unclassified failure", "encode": "voice encode failed", "upload": "voice upload failed", "fetch": "voice fetch-back failed", "hash": "voice fetch-back sha256 mismatch", "oversize": "invalid rendered voice result", "duration": "invalid rendered voice result", "waveform": "invalid voice waveform", "audio-send": "voice event send failed"}[stage]
			if !strings.Contains(r.stderr, wantWarning) {
				t.Fatalf("wrong failure stage: want %q got %q", wantWarning, r.stderr)
			}
			if stage != "config" && f.calls != 1 {
				t.Fatalf("did not exercise renderer for %s", stage)
			}
			if strings.Count(r.stderr, "[VOICE_TEXT_ONLY]") != 1 || strings.Count(strings.TrimSpace(r.stderr), "\n") != 0 || strings.Contains(r.stderr, "PRIVATE TEXT") {
				t.Fatalf("warning=%q", r.stderr)
			}
			ev, ok := e.hs.eventByID(strings.TrimSpace(r.stdout))
			if !ok || ev.Message().MsgType != "m.text" || !strings.HasSuffix(ev.Message().Body, "PRIVATE TEXT") {
				t.Fatal("text was lost or changed")
			}
			e.hs.mu.Lock()
			after := len(e.hs.events)
			e.hs.mu.Unlock()
			if after != before+1 {
				t.Fatalf("created %d events", after-before)
			}
			if stage == "config" && f.calls != 0 {
				t.Fatal("missing config ran renderer")
			}
		})
	}
}

// A failed text send keeps its original nonzero exit and performs no voice
// rendering, upload or fetch. The successful controls above prevent reject-all.
func TestPostVoiceTextFailureAndNoFlag(t *testing.T) {
	e, _, f := voiceWorld(t)
	removeVoiceConfig(t, e.dir)
	e.as("alice", "post", "proj", "--topic", "alpha", "plain").ok(t)
	if f.calls != 0 || e.hs.CallCount(uploadRoute) != 0 || e.hs.CallCount(downloadRoute) != 0 {
		t.Fatal("no flag ran voice work")
	}
	e.hs.fail(sendRoute, 403, "M_FORBIDDEN", "denied")
	r := e.as("alice", "post", "proj", "--topic", "alpha", "--voice", "text").failsWith(t, ExitError, "post to")
	if strings.Contains(r.stderr, "VOICE_TEXT_ONLY") || f.calls != 0 || e.hs.CallCount(uploadRoute) != 0 {
		t.Fatal("failed text attempted voice or changed verdict")
	}
}

// Truncated output is visibly identified without changing the original text.
func TestPostVoiceTruncationCaption(t *testing.T) {
	e, _, f := voiceWorld(t)
	f.audio.Truncated = true
	e.as("alice", "post", "proj", "--topic", "alpha", "--voice", "text").ok(t)
	e.hs.mu.Lock()
	last := e.hs.events[len(e.hs.events)-1]
	e.hs.mu.Unlock()
	if !strings.Contains(last.Message().Body, "truncated") {
		t.Fatal("capped audio claimed complete")
	}
}
