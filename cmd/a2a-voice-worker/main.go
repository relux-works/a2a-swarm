// Command a2a-voice-worker is the optional native inference process.
package main

import (
	"context"
	"os"

	"github.com/relux-works/a2a-swarm/internal/voice"
	"github.com/relux-works/a2a-swarm/internal/voiceworker"
)

func main() {
	// Even direct render-only invocations authenticate the model before inference.
	if len(os.Args) != 3 || voice.ValidateModel(context.Background(), os.Args[1]) != nil {
		os.Exit(1)
	}
	os.Exit(voiceworker.Worker(os.Args[1:], os.Stdin))
}
