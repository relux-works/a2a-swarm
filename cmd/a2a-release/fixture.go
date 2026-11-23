package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/relux-works/a2a-swarm/internal/fixturecompare"
)

// A completed diagnostic has its own nonzero exit, including with no findings.
// It can never be interpreted as a successful release verification by a shell.
var errFixtureDiagnostic = errors.New(fixturecompare.Label + ": fixture comparison recorded (exit 2)")

const fixtureUsage = `usage: a2a-release fixture-compare N_DIR NEXT_DIR NEW_EVIDENCE_DIR
NOT RELEASE VERIFICATION: completed diagnostics exit 2; operational refusals exit 1.
Each input must contain exactly a2a, a2a-keychain-status and SHA256SUMS.
SHA256SUMS must supply exactly two lowercase SHA-256 records: hex, two spaces, basename.
Capture pins independently; the comparator never synthesizes them.
Images must be regular executable Darwin arm64 files, at most 256 MiB each.
Evidence must be new, with an existing parent outside both input directories.
Standalone only: CI, runner, release and signing environment markers must be empty.`

func fixtureCompare(args []string) error {
	fmt.Fprintln(os.Stderr, fixturecompare.Label)
	if len(args) != 4 {
		return errors.New(fixtureUsage)
	}
	report, err := fixturecompare.Compare(args[1], args[2], args[3])
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		return err
	}
	return errFixtureDiagnostic
}

func exitCode(err error) int {
	if errors.Is(err, errFixtureDiagnostic) {
		return 2
	}
	if err != nil {
		return 1
	}
	return 0
}
