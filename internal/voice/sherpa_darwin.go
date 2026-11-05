//go:build darwin && cgo && sherpa

package voice

import (
	"io"
	"strings"

	sherpa "github.com/k2-fsa/sherpa-onnx-go-macos"
)

// Worker runs native inference in a reapable subprocess: CGO Generate itself
// has no context cancellation. Parent bounds its lifetime and discards diagnostics.
func Worker(args []string, input io.Reader) int {
	if len(args) != 2 {
		return 2
	}
	text, err := io.ReadAll(io.LimitReader(input, maxInputRunes*4+1))
	if err != nil || len(text) > maxInputRunes*4 {
		return 1
	}
	return withWorkerModel(args[0], args[1], manifestSHA256, func(paths workerPaths) int {
		cfg := sherpa.OfflineTtsConfig{Model: sherpa.OfflineTtsModelConfig{
			Vits: sherpa.OfflineTtsVitsModelConfig{Model: paths.model, Tokens: paths.tokens, DataDir: paths.data, NoiseScale: 0.667, NoiseScaleW: 0.8, LengthScale: 1}, NumThreads: 4, Provider: "cpu", Debug: 0}, MaxNumSentences: 2, SilenceScale: 1}
		tts := sherpa.NewOfflineTts(&cfg)
		if tts == nil {
			return 1
		}
		defer sherpa.DeleteOfflineTts(tts)
		audio := tts.Generate(strings.TrimSpace(string(text)), 0, 1)
		if audio == nil || len(audio.Samples) == 0 || !audio.Save(paths.output) {
			return 1
		}
		return 0
	})
}
