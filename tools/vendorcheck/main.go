// vendorcheck binds a published build commit to main and regenerated vendor.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var errDrift = errors.New("vendor_tree_drift")

const mainRef = "refs/remotes/origin/main"
const buildRef = "refs/remotes/origin/curator-build"

type entry struct{ mode, oid string }

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("vendorcheck", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	repository := flags.String("repository", "", "absolute repository with freshly fetched origin refs")
	commit := flags.String("commit", "", "full build commit object ID")
	base := flags.String("main-commit", "", "full baseline commit reachable from origin/main")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || !filepath.IsAbs(*repository) {
		return errors.New("vendor_arguments_invalid: pass absolute --repository, --commit and --main-commit")
	}
	if !fullOID(*commit) || !fullOID(*base) {
		return errors.New("commit_selector_invalid: full lowercase object IDs required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, oid := range []string{*commit, *base} {
		resolved, err := git(ctx, *repository, nil, "rev-parse", "--verify", oid+"^{commit}")
		if err != nil || strings.TrimSpace(string(resolved)) != oid {
			return fmt.Errorf("commit_unavailable: %s", oid)
		}
	}
	if _, err := git(ctx, *repository, nil, "merge-base", "--is-ancestor", *base, mainRef); err != nil {
		return errors.New("main_commit_not_reachable")
	}
	selected, err := readTree(ctx, *repository, *commit)
	if err != nil {
		return err
	}
	baseline, err := readTree(ctx, *repository, *base)
	if err != nil {
		return err
	}
	if err := compareSource(baseline, selected); err != nil {
		return err
	}
	if _, err := git(ctx, *repository, nil, "merge-base", "--is-ancestor", *commit, buildRef); err != nil {
		return errors.New("build_pin_not_on_branch")
	}
	if _, ok := selected["skill-build.json"]; !ok {
		return errors.New("build_descriptor_missing")
	}
	scratch, err := os.MkdirTemp("", "vendorcheck-")
	if err != nil {
		return errors.New("vendor_temporary_state_failed")
	}
	defer os.RemoveAll(scratch)
	root := filepath.Join(scratch, "source")
	if err := materialize(ctx, *repository, root, selected); err != nil {
		return err
	}
	committedVendor := map[string]entry{}
	for path, item := range selected {
		if strings.HasPrefix(path, "vendor/") {
			committedVendor[strings.TrimPrefix(path, "vendor/")] = item
		}
	}
	if len(committedVendor) == 0 {
		return errors.New("vendor_tree_missing")
	}
	generated := filepath.Join(scratch, "generated")
	cmd := exec.CommandContext(ctx, "go", "mod", "vendor", "-o", generated)
	cmd.Dir, cmd.Stdout, cmd.Stderr = root, out, out
	cmd.Env = append(os.Environ(), "GOENV=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=", "GO111MODULE=on", "GOPROXY=off", "GOSUMDB=off", "GOVCS=*:off")
	if err := cmd.Run(); err != nil {
		return errors.New("vendor_generation_failed")
	}
	// The generator must not silently repair the selected module metadata.
	for _, name := range []string{"go.mod", "go.sum"} {
		item, exists := selected[name]
		if !exists {
			continue
		}
		want, err := git(ctx, *repository, nil, "cat-file", "blob", item.oid)
		got, readErr := os.ReadFile(filepath.Join(root, name))
		if err != nil || readErr != nil || !bytes.Equal(want, got) {
			return fmt.Errorf("vendor_generation_changed_source: %s", name)
		}
	}
	generatedVendor, err := directoryTree(generated, len(*commit))
	if err != nil {
		return err
	}
	if err := compareVendor(generatedVendor, committedVendor); err != nil {
		return err
	}
	fmt.Fprintf(out, "vendor_tree_equal commit=%s main=%s branch=curator-build\n", *commit, *base)
	return nil
}

func fullOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, ch := range s {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

func git(ctx context.Context, repo string, input []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-replace-objects", "-C", repo}, args...)...)
	cmd.Stdin = bytes.NewReader(input)
	// A caller's Git selectors must not redirect the named repository or objects.
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if !strings.HasPrefix(name, "GIT_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	return cmd.Output()
}

func readTree(ctx context.Context, repo, oid string) (map[string]entry, error) {
	data, err := git(ctx, repo, nil, "ls-tree", "-rz", "--full-tree", oid)
	if err != nil {
		return nil, errors.New("commit_tree_unavailable")
	}
	entries := map[string]entry{}
	for _, row := range bytes.Split(data, []byte{0}) {
		if len(row) == 0 {
			continue
		}
		header, path, ok := strings.Cut(string(row), "\t")
		fields := strings.Fields(header)
		if !ok || !fs.ValidPath(path) || strings.ContainsAny(path, "\r\n\t\\") || len(fields) != 3 {
			return nil, errors.New("commit_entry_unsupported")
		}
		if fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") || !fullOID(fields[2]) {
			return nil, fmt.Errorf("commit_entry_unsupported: %s", path)
		}
		entries[path] = entry{fields[0], fields[2]}
	}
	return entries, nil
}

func compareSource(main, build map[string]entry) error {
	paths := map[string]bool{}
	for path := range main {
		paths[path] = true
	}
	for path := range build {
		paths[path] = true
	}
	for _, path := range sortedKeys(paths) {
		if strings.HasPrefix(path, "vendor/") || path == "skill-build.json" {
			continue
		}
		left, present := main[path]
		right, exists := build[path]
		if present != exists || left != right {
			return fmt.Errorf("source_tree_drift: %s", path)
		}
	}
	return nil
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Read raw blobs, not archive/worktree bytes: attributes, ignore rules and HEAD
// cannot hide entries or change their content. Git modes remain part of proof.
func materialize(ctx context.Context, repo, root string, entries map[string]entry) error {
	paths := sortedKeys(entries)
	var requests strings.Builder
	for _, path := range paths {
		fmt.Fprintln(&requests, entries[path].oid)
	}
	data, err := git(ctx, repo, []byte(requests.String()), "cat-file", "--batch")
	if err != nil {
		return errors.New("commit_blob_unavailable")
	}
	reader := bufio.NewReader(bytes.NewReader(data))
	for _, path := range paths {
		header, err := reader.ReadString('\n')
		fields := strings.Fields(header)
		if err != nil || len(fields) != 3 || fields[0] != entries[path].oid || fields[1] != "blob" {
			return errors.New("commit_blob_unavailable")
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 || size > int64(len(data)) {
			return errors.New("commit_blob_unavailable")
		}
		body := make([]byte, int(size))
		if _, err := io.ReadFull(reader, body); err != nil {
			return errors.New("commit_blob_unavailable")
		}
		if end, err := reader.ReadByte(); err != nil || end != '\n' {
			return errors.New("commit_blob_unavailable")
		}
		if strings.HasPrefix(path, "vendor/") {
			continue
		}
		destination := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return errors.New("commit_materialization_failed")
		}
		mode := fs.FileMode(0o644)
		if entries[path].mode == "100755" {
			mode = 0o755
		}
		if err := os.WriteFile(destination, body, mode); err != nil {
			return errors.New("commit_materialization_failed")
		}
	}
	return nil
}

// Hash generated regular files using the repository's Git object format. Compare
// those entries directly to the committed tree, including executable modes.
// No committed vendor path is round-tripped through the host filesystem.
func directoryTree(root string, oidLength int) (map[string]entry, error) {
	entries := map[string]entry{}
	err := filepath.WalkDir(root, func(path string, item fs.DirEntry, err error) error {
		if err != nil {
			return errors.New("vendor_generated_tree_unreadable")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." || item.IsDir() {
			return nil
		}
		name := filepath.ToSlash(relative)
		if !item.Type().IsRegular() {
			return fmt.Errorf("%w: %s", errDrift, name)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%w: %s", errDrift, name)
		}
		info, err := item.Info()
		if err != nil {
			return fmt.Errorf("%w: %s", errDrift, name)
		}
		mode := "100644"
		if info.Mode().Perm()&0o111 != 0 {
			mode = "100755"
		}
		object := append([]byte(fmt.Sprintf("blob %d\x00", len(data))), data...)
		oid := ""
		if oidLength == 64 {
			sum := sha256.Sum256(object)
			oid = hex.EncodeToString(sum[:])
		} else {
			sum := sha1.Sum(object)
			oid = hex.EncodeToString(sum[:])
		}
		entries[name] = entry{mode, oid}
		return nil
	})
	return entries, err
}

func compareVendor(expected, actual map[string]entry) error {
	paths := map[string]bool{}
	for path := range expected {
		paths[path] = true
	}
	for path := range actual {
		paths[path] = true
	}
	for _, path := range sortedKeys(paths) {
		left, present := expected[path]
		right, exists := actual[path]
		if present != exists || left != right {
			return fmt.Errorf("%w: %s", errDrift, path)
		}
	}
	return nil
}

func compareTrees(expected, actual string) error {
	left, err := directoryTree(expected, 40)
	if err != nil {
		return err
	}
	right, err := directoryTree(actual, 40)
	if err != nil {
		return err
	}
	return compareVendor(left, right)
}
