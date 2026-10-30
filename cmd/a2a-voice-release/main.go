// Command a2a-voice-release builds, signs and hashes an isolated helper release.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/relux-works/a2a-swarm/internal/voicerelease"
)

func main() {
	out := flag.String("out", "", "new release directory (must not already exist)")
	flag.Parse()
	if *out == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: a2a-voice-release -out NEW_DIRECTORY")
		os.Exit(2)
	}
	sum, err := voicerelease.Build(context.Background(), *out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%s  MANIFEST.sha256\n", sum)
}
