package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/fixturecompare"
)

// The actual main/exit path runs in a child so exit codes and user-visible labels
// are tested together, without injecting a different implementation.
func TestReleaseCommandProcess(t *testing.T) {
	if os.Getenv("A2A_RELEASE_TEST_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"a2a-release"}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	os.Exit(99)
}

func releaseProcess(t *testing.T, extra map[string]string, args ...string) (string, int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self, append([]string{"-test.run=^TestReleaseCommandProcess$", "--"}, args...)...)
	clear := map[string]bool{
		"CI": true, "GITHUB_ACTIONS": true, "RUNNER_ENVIRONMENT": true, "RUNNER_TEMP": true,
		"RELEASE_ADMITTED_TAG_OBJECT": true, "RELEASE_TAG": true,
		"MACOS_SIGN_P12": true, "MACOS_SIGN_P12_PASSWORD": true, "ASC_ISSUER_ID": true, "ASC_KEY_ID": true, "ASC_KEY_P8": true,
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		_, overridden := extra[key]
		if !clear[key] && !overridden && !strings.HasPrefix(key, "A2A_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "A2A_RELEASE_TEST_PROCESS=1")
	for key, value := range extra {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	b, err := cmd.CombinedOutput()
	if err == nil {
		return string(b), 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return string(b), exit.ExitCode()
	}
	t.Fatal(err)
	return "", -1
}

// Only the distinct fixture command reaches its dispatcher; legacy release,
// compare and verify reject fixture-shaped arguments without its label.
func TestFixtureCommandRouting(t *testing.T) {
	for _, command := range []string{"release", "prepare", "verify", "compare", "admit"} {
		t.Run(command, func(t *testing.T) {
			args := []string{command, "fixture-compare"}
			if command == "compare" {
				args = append(args, "fixture-compare")
			}
			if command == "verify" || command == "compare" {
				args = append(args, filepath.Join(t.TempDir(), "evidence"))
			}
			out, code := releaseProcess(t, nil, args...)
			if code != 1 || strings.Contains(out, fixturecompare.Label) {
				t.Fatalf("legacy command reached fixture mode: code=%d %s", code, out)
			}
		})
	}
	out, code := releaseProcess(t, nil, "fixture-compare")
	if code != 1 || !strings.Contains(out, fixturecompare.Label) || !strings.Contains(out, "usage:") ||
		!strings.Contains(out, "SHA256SUMS") || !strings.Contains(out, "exactly two lowercase") || !strings.Contains(out, "two spaces") {
		t.Fatalf("distinct diagnostic usage: code=%d %s", code, out)
	}
}

// Every actual CI/release environment marker refuses before input access. The
// release scripts cannot enable the mode with "false" or an admitted tag value.
func TestFixtureCommandEnvironment(t *testing.T) {
	for _, key := range []string{"CI", "GITHUB_ACTIONS", "RUNNER_ENVIRONMENT", "RUNNER_TEMP", "RELEASE_TAG", "RELEASE_ADMITTED_TAG_OBJECT", "MACOS_SIGN_P12", "MACOS_SIGN_P12_PASSWORD", "ASC_ISSUER_ID", "ASC_KEY_ID", "ASC_KEY_P8"} {
		t.Run(key, func(t *testing.T) {
			out, code := releaseProcess(t, map[string]string{key: "fixture-marker"}, "fixture-compare", "missing-N", "missing-next", "missing-evidence")
			if code != 1 || !strings.Contains(out, "standalone environment required") || !strings.Contains(out, fixturecompare.Label) ||
				strings.Contains(out, "fixture-directory-missing") {
				t.Fatalf("release environment not fenced: code=%d %s", code, out)
			}
		})
	}
}

// Completed diagnostics always use exit 2, even if there are no findings; a
// verification failure uses 1, and ordinary successful release commands retain 0.
func TestFixtureExitClassification(t *testing.T) {
	if exitCode(nil) != 0 || exitCode(errFixtureDiagnostic) != 2 {
		t.Fatal("diagnostic became a release pass")
	}
}
