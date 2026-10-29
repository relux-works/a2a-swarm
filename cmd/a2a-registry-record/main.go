// Command a2a-registry-record describes the caller's own live Claude Code
// session-registry record by keys and JSON types only, and optionally writes
// a synthetic fixture for a reviewed qualification change. See
// docs/registry-qualification.md.
//
// Exit status: 0 success, 2 unusable input or a refused fixture.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/relux-works/a2a-swarm/internal/registryqual"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("a2a-registry-record", flag.ContinueOnError)
	fs.SetOutput(stderr)
	record := fs.String("record", "", "own live sessions/<pid>.json to describe (keys and types only)")
	fixture := fs.String("fixture", "", "also write a synthetic fixture to this new file")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *record == "" {
		fmt.Fprintln(stderr, "usage: a2a-registry-record --record PATH [--fixture NEW_FILE]")
		return 2
	}
	data, err := registryqual.ReadRecord(*record)
	if err != nil {
		fmt.Fprintln(stderr, "a2a-registry-record:", err)
		return 2
	}
	fields, err := registryqual.Manifest(data)
	if err != nil {
		fmt.Fprintln(stderr, "a2a-registry-record:", err)
		return 2
	}
	fmt.Fprintln(stdout, "record keys (in order, types only):")
	for _, f := range fields {
		fmt.Fprintf(stdout, "  %s\t%s\n", f.Key, f.Type)
	}
	if *fixture == "" {
		return 0
	}
	out, err := registryqual.Fixture(data)
	if err != nil {
		fmt.Fprintln(stderr, "a2a-registry-record:", err)
		return 2
	}
	f, err := os.OpenFile(*fixture, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		fmt.Fprintln(stderr, "a2a-registry-record:", err)
		return 2
	}
	if _, err := f.Write(out); err != nil {
		f.Close()
		fmt.Fprintln(stderr, "a2a-registry-record:", err)
		return 2
	}
	if err := f.Close(); err != nil {
		fmt.Fprintln(stderr, "a2a-registry-record:", err)
		return 2
	}
	fmt.Fprintln(stdout, "fixture written:", *fixture)
	return 0
}
