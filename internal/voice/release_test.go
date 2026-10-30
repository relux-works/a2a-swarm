package voice

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func releaseFixture(t *testing.T) Config {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "lib"), 0700); err != nil {
		t.Fatal(err)
	}
	var manifest strings.Builder
	for _, name := range ReleaseFiles() {
		data := []byte("fixture " + name)
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), data, 0700); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&manifest, "%s  %s\n", digest(data), name)
	}
	b := []byte(manifest.String())
	if err := os.WriteFile(filepath.Join(root, ManifestName), b, 0600); err != nil {
		t.Fatal(err)
	}
	return Config{Enabled: true, MaxSeconds: 180, Worker: filepath.Join(root, WorkerName), WorkerSHA256: digest([]byte("fixture " + WorkerName)), ManifestSHA256: digest(b)}
}

// Valid relocated payloads pass; missing/modified workers, libraries, manifests,
// redirects, unlisted files and cancellation refuse before any process runs.
func TestVerifyWorkerCompatibilityAndTampering(t *testing.T) {
	for _, variant := range []string{"valid", "relocated", "worker-missing", "worker-changed", "library-missing", "library-changed", "manifest-missing", "manifest-changed", "manifest-repinned", "worker-pin", "no-exec", "worker-link", "library-link", "lib-link", "extra", "cancelled"} {
		t.Run(variant, func(t *testing.T) {
			c := releaseFixture(t)
			root := filepath.Dir(c.Worker)
			lib := filepath.Join(root, "lib", "libonnxruntime.dylib")
			manifest := filepath.Join(root, ManifestName)
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch variant {
			case "relocated":
				parent, err := filepath.EvalSymlinks(t.TempDir())
				must(err)
				moved := filepath.Join(parent, "moved")
				must(os.Rename(root, moved))
				c.Worker = filepath.Join(moved, WorkerName)
			case "worker-missing":
				must(os.Remove(c.Worker))
			case "worker-changed":
				must(os.WriteFile(c.Worker, []byte("changed"), 0700))
			case "library-missing":
				must(os.Remove(lib))
			case "library-changed":
				must(os.WriteFile(lib, []byte("changed"), 0600))
			case "manifest-missing":
				must(os.Remove(manifest))
			case "manifest-changed", "manifest-repinned":
				b, err := os.ReadFile(manifest)
				must(err)
				b = bytes.ReplaceAll(b, []byte("lib/libonnxruntime.dylib"), []byte("../escape"))
				must(os.WriteFile(manifest, b, 0600))
				if variant == "manifest-repinned" {
					c.ManifestSHA256 = digest(b)
				}
			case "worker-pin":
				c.WorkerSHA256 = strings.Repeat("0", 64)
			case "no-exec":
				must(os.Chmod(c.Worker, 0600))
			case "worker-link", "library-link":
				file := c.Worker
				if variant == "library-link" {
					file = lib
				}
				target := filepath.Join(t.TempDir(), "target")
				must(os.Rename(file, target))
				must(os.Symlink(target, file))
			case "lib-link":
				target := filepath.Join(t.TempDir(), "target")
				must(os.Rename(filepath.Join(root, "lib"), target))
				must(os.Symlink(target, filepath.Join(root, "lib")))
			case "extra":
				must(os.WriteFile(filepath.Join(root, "unexpected"), nil, 0600))
			case "cancelled":
				cancel()
			}
			got, err := VerifyWorker(ctx, c)
			if variant == "valid" || variant == "relocated" {
				if err != nil || got != c.Worker {
					t.Fatalf("valid release: %q %v", got, err)
				}
			} else if err != errHelper || got != "" {
				t.Fatalf("refusal: %q %v", got, err)
			}
		})
	}
}

type verifyingRunner struct{ calls int }

func (r *verifyingRunner) PrepareWorker(ctx context.Context, c Config) (PreparedWorker, error) {
	return PrepareWorker(ctx, c)
}
func (r *verifyingRunner) ValidateModel(context.Context, string) error { return nil }
func (r *verifyingRunner) Run(context.Context, string, []string, io.Reader) error {
	r.calls++
	return nil
}

// Worker and library damage refuses before exec; an intact payload reaches the bounded runner.
func TestRenderRejectsLibraryBeforeExec(t *testing.T) {
	for _, variant := range []string{"valid", "worker-missing", "worker-changed", "library-missing", "library-changed"} {
		t.Run(variant, func(t *testing.T) {
			c := releaseFixture(t)
			file := c.Worker
			if strings.HasPrefix(variant, "library-") {
				file = filepath.Join(filepath.Dir(c.Worker), "lib", "libonnxruntime.dylib")
			}
			if strings.HasSuffix(variant, "missing") {
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			} else if strings.HasSuffix(variant, "changed") {
				if err := os.WriteFile(file, []byte("changed"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			r := &verifyingRunner{}
			err := (LocalRenderer{Runner: r}).synth(context.Background(), t.TempDir(), c, "text", filepath.Join(t.TempDir(), "out.wav"))
			if variant == "valid" {
				if err != nil || r.calls != 1 {
					t.Fatalf("intact payload: calls=%d err=%v", r.calls, err)
				}
			} else if err == nil || r.calls != 0 {
				t.Fatalf("damaged payload reached exec: calls=%d err=%v", r.calls, err)
			}
		})
	}
}

// Rendering with an intact helper and missing model fails locally without any download attempt.
func TestRenderNeverDownloads(t *testing.T) {
	old := http.DefaultTransport
	calls := 0
	http.DefaultTransport = roundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, fmt.Errorf("unexpected network") })
	t.Cleanup(func() { http.DefaultTransport = old })
	c := releaseFixture(t)
	if _, err := (LocalRenderer{}).Render(context.Background(), t.TempDir(), c, "text"); err == nil || calls != 0 {
		t.Fatalf("missing model: err=%v network=%d", err, calls)
	}
}

// The helper config admits old enablement and complete canonical pins; partial,
// relative, picker, uppercase and malformed pins never become executable input.
func TestWorkerConfigPins(t *testing.T) {
	good := releaseFixture(t)
	for _, variant := range []string{"valid", "old", "relative", "wrong-name", "partial", "uppercase", "nonhex"} {
		c := good
		switch variant {
		case "old":
			c.Worker, c.WorkerSHA256, c.ManifestSHA256 = "", "", ""
		case "relative":
			c.Worker = WorkerName
		case "wrong-name":
			c.Worker = "/tmp/other-worker"
		case "partial":
			c.ManifestSHA256 = ""
		case "uppercase":
			c.WorkerSHA256 = strings.Repeat("A", 64)
		case "nonhex":
			c.WorkerSHA256 = strings.Repeat("z", 64)
		}
		err := c.Validate()
		if (variant == "valid" || variant == "old") != (err == nil) {
			t.Fatalf("%s: %v", variant, err)
		}
	}
}
