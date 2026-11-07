// vendorcheck compares a build checkout with a freshly generated vendor tree.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

var errDrift = errors.New("vendor_tree_drift")

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	repository := flag.String("repository", "", "absolute build checkout to verify")
	flag.Parse()
	if !filepath.IsAbs(*repository) || flag.NArg() != 0 {
		return errors.New("vendor_repository_required: pass an absolute --repository")
	}
	if info, err := os.Lstat(filepath.Join(*repository, "vendor")); err != nil || !info.IsDir() {
		return errors.New("vendor_tree_missing")
	}
	scratch, err := os.MkdirTemp("", "vendorcheck-")
	if err != nil {
		return errors.New("vendor_temporary_state_failed")
	}
	defer os.RemoveAll(scratch)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	generated := filepath.Join(scratch, "vendor")
	cmd := exec.CommandContext(ctx, "go", "mod", "vendor", "-o", generated)
	cmd.Dir, cmd.Stdout, cmd.Stderr = *repository, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return errors.New("vendor_generation_failed")
	}
	if err := compareTrees(generated, filepath.Join(*repository, "vendor")); err != nil {
		return err
	}
	fmt.Println("vendor_tree_equal")
	return nil
}

// compareTrees checks names, entry types and regular-file bytes in both
// directions. It does not compare timestamps or filesystem permissions.
func compareTrees(expected, actual string) error {
	entries := make(map[string]fs.DirEntry)
	if err := filepath.WalkDir(expected, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(expected, path)
		if err != nil {
			return err
		}
		if relative != "." {
			entries[relative] = entry
		}
		return nil
	}); err != nil {
		return fmt.Errorf("vendor_generated_tree_unreadable: %w", err)
	}
	if err := filepath.WalkDir(actual, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("%w: unreadable entry", errDrift)
		}
		relative, err := filepath.Rel(actual, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		want, present := entries[relative]
		if !present || want.Type() != entry.Type() || (!entry.IsDir() && !entry.Type().IsRegular()) {
			return fmt.Errorf("%w: %s", errDrift, filepath.ToSlash(relative))
		}
		delete(entries, relative)
		if entry.IsDir() {
			return nil
		}
		left, err := os.ReadFile(filepath.Join(expected, relative))
		if err != nil {
			return fmt.Errorf("vendor_generated_tree_unreadable: %s", filepath.ToSlash(relative))
		}
		right, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(left, right) {
			return fmt.Errorf("%w: %s", errDrift, filepath.ToSlash(relative))
		}
		return nil
	}); err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("%w: missing entries", errDrift)
	}
	return nil
}
