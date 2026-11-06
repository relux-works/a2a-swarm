package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/voice"
)

func fixtureDigest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func fixtureWrite(t *testing.T, p string, b []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, b, mode); err != nil {
		t.Fatal(err)
	}
}

// Build the actual CLI with only the model manifest pin replaced by a tiny
// fixture pin. The optional deterministic rename barrier runs AFTER the real
// ValidateModel. All helper verification, snapshot and execution code is original.
func buildVoiceWitnessCLI(t *testing.T, ctx context.Context) (string, []byte, []byte, []byte) {
	t.Helper()
	manifest, err := json.Marshal(map[string]string{"model.onnx": fixtureDigest([]byte("fixture model"))})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	source, err := filepath.Abs("../voice/install.go")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.Replace(b, []byte(`const manifestSHA256 = "e7c154c19f233349ff4c3578316b43cb144f594af9411aab4814dbfd69a63392"`), []byte(`const manifestSHA256 = "`+fixtureDigest(manifest)+`"`), 1)
	old := []byte("return validateModel(ctx, ModelDir(base), manifestSHA256)")
	replacement := []byte(`if err := validateModel(ctx, ModelDir(base), manifestSHA256); err != nil { return err }
 var swap struct { From, To string }
 b, err := os.ReadFile(filepath.Join(base, "before-exec.json"))
 if os.IsNotExist(err) { return nil }
 if err != nil { return err }
 if err := json.Unmarshal(b, &swap); err != nil { return err }
 if err := os.Rename(swap.From, swap.To); err != nil { return err }
 return os.Remove(filepath.Join(base, "before-exec.json"))`)
	if !bytes.Contains(b, old) {
		t.Fatal("model barrier source changed")
	}
	b = bytes.Replace(b, old, replacement, 1)
	modified := filepath.Join(dir, "install.go")
	fixtureWrite(t, modified, b, 0600)
	overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{source: modified}})
	if err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(dir, "overlay.json")
	fixtureWrite(t, overlayPath, overlay, 0600)
	exe := filepath.Join(dir, "a2a")
	cmd := exec.CommandContext(ctx, "go", "build", "-overlay", overlayPath, "-o", exe, "../../cmd/a2a")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture CLI build: %v %s", err, b)
	}
	helper := filepath.Join(dir, voice.WorkerName)
	cmd = exec.CommandContext(ctx, "go", "build", "-o", helper, "../voice/testdata/witness")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("witness build: %v %s", err, b)
	}
	payload, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.CommandContext(ctx, "go", "build", "-ldflags=-X main.buildVersion=changed", "-o", helper, "../voice/testdata/witness")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("alternate witness build: %v %s", err, b)
	}
	changed, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(payload, changed) {
		t.Fatal("alternate helper has identical bytes")
	}
	return exe, payload, changed, manifest
}

func executableRelease(t *testing.T, payload []byte) voice.Config {
	t.Helper()
	c := cliVoiceRelease(t)
	fixtureWrite(t, c.Worker, payload, 0700)
	c.WorkerSHA256 = fixtureDigest(payload)
	var manifest strings.Builder
	for _, name := range voice.ReleaseFiles() {
		p := filepath.Join(filepath.Dir(c.Worker), name)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&manifest, "%s  %s\n", fixtureDigest(b), name)
	}
	b := []byte(manifest.String())
	c.ManifestSHA256 = fixtureDigest(b)
	fixtureWrite(t, filepath.Join(filepath.Dir(c.Worker), voice.ManifestName), b, 0600)
	return c
}

