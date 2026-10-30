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

// HashReleaseFile refuses links, special files and oversized payloads. Context
// cancellation is checked between chunks as well as before any file access.
func HashReleaseFile(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 256<<20 {
		return "", errors.New("invalid voice release file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 64<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buf)
		total += int64(n)
		if total > 256<<20 {
			return "", errors.New("oversized voice release file")
		}
		h.Write(buf[:n])
		if err == io.EOF {
			return hex.EncodeToString(h.Sum(nil)), nil
		}
		if err != nil {
			return "", err
		}
	}
}

var errHelper = errors.New("voice helper unavailable or integrity check failed")

// VerifyWorker authenticates the helper AND its entire native dependency set
// before each invocation. No discovery, network access or fallback executable.
func VerifyWorker(ctx context.Context, c Config) (string, error) {
	if c.Validate() != nil || c.Worker == "" || ctx.Err() != nil {
		return "", errHelper
	}
	root := filepath.Dir(c.Worker)
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
	manifestPath := filepath.Join(root, ManifestName)
	st, err := os.Lstat(manifestPath)
	if err != nil || !st.Mode().IsRegular() {
		return "", errHelper
	}
	b, err := readBounded(manifestPath, 4096)
	if err != nil || digest(b) != c.ManifestSHA256 {
		return "", errHelper
	}
	lines := strings.Split(string(b), "\n")
	files := ReleaseFiles()
	if len(lines) != len(files)+1 || lines[len(files)] != "" {
		return "", errHelper
	}
	for i, name := range files {
		want, file, ok := strings.Cut(lines[i], "  ")
		if !ok || file != name || !validSHA(want) || name == WorkerName && want != c.WorkerSHA256 {
			return "", errHelper
		}
		got, err := HashReleaseFile(ctx, filepath.Join(root, filepath.FromSlash(name)))
		if err != nil || got != want {
			return "", errHelper
		}
	}
	st, err = os.Stat(c.Worker)
	if err != nil || st.Mode().Perm()&0111 == 0 {
		return "", errHelper
	}
	return c.Worker, nil
}
