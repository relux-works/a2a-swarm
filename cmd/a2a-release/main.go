// a2a-release is a repository build tool; it is not installed with the CLI.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/relux-works/a2a-swarm/internal/darwinrelease"
)

func run() error {
	root, e := os.Getwd()
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	args := os.Args[1:]
	if len(args) == 0 {
		return fmt.Errorf("usage: a2a-release admit|release|verify|compare")
	}
	switch args[0] {
	case "admit":
		if len(args) != 2 {
			return fmt.Errorf("usage: a2a-release admit vX.Y.Z")
		}
		a, e := darwinrelease.Admit(ctx, root, args[1], os.Getenv("RELEASE_ADMITTED_TAG_OBJECT"))
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(a)
	case "release":
		if len(args) != 2 || os.Getenv("RELEASE_ADMITTED_TAG_OBJECT") == "" {
			return fmt.Errorf("release requires admitted tag object")
		}
		return darwinrelease.RunRelease(ctx, root, args[1], os.Getenv("RELEASE_ADMITTED_TAG_OBJECT"))
	case "verify":
		if len(args) != 3 {
			return fmt.Errorf("usage: a2a-release verify PAYLOAD_DIR EVIDENCE_DIR")
		}
		return verify(args[1], args[2])
	case "compare":
		if len(args) != 4 {
			return fmt.Errorf("usage: a2a-release compare OLD_PAYLOAD NEW_PAYLOAD EVIDENCE_DIR")
		}
		out := args[3]
		if e = os.MkdirAll(out, 0700); e != nil {
			return e
		}
		var pairs [][]darwinrelease.Identity
		for i, dir := range args[1:3] {
			ev := filepath.Join(out, fmt.Sprint(i))
			if e = os.Mkdir(ev, 0700); e != nil {
				return e
			}
			var ids []darwinrelease.Identity
			for _, name := range darwinrelease.Binaries {
				id, e := darwinrelease.VerifyBinary(filepath.Join(dir, name), name, ev)
				if e != nil {
					return e
				}
				ids = append(ids, id)
			}
			pairs = append(pairs, ids)
		}
		if e = darwinrelease.CompareDR(pairs[0], pairs[1]); e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(pairs)
	}
	return fmt.Errorf("unknown release command")
}
func verify(dir, out string) error {
	if e := os.Mkdir(out, 0700); e != nil {
		return e
	}
	var ids []darwinrelease.Identity
	for _, name := range darwinrelease.Binaries {
		id, e := darwinrelease.VerifyBinary(filepath.Join(dir, name), name, out)
		if e != nil {
			return e
		}
		ids = append(ids, id)
	}
	return json.NewEncoder(os.Stdout).Encode(ids)
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, "a2a-release:", e)
		os.Exit(1)
	}
}