// An executable positive reaches the production subprocess through real model
// validation; damage/refusal cases differ only at the named boundary. Rename
// barriers prove source swaps cannot change executed bytes or loaded siblings.
// These synthetic helpers/encoders prove wiring, not native synthesis or Opus.
func TestPostVoiceExecutableIntegrity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	exe, payload, alternate, modelManifest := buildVoiceWitnessCLI(t, ctx)
	for _, variant := range []string{"valid", "helper-changed", "library-changed", "manifest-changed", "parent-link", "dotdot-link", "worker-swap", "library-swap", "long", "second-worker", "second-library", "second-manifest"} {
		t.Run(variant, func(t *testing.T) {
			e, _, _ := voiceWorld(t)
			c := executableRelease(t, payload)
			root := filepath.Dir(c.Worker)
			model := voice.ModelDir(e.dir)
			if err := os.MkdirAll(model, 0700); err != nil {
				t.Fatal(err)
			}
			fixtureWrite(t, filepath.Join(model, "manifest.json"), modelManifest, 0600)
			fixtureWrite(t, filepath.Join(model, "model.onnx"), []byte("fixture model"), 0600)
			bin := t.TempDir()
			fixtureWrite(t, filepath.Join(bin, "ffmpeg"), payload, 0700)
			expectedCalls := 1
			changedTarget := ""
			var changedBytes []byte
			warning := ""
			swap := func(when, target string) {
				changedTarget = target
				replacement := filepath.Join(t.TempDir(), "replacement")
				changedBytes = []byte("unverified replacement")
				if filepath.Base(target) == voice.WorkerName {
					changedBytes = alternate
				}
				fixtureWrite(t, replacement, changedBytes, 0700)
				b, err := json.Marshal(map[string]string{"From": replacement, "To": target})
				if err != nil {
					t.Fatal(err)
				}
				fixtureWrite(t, filepath.Join(e.dir, when+".json"), b, 0600)
			}
			switch variant {
			case "helper-changed":
				fixtureWrite(t, c.Worker, alternate, 0700)
				expectedCalls = 0
			case "library-changed":
				fixtureWrite(t, filepath.Join(root, "lib/libonnxruntime.dylib"), []byte("changed"), 0600)
				expectedCalls = 0
			case "manifest-changed":
				fixtureWrite(t, filepath.Join(root, voice.ManifestName), []byte("changed"), 0600)
				expectedCalls = 0
			case "parent-link":
				link := filepath.Join(t.TempDir(), "linked")
				if err := os.Symlink(filepath.Dir(root), link); err != nil {
					t.Fatal(err)
				}
				c.Worker = link + "/" + filepath.Base(root) + "/" + voice.WorkerName
				expectedCalls = 0
			case "dotdot-link":
				// Pins resolve lexically to root; execution of the old code resolves to a
				// different release through hop/.., without changing c's authenticated tree.
				other := executableRelease(t, alternate)
				otherParent, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				nested := filepath.Join(otherParent, filepath.Base(root))
				if err := os.Rename(filepath.Dir(other.Worker), nested); err != nil {
					t.Fatal(err)
				}
				sub := filepath.Join(otherParent, "sub")
				if err := os.Mkdir(sub, 0700); err != nil {
					t.Fatal(err)
				}
				hop := filepath.Join(filepath.Dir(root), "hop")
				if err := os.Symlink(sub, hop); err != nil {
					t.Fatal(err)
				}
				c.Worker = hop + "/../" + filepath.Base(root) + "/" + voice.WorkerName
				expectedCalls = 0
			case "worker-swap":
				swap("before-exec", c.Worker)
			case "library-swap":
				swap("before-exec", filepath.Join(root, "lib/libonnxruntime.dylib"))
			case "second-worker":
				swap("after-first", c.Worker)
			case "second-library":
				swap("after-first", filepath.Join(root, "lib/libonnxruntime.dylib"))
			case "second-manifest":
				swap("after-first", filepath.Join(root, voice.ManifestName))
			case "long":
				expectedCalls = 2
			}
			// Write config directly to include the ambiguous spelling negative; config
			// validation and worker verification are both exercised by the built CLI.
			cfg, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			fixtureWrite(t, voice.ConfigPath(e.dir), cfg, 0600)
			text := "Полный текст сохранён"
			if variant == "long" || strings.HasPrefix(variant, "second-") {
				text = strings.Repeat("Я", 6001)
			}
			args := []string{"--as", "alice", "post", "proj", "--topic", "alpha", "--voice", text}
			cmd := exec.CommandContext(ctx, exe, args...)
			cmd.Env = []string{"PATH=" + bin, "A2A_CONFIG_DIR=" + e.dir, "A2A_SECRET_STORE=file"}
			var out, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("text exit changed: %v %s", err, &stderr)
			}
			ev, ok := e.hs.eventByID(strings.TrimSpace(out.String()))
			if !ok || ev.Message().MsgType != "m.text" || !strings.HasSuffix(ev.Message().Body, text) {
				t.Fatal("lost original text")
			}
			if changedTarget != "" {
				changed, err := os.ReadFile(changedTarget)
				if err != nil || !bytes.Equal(changed, changedBytes) {
					t.Fatalf("rename barrier did not execute: %q %v", changed, err)
				}
			}
			marker, err := os.ReadFile(filepath.Join(e.dir, "execution-marker"))
			if expectedCalls == 0 {
				if !os.IsNotExist(err) {
					t.Fatalf("refused payload executed: %s %v", marker, err)
				}
			} else {
				if err != nil {
					t.Fatalf("intact payload never executed: %v", err)
				}
				lines := bytes.Split(bytes.TrimSpace(marker), []byte("\n"))
				if len(lines) != expectedCalls {
					t.Fatalf("executions=%d want=%d", len(lines), expectedCalls)
				}
				for _, line := range lines {
					var got struct {
						Executable  string
						Libraries   []string
						Version     string
						Environment []string
					}
					if err := json.Unmarshal(line, &got); err != nil {
						t.Fatal(err)
					}
					if len(got.Environment) != 0 || got.Version != "intact" || got.Executable == c.Worker || len(got.Libraries) != 2 || got.Libraries[0] != "release fixture" || got.Libraries[1] != "release fixture" {
						t.Fatalf("executed unverified payload: %+v", got)
					}
					if _, err := os.Stat(got.Executable); !os.IsNotExist(err) {
						t.Fatal("private execution payload retained")
					}
				}
			}
			refused := expectedCalls == 0 || strings.HasPrefix(variant, "second-")
			if refused {
				warning = "a2a: warning [VOICE_TEXT_ONLY]: voice synthesis failed or timed out; check the local engine and installed model; text retained\n"
				if variant == "dotdot-link" {
					// Strict config rejects the ambiguous spelling even before rendering.
					if !strings.HasPrefix(stderr.String(), "a2a: warning [VOICE_TEXT_ONLY]: ") {
						t.Fatalf("missing warning: %q", stderr.String())
					}
					warning = "a2a: warning [VOICE_TEXT_ONLY]: voice helper requires an absolute a2a-voice-worker path and lowercase SHA256 pins for worker and manifest; text retained\n"
				}
				if e.hs.CallCount(uploadRoute) != 0 {
					t.Fatal("failed voice uploaded")
				}
			} else if e.hs.CallCount(uploadRoute) != 1 {
				t.Fatal("intact voice pipeline did not upload")
			}
			if stderr.String() != warning {
				t.Fatalf("warning=%q want=%q", stderr.String(), warning)
			}
		})
	}
}
