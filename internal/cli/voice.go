package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/voice"
)

type voicePostFailure uint8

const (
	voiceInvalidResult voicePostFailure = iota + 1
	voiceInvalidWaveform
	voiceUploadFailure
	voiceFetchFailure
	voiceIntegrityFailure
	voiceSendFailure
)

func (f voicePostFailure) Error() string {
	switch f {
	case voiceInvalidResult:
		return "invalid rendered voice result"
	case voiceInvalidWaveform:
		return "invalid voice waveform"
	case voiceUploadFailure:
		return "voice upload failed"
	case voiceFetchFailure:
		return "voice fetch-back failed"
	case voiceIntegrityFailure:
		return "voice fetch-back sha256 mismatch"
	case voiceSendFailure:
		return "voice event send failed or acknowledgement unknown; do not resend text"
	default:
		return voice.NewSynthesisError(voice.SynthesisUnknown, -1).Error()
	}
}

// voiceStatusPaths selects only public routing metadata. In particular, status
// must work without credential metadata, a secret backend or owner-DM state.
func (a *App) voiceStatusPaths() (config.Paths, error) {
	p, err := config.ResolvePaths(a.Getenv)
	if err != nil {
		return p, errors.New("voice status: cannot resolve configuration")
	}
	r, err := config.LoadConnections(p)
	if err != nil {
		return p, errors.New("voice status: invalid connection registry")
	}
	if r == nil {
		if a.profile != "" || a.connectionFingerprint != "" {
			return p, usagef("voice status: profile selection requires connections.json")
		}
		return p, nil
	}
	label := a.profile
	if label == "" {
		label = r.Current
	}
	c, ok := r.Profiles[label]
	if !ok {
		return p, usagef("voice status: unknown connection profile")
	}
	name := a.as
	if name == "" {
		name = a.Getenv(config.EnvAgent)
	}
	if a.connectionFingerprint != "" {
		b, err := c.Binding(name, a.human)
		if err != nil || b.Fingerprint() != a.connectionFingerprint {
			return p, errors.New("voice status: selected connection fingerprint changed")
		}
	}
	p, err = c.Paths(p, name, a.human)
	if err != nil {
		return p, errors.New("voice status: cannot resolve selected configuration")
	}
	return p, nil
}

func cmdVoiceStatus(a *App, args []string) error {
	fs := newFlagSet("voice status")
	asJSON := fs.Bool("json", false, "print local voice status as JSON")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("voice status takes flags only")
	}
	p, err := a.voiceStatusPaths()
	if err != nil {
		return err
	}
	s := voice.Inspect(p.Base)
	if *asJSON {
		return json.NewEncoder(a.Stdout).Encode(s)
	}
	_, err = fmt.Fprintf(a.Stdout, "voice status: config=%s native=%s model=%s encoder=%s cap=%ds synthesis=%s last_attempt=%s\n", s.Config, s.Native, s.Model, s.Encoder, s.MaxSeconds, s.Synthesis, s.LastAttempt)
	return err
}

func cmdVoice(ctx context.Context, a *App, args []string) error {
	if len(args) > 0 && args[0] == "status" {
		return cmdVoiceStatus(a, args[1:])
	}
	if len(args) == 0 {
		return usagef("usage: a2a voice status [--json] | voice config --enable | voice install")
	}
	p, err := a.paths()
	if err != nil {
		return err
	}
	switch args[0] {
	case "config":
		fs := newFlagSet("voice config")
		enabled := fs.Bool("enable", false, "explicitly enable local Irina voice replies")
		rest, err := parseInterspersed(fs, args[1:])
		if err != nil {
			return err
		}
		if len(rest) != 0 {
			return usagef("voice config takes flags only")
		}
		c := voice.Config{Enabled: *enabled, MaxSeconds: voice.MaxSeconds}
		if err = c.Validate(); err != nil {
			return usagef("%s", err)
		}
		if err = voice.Save(p.Base, c); err != nil {
			return err
		}
		a.printf("voice config: enabled sherpa/Irina, cap=%ds\n", c.MaxSeconds)
		return nil
	case "install":
		if len(args) != 1 {
			return usagef("voice install takes no arguments (pinned Irina bundle only)")
		}
		if err = voice.Install(ctx, p.Base, a.HTTP); err != nil {
			return err
		}
		a.printf("voice model installed: Irina sha256=%s\n", voice.ModelSHA256)
		return nil
	}
	return usagef("unknown voice command")
}

