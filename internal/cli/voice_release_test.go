package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/voice"
)

// Complete helper pins round-trip locally; a partial update refuses and keeps
// the previous config byte-for-byte without contacting Matrix.
func TestVoiceConfigHelperPins(t *testing.T) {
	e := newEnv(t)
	c := cliVoiceRelease(t)
	e.run("voice", "config", "--enable", "--worker", c.Worker, "--worker-sha256", c.WorkerSHA256, "--manifest-sha256", c.ManifestSHA256).ok(t)
	got, err := voice.Load(e.dir)
	if err != nil || got != c {
		t.Fatalf("pins=%+v err=%v", got, err)
	}
	before, err := os.ReadFile(voice.ConfigPath(e.dir))
	if err != nil {
		t.Fatal(err)
	}
	e.run("voice", "config", "--enable", "--worker", c.Worker).failsWith(t, ExitUsage, "SHA256 pins")
	after, err := os.ReadFile(voice.ConfigPath(e.dir))
	if err != nil || !bytes.Equal(before, after) || len(e.hs.Calls()) != 0 {
		t.Fatal("partial update changed config or used network")
	}
}

type encoderFailureRunner struct {
	noOpus bool
	calls  int
}

func (*encoderFailureRunner) ValidateModel(context.Context, string) error { return nil }
func (*encoderFailureRunner) PrepareWorker(context.Context, voice.Config) (voice.PreparedWorker, error) {
	return voice.PreparedWorker{Path: "fixture-worker"}, nil
}
func (r *encoderFailureRunner) Run(_ context.Context, name string, args []string, _ io.Reader) error {
	r.calls++
	out := args[len(args)-1]
	if name == "ffmpeg" {
		if !r.noOpus {
			return exec.ErrNotFound
		}
		if strings.HasSuffix(out, ".ogg") {
			return errors.New("encoder libopus unavailable")
		}
		return os.WriteFile(out, bytes.Repeat([]byte{1, 0}, 24000), 0600)
	}
	return os.WriteFile(out, []byte("synth fixture"), 0600)
}

// Actual renderer decode/encode refusal with absent ffmpeg or missing libopus
// keeps exactly the original text event and exit0, with no media upload.
func TestPostVoiceEncoderUnavailable(t *testing.T) {
	for _, noOpus := range []bool{false, true} {
		t.Run(fmt.Sprint(noOpus), func(t *testing.T) {
			e, _, _ := voiceWorld(t)
			runner := &encoderFailureRunner{noOpus: noOpus}
			e.configure = func(a *App) { a.voiceRenderer = voice.LocalRenderer{Runner: runner} }
			r := e.as("alice", "post", "proj", "--topic", "alpha", "--voice", "Complete original text").ok(t)
			ev, ok := e.hs.eventByID(strings.TrimSpace(r.stdout))
			if !ok || ev.Message().MsgType != "m.text" || !strings.HasSuffix(ev.Message().Body, "Complete original text") || e.hs.CallCount(uploadRoute) != 0 {
				t.Fatal("encoder failure lost text or uploaded media")
			}
			want := "voice decode failed (ffmpeg with Opus support required)"
			calls := 2
			if noOpus {
				want = "voice encode failed (ffmpeg with libopus required)"
				calls = 3
			}
			if r.stderr != "a2a: warning [VOICE_TEXT_ONLY]: "+want+"; text retained\n" || runner.calls != calls {
				t.Fatalf("wrong encoder stage: calls=%d warning=%q", runner.calls, r.stderr)
			}
		})
	}
}

