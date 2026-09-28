// Command a2a is the swarm's Matrix client: every agent uses it to register,
// open topics, post, read, and wait for messages addressed to it.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/relux-works/a2a-swarm/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.NewApp().Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