func (s *session) postVoice(ctx context.Context, roomID, root, textID, text string) error {
	cfg, err := voice.Load(s.paths.Base)
	if err != nil {
		return voice.NewSynthesisError(voice.SynthesisValidation, -1)
	}
	renderer := s.app.voiceRenderer
	if renderer == nil {
		renderer = voice.LocalRenderer{}
	}
	audio, err := renderer.Render(ctx, s.paths.Base, cfg, text)
	if err != nil {
		return voice.SafeRenderError(err)
	}
	if len(audio.OGG) == 0 || len(audio.OGG) > voice.MaxAudioBytes || audio.DurationMS <= 0 || audio.DurationMS > voice.MaxSeconds*1000 || len(audio.Waveform) == 0 || len(audio.Waveform) > 100 {
		return voiceInvalidResult
	}
	for _, v := range audio.Waveform {
		if v < 0 || v > 1024 {
			return voiceInvalidWaveform
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	mxc, err := s.client.Upload(ctx, "voice.ogg", "audio/ogg", bytes.NewReader(audio.OGG), int64(len(audio.OGG)))
	if err != nil {
		return voiceUploadFailure
	}
	remote, err := s.client.Download(ctx, mxc)
	if err != nil {
		return voiceFetchFailure
	}
	h := sha256.New()
	n, readErr := io.Copy(h, io.LimitReader(remote, int64(len(audio.OGG))+1))
	closeErr := remote.Close()
	want := sha256.Sum256(audio.OGG)
	if readErr != nil || closeErr != nil || n != int64(len(audio.OGG)) || !bytes.Equal(h.Sum(nil), want[:]) {
		return voiceIntegrityFailure
	}
	body := "Voice reply"
	if audio.Truncated {
		body = "Voice reply (truncated; full text in replied message)"
	}
	// No second protocol header/mentions: the text is the single authoritative
	// decision/request. This is only an audio representation replying to it.
	content := map[string]any{
		"msgtype": "m.audio", "body": body, "filename": "voice.ogg", "url": mxc,
		"info":                     map[string]any{"mimetype": "audio/ogg", "size": len(audio.OGG), "duration": audio.DurationMS},
		"org.matrix.msc3245.voice": map[string]any{},
		"org.matrix.msc1767.text":  body,
		"org.matrix.msc1767.file":  map[string]any{"name": "voice.ogg", "mimetype": "audio/ogg", "size": len(audio.OGG), "url": mxc},
		"org.matrix.msc1767.audio": map[string]any{"duration": audio.DurationMS, "waveform": audio.Waveform},
		"m.relates_to":             matrix.RelatesTo{RelType: "m.thread", EventID: root, IsFallingBack: false, InReplyTo: &matrix.InReplyTo{EventID: textID}},
		"m.mentions":               matrix.Mentions{},
	}
	if _, err = s.client.SendMessageReceipt(ctx, roomID, content); err != nil {
		return voiceSendFailure
	}
	return nil
}

func (a *App) voiceWarning(err error) {
	// Admit only closed local types, including at this final output boundary.
	// Never format an arbitrary runner/renderer error or an enclosing wrapper.
	message := voice.SafeRenderError(err).Error()
	var post voicePostFailure
	if errors.As(err, &post) {
		message = post.Error()
	}
	fmt.Fprintf(a.Stderr, "a2a: warning [VOICE_TEXT_ONLY]: %s; text retained\n", message)
}
