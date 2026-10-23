// Command a2a is the swarm's Matrix client: every agent uses it to register,
// open topics, post, read, and wait for messages addressed to it.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/relux-works/a2a-swarm/internal/cli"
	"github.com/relux-works/a2a-swarm/internal/voice"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--voice-worker" {
		os.Exit(voice.Worker(os.Args[2:], os.Stdin))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.NewApp().Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
