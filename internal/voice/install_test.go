package voice

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Wrong pinned content is rejected before decompression or publication; a
// failed download does not leave an install or a task-owned staging directory.
func TestInstallWrongSHAAndHTTPFailure(t *testing.T) {
	for _, status := range []int{200, 403} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			base := t.TempDir()
			calls := 0
			client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != ModelURL {
					t.Fatal("un-pinned URL")
				}
				if _, ok := r.Context().Deadline(); !ok {
					t.Fatal("unbounded download")
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("wrong model")), Header: make(http.Header)}, nil
			})}
			err := Install(context.Background(), base, client)
			if err == nil || calls != 1 {
				t.Fatalf("install=%v calls=%d", err, calls)
			}
			if status == 200 && !strings.Contains(err.Error(), "sha256 mismatch") {
				t.Fatalf("wrong rejection %v", err)
			}
			if status == 403 && !strings.Contains(err.Error(), "download refused") {
				t.Fatalf("wrong HTTP rejection: %v", err)
			}
			if _, err := os.Stat(ModelDir(base)); !os.IsNotExist(err) {
				t.Fatal("partial published model")
			}
			entries, err := os.ReadDir(filepath.Dir(ModelDir(base)))
			if err != nil || len(entries) != 0 {
				t.Fatalf("left stage=%v %v", entries, err)
			}
		})
	}
}

func fixtureArchive(t *testing.T, name string, kind byte) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	payload := []byte("hello")
	h := &tar.Header{Name: name, Mode: 0600, Typeflag: kind}
	if kind == tar.TypeReg {
		h.Size = int64(len(payload))
	}
	if kind == tar.TypeSymlink {
		h.Linkname = "../../escape"
	}
	if err := tw.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if kind == tar.TypeReg {
		tw.Write(payload)
	}
	tw.Close()
	return b.Bytes()
}

// Safe regular files round-trip; traversal, symlinks, and wrong manifest fail
// for the intended integrity/path reasons and never escape the fixture root.
func TestExtractSafetyAndManifest(t *testing.T) {
	sum := sha256.Sum256([]byte("hello"))
	manifest, _ := json.Marshal(map[string]string{"file": hex.EncodeToString(sum[:])})
	want := digest(manifest)
	for _, test := range []struct {
		name string
		kind byte
		pin  string
		ok   bool
	}{
		{modelName + "/file", tar.TypeReg, want, true},
		{modelName + "/../escape", tar.TypeReg, want, false},
		{modelName + "/..", tar.TypeReg, want, false},
		{modelName + "/link", tar.TypeSymlink, want, false},
		{modelName + "/file", tar.TypeReg, "wrong", false},
	} {
		t.Run(test.name+test.pin[:3], func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "model")
			os.Mkdir(dest, 0700)
			err := extract(context.Background(), bytes.NewReader(fixtureArchive(t, test.name, test.kind)), dest, test.pin)
			if test.ok {
				if err != nil {
					t.Fatal(err)
				}
				b, err := os.ReadFile(filepath.Join(dest, "file"))
				if err != nil || string(b) != "hello" {
					t.Fatalf("content=%q %v", b, err)
				}
			} else if err == nil {
				t.Fatal("accepted unsafe fixture")
			}
			if !test.ok {
				want := "unsafe voice model archive path"
				if test.kind == tar.TypeSymlink {
					want = "unsupported or oversized voice model archive entry"
				}
				if test.pin == "wrong" {
					want = "voice model file manifest mismatch"
				}
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("wrong rejection: want %q got %v", want, err)
				}
			}
			if _, err := os.Stat(filepath.Join(parent, "escape")); !os.IsNotExist(err) {
				t.Fatal("escaped extraction")
			}
		})
	}
}

// Configuration accepts only the explicit local enablement and the exact cap;
// malformed, extra-field, oversized and trailing JSON cannot silently default.
func TestConfigStrict(t *testing.T) {
	base := t.TempDir()
	good := Config{true, 180}
	if err := Save(base, good); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(base); err != nil || got != good {
		t.Fatalf("config=%+v %v", got, err)
	}
	for _, b := range []string{`{}`, `{"engine":"cloud","enabled":true,"max_seconds":180}`, `{"enabled":true,"max_seconds":181}`, `{"enabled":true,"max_seconds":180,"cloud":true}`, `{"enabled":true,"max_seconds":180} {}`, strings.Repeat("x", 5000)} {
		os.WriteFile(ConfigPath(base), []byte(b), 0600)
		if _, err := Load(base); err == nil {
			t.Fatal("accepted malformed configuration")
		}
	}
}

// Cached assets are authenticated, including parent directories: changed data,
// changed manifests, symlinks and cancellation cannot reach native inference.
func TestModelCacheIntegrity(t *testing.T) {
	for _, variant := range []string{"valid", "changed-file", "changed-manifest", "redirect", "cancelled", "missing"} {
		t.Run(variant, func(t *testing.T) {
			root := t.TempDir()
			payload := []byte("pinned model fixture")
			sum := sha256.Sum256(payload)
			manifest, _ := json.Marshal(map[string]string{"sub/model": hex.EncodeToString(sum[:])})
			pin := digest(manifest)
			os.Mkdir(filepath.Join(root, "sub"), 0700)
			os.WriteFile(filepath.Join(root, "sub/model"), payload, 0600)
			os.WriteFile(filepath.Join(root, "manifest.json"), manifest, 0600)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch variant {
			case "changed-file":
				os.WriteFile(filepath.Join(root, "sub/model"), []byte("tampered"), 0600)
			case "changed-manifest":
				os.WriteFile(filepath.Join(root, "manifest.json"), []byte("{}"), 0600)
			case "redirect":
				os.Rename(filepath.Join(root, "sub"), filepath.Join(root, "original"))
				if err := os.Symlink(filepath.Join(root, "original"), filepath.Join(root, "sub")); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				cancel()
			case "missing":
				os.Remove(filepath.Join(root, "sub/model"))
			}
			err := validateModel(ctx, root, pin)
			if variant == "valid" && err != nil {
				t.Fatal(err)
			}
			if variant != "valid" && err == nil {
				t.Fatal("invalid cache admitted")
			}
			if variant != "valid" {
				want := map[string]string{"changed-file": "integrity check failed", "changed-manifest": "manifest is missing or changed", "redirect": "asset is missing or redirected", "cancelled": "context canceled", "missing": "asset is missing or redirected"}[variant]
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("wrong cache rejection: want %q got %v", want, err)
				}
			}
		})
	}
}
