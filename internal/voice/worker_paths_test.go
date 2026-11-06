package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The child runs the same path boundary as the native constructor. It never
// links or calls Sherpa, downloads a model, or reads live configuration.
func TestWorkerPathsProcess(t *testing.T) {
	if os.Getenv("A2A_WORKER_PATH_FIXTURE") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 4 {
		os.Exit(90)
	}
	code := withWorkerModel(args[1], args[2], args[3], func(p workerPaths) int {
		if os.Getenv("A2A_WORKER_PATH_CONTROL") == "1" {
			if _, err := os.Stderr.WriteString("inert path callback entered\n"); err != nil {
				t.Fatal(err)
			}
		}
		if p.data != "espeak-ng-data" || filepath.IsAbs(p.model) || filepath.IsAbs(p.tokens) {
			t.Fatalf("native DataDir must stay relative and 14 bytes; got %d bytes", len(p.data))
		}
		// Simulate source-path replacement after validation. Relative paths must
		// keep using the directory entered by the worker, not its old pathname.
		if os.Getenv("A2A_WORKER_PATH_RENAME") == "1" {
			if err := os.Rename(args[1], args[1]+".retired"); err != nil {
				t.Fatal(err)
			}
			workerModelFixture(t, args[1])
			if err := os.WriteFile(filepath.Join(args[1], "tokens.txt"), []byte("foreign replacement"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		for path, want := range map[string]string{p.model: "model fixture", p.tokens: "tokens fixture", filepath.Join(p.data, "phontab"): "espeak fixture"} {
			b, err := os.ReadFile(path)
			if err != nil || string(b) != want {
				t.Fatalf("native paths lost authenticated fixture: %v", err)
			}
		}
		if !filepath.IsAbs(p.output) {
			t.Fatal("output must be absolute before changing cwd")
		}
		if os.Getenv("A2A_WORKER_PATH_EXIT") == "1" {
			os.Exit(7) // native exit bypasses Go defers; no alias to leak
		}
		if os.Getenv("A2A_WORKER_PATH_WAIT") == "1" {
			// Signal that validation completed and cwd is held. The parent can
			// overlap two inference lifetimes or cancel this process exactly here.
			if _, err := os.Stdout.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(os.Stdin, make([]byte, 1)); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(p.output, []byte("inert inference"), 0600); err != nil {
			t.Fatal(err)
		}
		return 0
	})
	os.Exit(code)
}

// Valid neighbors produce the exact inert output; refused roots, pins and assets
// exit before inference, while native exit retains its distinct process status.
func TestWorkerPathsControls(t *testing.T) {
	for _, variant := range []string{"short", "relative", "multibyte", "physical-parent", "root-link", "data-link", "changed-file", "changed-manifest", "wrong-pin", "empty-root", "file-root", "missing", "rename", "native-exit"} {
		t.Run(variant, func(t *testing.T) {
			parent := t.TempDir()
			model := filepath.Join(parent, "model")
			if variant == "multibyte" {
				model = filepath.Join(parent, strings.Repeat("я", 100), "model")
			}
			pin := workerModelFixture(t, model)
			out := filepath.Join(parent, "out.wav")
			modelArg, outArg := model, out
			success := true
			switch variant {
			case "relative":
				modelArg, outArg = "model", "out.wav"
			case "physical-parent", "root-link":
				link := filepath.Join(parent, "link")
				target := parent
				if variant == "root-link" {
					target = model
					success = false
				}
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				modelArg = link
				if variant == "physical-parent" {
					modelArg = filepath.Join(link, "model")
				}
			case "data-link":
				data := filepath.Join(model, "espeak-ng-data")
				if err := os.Rename(data, data+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(data+".old", data); err != nil {
					t.Fatal(err)
				}
				success = false
			case "changed-file":
				if err := os.WriteFile(filepath.Join(model, "tokens.txt"), []byte("TOKENS fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				success = false
			case "changed-manifest":
				if err := os.WriteFile(filepath.Join(model, "manifest.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
				success = false
			case "missing":
				if err := os.Remove(filepath.Join(model, "espeak-ng-data/phontab")); err != nil {
					t.Fatal(err)
				}
				success = false
			case "wrong-pin":
				pin = strings.Repeat("0", 64)
				success = false
			case "empty-root", "file-root":
				if err := os.RemoveAll(model); err != nil {
					t.Fatal(err)
				}
				if variant == "empty-root" {
					if err := os.Mkdir(model, 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(model, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
				success = false
			case "native-exit":
				success = false
			}
			cmd := workerFixtureCommand(t, parent, modelArg, outArg, pin)
			cmd.Env = append(cmd.Env, "A2A_WORKER_PATH_CONTROL=1")
			if variant == "rename" {
				cmd.Env = append(cmd.Env, "A2A_WORKER_PATH_RENAME=1")
			}
			if variant == "native-exit" {
				cmd.Env = append(cmd.Env, "A2A_WORKER_PATH_EXIT=1")
			}
			b, err := cmd.CombinedOutput()
			if (err == nil) != success {
				t.Fatalf("success=%v error=%v\n%s", success, err, b)
			}
			if !success {
				if variant != "native-exit" && bytes.Contains(b, []byte("inert path callback entered\n")) {
					t.Fatalf("refused model reached inference\n%s", b)
				}
				exit, ok := err.(*exec.ExitError)
				wantExit := 1
				if variant == "native-exit" {
					wantExit = 7
				}
				if !ok || exit.ExitCode() != wantExit {
					t.Fatalf("wrong refusal: want exit %d, got %v\n%s", wantExit, err, b)
				}
				if _, err := os.Stat(out); !os.IsNotExist(err) {
					t.Fatal("refused worker reached inference/output")
				}
			} else if b, err := os.ReadFile(out); err != nil || string(b) != "inert inference" {
				t.Fatalf("valid control output=%q error=%v", b, err)
			}
			// Preparation creates no staging files or aliases, even on exit(7).
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.Name() != "model" && e.Name() != "model.retired" && e.Name() != "out.wav" && e.Name() != "link" && e.Name() != strings.Repeat("я", 100) {
					t.Fatalf("unexpected retained artifact %q", e.Name())
				}
			}
		})
	}
}

// Two actual children hold authenticated cwd concurrently without moving the
// parent cwd or changing the installed assets, and both finish their own output.
func TestWorkerPathsConcurrent(t *testing.T) {
	parent := t.TempDir()
	model := filepath.Join(parent, "model")
	pin := workerModelFixture(t, model)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	first := workerFixtureCommand(t, parent, model, filepath.Join(parent, "first.wav"), pin)
	second := workerFixtureCommand(t, parent, model, filepath.Join(parent, "notice.wav"), pin)
	writers := make([]io.WriteCloser, 0, 2)
	for _, cmd := range []*exec.Cmd{first, second} {
		cmd.Env = append(cmd.Env, "A2A_WORKER_PATH_WAIT=1")
		writers = append(writers, startWorkerFixtureWaiting(t, cmd))
	}
	// Both children have entered/validated the model before either may finish.
	for _, w := range writers {
		if _, err := w.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		w.Close()
	}
	for _, cmd := range []*exec.Cmd{first, second} {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(cmd.Args[len(cmd.Args)-2]); err != nil || string(b) != "inert inference" {
			t.Fatalf("concurrent worker output=%q error=%v", b, err)
		}
	}
	if after, err := os.Getwd(); err != nil || after != cwd {
		t.Fatal("worker changed parent cwd")
	}
	if err := validateModel(context.Background(), model, pin); err != nil {
		t.Fatalf("workers altered installed model: %v", err)
	}
}

func startWorkerFixtureWaiting(t *testing.T, cmd *exec.Cmd) io.WriteCloser {
	t.Helper()
	w, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	r, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	ready := make([]byte, 1)
	if _, err := io.ReadFull(r, ready); err != nil || ready[0] != 1 {
		t.Fatalf("worker did not reach inference: %v", err)
	}
	return w
}

// Cancellation after the actual child enters inference kills and reaps it,
// produces no output or staging artifacts, and preserves the installed assets.
func TestWorkerPathsCancellation(t *testing.T) {
	parent := t.TempDir()
	model := filepath.Join(parent, "model")
	pin := workerModelFixture(t, model)
	out := filepath.Join(parent, "out.wav")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := workerFixtureCommand(t, parent, model, out, pin)
	// Reuse the bounded fixture command's arguments/environment, with an exact
	// cancellation point after entering the authenticated model directory.
	cancelled := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	cancelled.Dir, cancelled.Env = cmd.Dir, append(cmd.Env, "A2A_WORKER_PATH_WAIT=1")
	startWorkerFixtureWaiting(t, cancelled)
	cancel()
	if err := cancelled.Wait(); err == nil {
		t.Fatal("cancelled worker succeeded")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("cancelled worker produced output")
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 || entries[0].Name() != "model" {
		t.Fatal("cancellation retained staging or changed model")
	}
	if err := validateModel(context.Background(), model, pin); err != nil {
		t.Fatal(err)
	}
}

func workerModelFixture(t *testing.T, root string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "espeak-ng-data"), 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for name, content := range map[string]string{"ru_RU-irina-medium.onnx": "model fixture", "tokens.txt": "tokens fixture", "espeak-ng-data/phontab": "espeak fixture"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		files[name] = digest([]byte(content))
	}
	manifest, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	return digest(manifest)
}

func workerFixtureCommand(t *testing.T, cwd, model, output, pin string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWorkerPathsProcess$", "--", model, output, pin)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "A2A_WORKER_PATH_FIXTURE=1")
	return cmd
}

// The original per-connection DataDir exceeds the reported 257-byte boundary;
// the real child still opens pinned assets and writes outside the model tree.
func TestWorkerPathsLongBase(t *testing.T) {
	parent := t.TempDir()
	base := filepath.Join(parent, strings.Repeat("c", 100), "connections", "relux", "identities", "agents", strings.Repeat("a", 64))
	model := ModelDir(base)
	pin := workerModelFixture(t, model)
	if n := len(filepath.Join(model, "espeak-ng-data")); n < 257 {
		t.Fatalf("witness did not reach reported overflow: %d bytes", n)
	} else {
		t.Logf("original DataDir: %d bytes; required worker spelling: 14 bytes", n)
	}
	out := filepath.Join(parent, "out.wav")
	if b, err := workerFixtureCommand(t, parent, model, out, pin).CombinedOutput(); err != nil {
		t.Fatalf("long-path constructor boundary failed: %v\n%s", err, b)
	}
	if b, err := os.ReadFile(out); err != nil || string(b) != "inert inference" {
		t.Fatalf("output=%q error=%v", b, err)
	}
}

// Only encoding is faked here; ordinary and notice synthesis enter the real
// worker path preparation in separate children against an authenticated fixture.
type workerPathRunner struct {
	*renderRunner
	pin    string
	failAt int
}

func (r *workerPathRunner) ValidateModel(ctx context.Context, base string) error {
	return validateModel(ctx, ModelDir(base), r.pin)
}

func (r *workerPathRunner) Run(ctx context.Context, name string, args []string, input io.Reader) error {
	if input == nil {
		return r.renderRunner.Run(ctx, name, args, input)
	}
	b, err := io.ReadAll(input)
	if err != nil {
		return err
	}
	r.inputs = append(r.inputs, string(b))
	out := args[len(args)-1]
	r.paths = append(r.paths, out)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWorkerPathsProcess$", "--", args[1], out, r.pin)
	cmd.Env = append(os.Environ(), "A2A_WORKER_PATH_FIXTURE=1")
	if len(r.inputs) == r.failAt {
		cmd.Env = append(cmd.Env, "A2A_WORKER_PATH_EXIT=1")
	}
	return cmd.Run()
}

func TestRenderWorkerPaths(t *testing.T) {
	for _, failAt := range []int{0, 1, 2} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			base := filepath.Join(t.TempDir(), strings.Repeat("a", 100), "connections", "relux", "identities", "agents", strings.Repeat("b", 64))
			r := &workerPathRunner{renderRunner: &renderRunner{t: t, source: bytes.Repeat([]byte{1, 0}, sampleRate)}, pin: workerModelFixture(t, ModelDir(base)), failAt: failAt}
			a, err := (LocalRenderer{Runner: r}).Render(context.Background(), base, Config{true, 180}, strings.Repeat("я", 6001))
			if failAt == 0 {
				if err != nil || !a.Truncated || len(r.inputs) != 2 || r.inputs[1] != TruncationNotice {
					t.Fatalf("ordinary/notice worker result: truncated=%v error=%v calls=%d", a.Truncated, err, len(r.inputs))
				}
			} else {
				// This fixture runner returns a raw process error, which #171 keeps
				// unclassified. Categories have their own tests in failure_test.go.
				want := "voice synthesis failed: unclassified failure"
				if err == nil || err.Error() != want || len(r.inputs) != failAt || len(a.OGG) != 0 {
					t.Fatalf("failure contract: error=%v calls=%d", err, len(r.inputs))
				}
			}
			for _, path := range r.paths {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("retained render scratch")
				}
			}
			if err := validateModel(context.Background(), ModelDir(base), r.pin); err != nil {
				t.Fatal(err)
			}
		})
	}
}