func cliVoiceRelease(t *testing.T) voice.Config {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "lib"), 0700); err != nil {
		t.Fatal(err)
	}
	var manifest strings.Builder
	workerSHA := ""
	for _, name := range voice.ReleaseFiles() {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.WriteFile(p, []byte("release fixture"), 0700); err != nil {
			t.Fatal(err)
		}
		sum, err := voice.HashReleaseFile(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		if name == voice.WorkerName {
			workerSHA = sum
		}
		fmt.Fprintf(&manifest, "%s  %s\n", sum, name)
	}
	p := filepath.Join(root, voice.ManifestName)
	if err := os.WriteFile(p, []byte(manifest.String()), 0600); err != nil {
		t.Fatal(err)
	}
	sum, err := voice.HashReleaseFile(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return voice.Config{Enabled: true, MaxSeconds: 180, Worker: filepath.Join(root, voice.WorkerName), WorkerSHA256: workerSHA, ManifestSHA256: sum}
}

// The real default CLI builds without CGO/Sherpa dependencies (also under the
// sherpa tag), and posts identical text/exit0 despite missing/tampered payloads.
func TestPostVoiceSharedBinaryNativeFree(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, target := range []string{"darwin", "linux"} {
		for _, cgo := range []string{"0", "1"} {
			for _, tags := range []string{"", "sherpa"} {
				cmd := exec.CommandContext(ctx, "go", "list", "-deps", "-json", "-tags", tags, "../../cmd/a2a")
				cmd.Env = append(os.Environ(), "GOOS="+target, "CGO_ENABLED="+cgo)
				b, err := cmd.Output()
				if err != nil {
					t.Fatalf("dependency evidence GOOS=%s CGO=%s tags=%s: %v", target, cgo, tags, err)
				}
				if err := checkVoiceDependencies(b, cgo == "0"); err != nil {
					t.Fatalf("GOOS=%s CGO=%s tags=%s: %v", target, cgo, tags, err)
				}
			}
		}
	}
	exe := filepath.Join(t.TempDir(), "a2a")
	build := exec.CommandContext(ctx, "go", "build", "-o", exe, "../../cmd/a2a")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("CGO-disabled shared build: %v %s", err, b)
	}

	for _, variant := range []string{"no-flag", "disabled", "helper-missing", "helper-changed", "library-missing", "library-changed", "model-missing"} {
		t.Run(variant, func(t *testing.T) {
			e, _, _ := voiceWorld(t)
			c := cliVoiceRelease(t)
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			switch variant {
			case "no-flag", "disabled":
				must(os.Remove(voice.ConfigPath(e.dir)))
			case "helper-missing":
				must(os.Remove(c.Worker))
			case "helper-changed":
				must(os.WriteFile(c.Worker, []byte("tampered"), 0700))
			case "library-missing":
				must(os.Remove(filepath.Join(filepath.Dir(c.Worker), "lib", "libonnxruntime.dylib")))
			case "library-changed":
				must(os.WriteFile(filepath.Join(filepath.Dir(c.Worker), "lib", "libonnxruntime.dylib"), []byte("tampered"), 0600))
			}
			if variant != "no-flag" && variant != "disabled" {
				must(voice.Save(e.dir, c))
			}
			// A missing model prevents the otherwise intact fixture's execution;
			// real process failure is independently covered by LocalRunner tests.
			args := []string{"--as", "alice", "post", "proj", "--topic", "alpha"}
			if variant != "no-flag" {
				args = append(args, "--voice")
			}
			args = append(args, "Полный текст сохранён")
			cmd := exec.CommandContext(ctx, exe, args...)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "A2A_CONFIG_DIR=" + e.dir, "A2A_SECRET_STORE=file"}
			var out, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("text exit changed: %v %s", err, &stderr)
			}
			ev, ok := e.hs.eventByID(strings.TrimSpace(out.String()))
			if !ok || ev.Message().MsgType != "m.text" || !strings.HasSuffix(ev.Message().Body, "Полный текст сохранён") {
				t.Fatalf("lost text: %s", &out)
			}
			if e.hs.CallCount(uploadRoute) != 0 {
				t.Fatal("failed voice uploaded")
			}
			if variant == "no-flag" {
				if stderr.Len() != 0 {
					t.Fatal(stderr.String())
				}
			} else {
				want := "a2a: warning [VOICE_TEXT_ONLY]: voice synthesis failed or timed out; check the local engine and installed model; text retained\n"
				if variant == "disabled" {
					want = "a2a: warning [VOICE_TEXT_ONLY]: missing voice config; run a2a voice config --enable; text retained\n"
				}
				if stderr.String() != want {
					t.Fatalf("warning=%q want=%q", stderr.String(), want)
				}
			}
		})
	}
}

// CGO-disabled builds must have no CGO at all. CGO-enabled dependency inspection
// permits standard-library libc support while always excluding native voice.
func checkVoiceDependencies(b []byte, noCGO bool) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	for {
		var pkg struct {
			ImportPath string
			CgoFiles   []string
		}
		err := dec.Decode(&pkg)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if strings.Contains(pkg.ImportPath, "sherpa") || strings.Contains(pkg.ImportPath, "onnxruntime") || pkg.ImportPath == "github.com/relux-works/a2a-swarm/internal/voiceworker" || noCGO && (len(pkg.CgoFiles) != 0 || pkg.ImportPath == "runtime/cgo") {
			return fmt.Errorf("native dependency: %+v", pkg)
		}
	}
}

// Linux libc net/user support is legitimate; native voice imports remain
// forbidden in both dependency modes, including an accidentally tagged import.
func TestVoiceDependencyCompatibility(t *testing.T) {
	for _, pkg := range []string{"net", "os/user", "runtime/cgo", "github.com/k2-fsa/sherpa-onnx-go-macos", "example/onnxruntime", "github.com/relux-works/a2a-swarm/internal/voiceworker"} {
		b, err := json.Marshal(map[string]any{"ImportPath": pkg, "CgoFiles": []string{"native.cgo.go"}})
		if err != nil {
			t.Fatal(err)
		}
		native := strings.Contains(pkg, "sherpa") || strings.Contains(pkg, "onnxruntime") || strings.HasSuffix(pkg, "/voiceworker")
		if err := checkVoiceDependencies(b, false); (err != nil) != native {
			t.Fatalf("CGO-enabled %s: %v", pkg, err)
		}
		if err := checkVoiceDependencies(b, true); err == nil {
			t.Fatalf("CGO-disabled accepted %s", pkg)
		}
	}
}
