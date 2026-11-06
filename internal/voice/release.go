package voice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const WorkerName = "a2a-voice-worker"
const ManifestName = "MANIFEST.sha256"

// ReleaseFiles is the complete v1 payload, in manifest order. The manifest
// authenticates the payload; its own digest is pinned separately in config.
func ReleaseFiles() []string {
	return []string{WorkerName, "lib/libonnxruntime.dylib", "lib/libsherpa-onnx-c-api.dylib"}
}

func validSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// HashReleaseFile hashes an opened regular file and checks its identity against
// the pathname before and after reading. Opens cannot follow a link or block on a FIFO.
func HashReleaseFile(ctx context.Context, path string) (string, error) {
	return copyReleaseFile(ctx, path, io.Discard, 256<<20)
}

func sameReleaseFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && a.Mode().IsRegular() && b.Mode().IsRegular() &&
		os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode()
}

func copyReleaseFile(ctx context.Context, path string, dst io.Writer, limit int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() > limit {
		return "", errHelper
	}
	f, err := openReleaseFile(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !sameReleaseFile(before, opened) {
		return "", errHelper
	}
	h := sha256.New()
	buf := make([]byte, 64<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, readErr := f.Read(buf)
		total += int64(n)
		if total > limit {
			return "", errHelper
		}
		if _, err := io.MultiWriter(h, dst).Write(buf[:n]); err != nil {
			return "", err
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	after, err := os.Lstat(path)
	final, statErr := f.Stat()
	if err != nil || statErr != nil || !sameReleaseFile(opened, final) || !sameReleaseFile(final, after) || total != opened.Size() {
		return "", errHelper
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var errHelper = errors.New("voice helper unavailable or integrity check failed")

// VerifyWorker authenticates the canonical source release. Execution must use
// PrepareWorker, which retains the authenticated bytes in a private snapshot.
func VerifyWorker(ctx context.Context, c Config) (string, error) {
	return verifyRelease(ctx, c, "")
}

// PreparedWorker owns a freshly created private payload until the subprocess
// has exited (including dyld loading). Close removes it on every return path.
type PreparedWorker struct {
	Path    string
	cleanup func()
}

func (w PreparedWorker) Close() {
	if w.cleanup != nil {
		w.cleanup()
	}
}

func PrepareWorker(ctx context.Context, c Config) (PreparedWorker, error) {
	dir, err := os.MkdirTemp("", "a2a-voice-exec-*")
	if err != nil {
		return PreparedWorker{}, errHelper
	}
	cleanup := func() { os.RemoveAll(dir) }
	if err := os.Mkdir(filepath.Join(dir, "lib"), 0700); err != nil {
		cleanup()
		return PreparedWorker{}, errHelper
	}
	if _, err := verifyRelease(ctx, c, dir); err != nil {
		cleanup()
		return PreparedWorker{}, errHelper
	}
	// Resolve the OS temp directory's own aliases as well; execute exactly this
	// verified snapshot, never the mutable source release.
	exe, err := filepath.EvalSymlinks(filepath.Join(dir, WorkerName))
	if err != nil {
		cleanup()
		return PreparedWorker{}, errHelper
	}
	return PreparedWorker{Path: exe, cleanup: cleanup}, nil
}

func verifyRelease(ctx context.Context, c Config, snapshot string) (string, error) {
	if c.Validate() != nil || c.Worker == "" || ctx.Err() != nil {
		return "", errHelper
	}
	canonical, err := filepath.EvalSymlinks(c.Worker)
	if err != nil || canonical != c.Worker {
		return "", errHelper
	}
	root := filepath.Dir(canonical)
	for dir, names := range map[string][]string{root: {ManifestName, WorkerName, "lib"}, filepath.Join(root, "lib"): {"libonnxruntime.dylib", "libsherpa-onnx-c-api.dylib"}} {
		st, err := os.Lstat(dir)
		if err != nil || !st.IsDir() {
			return "", errHelper
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != len(names) {
			return "", errHelper
		}
		for i, entry := range entries {
			if entry.Name() != names[i] {
				return "", errHelper
			}
		}
	}
	var manifest strings.Builder
	got, err := copyReleaseFile(ctx, filepath.Join(root, ManifestName), &manifest, 4096)
	if err != nil || got != c.ManifestSHA256 {
		return "", errHelper
	}
	lines := strings.Split(manifest.String(), "\n")
	files := ReleaseFiles()
	if len(lines) != len(files)+1 || lines[len(files)] != "" {
		return "", errHelper
	}
	for i, name := range files {
		want, file, ok := strings.Cut(lines[i], "  ")
		if !ok || file != name || !validSHA(want) || name == WorkerName && want != c.WorkerSHA256 {
			return "", errHelper
		}
		source := filepath.Join(root, filepath.FromSlash(name))
		resolved, err := filepath.EvalSymlinks(source)
		if err != nil || resolved != source {
			return "", errHelper
		}
		st, err := os.Lstat(source)
		if err != nil || name == WorkerName && st.Mode().Perm()&0111 == 0 {
			return "", errHelper
		}
		var dst io.Writer = io.Discard
		var out *os.File
		if snapshot != "" {
			out, err = os.OpenFile(filepath.Join(snapshot, filepath.FromSlash(name)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return "", errHelper
			}
			dst = out
		}
		got, err := copyReleaseFile(ctx, source, dst, 256<<20)
		if out != nil {
			mode := os.FileMode(0400)
			if name == WorkerName {
				mode = 0500
			}
			chmodErr := out.Chmod(mode)
			closeErr := out.Close()
			if chmodErr != nil || closeErr != nil {
				return "", errHelper
			}
		}
		if err != nil || got != want {
			return "", errHelper
		}
		if snapshot != "" {
			got, err = HashReleaseFile(ctx, filepath.Join(snapshot, filepath.FromSlash(name)))
			if err != nil || got != want {
				return "", errHelper
			}
		}
	}
	return canonical, nil
}
