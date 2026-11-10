//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/daemon"
	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

type daemonHTTPTrap struct{}

func (daemonHTTPTrap) RoundTrip(*http.Request) (*http.Response, error) {
	panic("Daemon reached Matrix/HTTP")
}

type daemonSecretTrap struct{}

func (daemonSecretTrap) Read(context.Context, config.Paths, config.SecretReference) (string, error) {
	panic("Daemon reached credential provider")
}
func (daemonSecretTrap) Create(context.Context, config.Paths, config.SecretReference, string) error {
	panic("Daemon reached credential provider")
}

func localDaemonApp(out, stderr io.Writer) *App {
	return &App{Stdout: out, Stderr: stderr, Stdin: strings.NewReader(""), Getenv: func(string) string { panic("Daemon read identity/config environment") }, HTTP: &http.Client{Transport: daemonHTTPTrap{}}, connectionSecretStore: daemonSecretTrap{}, keychain: func(config.Paths) *keychainStore { panic("Daemon reached Keychain") }}
}

func daemonHome(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(p, "daemon")
}

func TestDaemonPipeRegistrationAndRemovalLocalOnly(t *testing.T) {
	home := daemonHome(t)
	var output, diagnostics bytes.Buffer
	args := []string{"daemon", "register-station", "--home", home, "--station", "station", "--identity", "local.name_=-", "--uid", strconv.Itoa(os.Geteuid()), "--emit", "-"}
	run := func() []byte {
		output.Reset()
		diagnostics.Reset()
		if code := localDaemonApp(&output, &diagnostics).Run(context.Background(), args); code != ExitOK {
			t.Fatal("local registration failed")
		}
		if diagnostics.Len() != 0 || bytes.Count(output.Bytes(), []byte{'\n'}) != 1 || !bytes.HasSuffix(output.Bytes(), []byte{'\n'}) {
			t.Fatal("enrollment is not one closed JSON line")
		}
		fields := map[string]string{}
		if json.Unmarshal(output.Bytes(), &fields) != nil || len(fields) != 4 {
			t.Fatal("enrollment schema invalid")
		}
		if fields["station"] != "station" || fields["identity"] != "local.name_=-" || fields["socket"] != daemon.DefaultSocket() {
			t.Fatal("enrollment binding differs")
		}
		secret, err := base64.StdEncoding.Strict().DecodeString(fields["secret_b64"])
		if err != nil || len(secret) != 32 {
			t.Fatal("invalid enrollment key")
		}
		// Inspect in memory only. Never log or snapshot the payload/registry bytes.
		data, err := os.ReadFile(filepath.Join(home, "stations.json"))
		if err != nil {
			t.Fatal(err)
		}
		var registry map[string]struct {
			Identity string
			UID      uint32
			Secret   []byte
		}
		if json.Unmarshal(data, &registry) != nil || !bytes.Equal(registry["station"].Secret, secret) {
			t.Fatal("registry did not retain original key")
		}
		return secret
	}
	first := run()
	args = append(args, "--rotate")
	second := run()
	if bytes.Equal(first, second) {
		t.Fatal("register did not rotate")
	}
	for _, path := range []string{home, filepath.Join(home, "stations.json")} {
		st, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0600)
		if st.IsDir() {
			mode = 0700
		}
		if st.Mode().Perm() != mode {
			t.Fatal("registry custody mode differs")
		}
	}
	output.Reset()
	diagnostics.Reset()
	if code := localDaemonApp(&output, &diagnostics).Run(context.Background(), []string{"--profile", "unused", "daemon", "unregister-station", "--home", home, "--station", "station"}); code != ExitOK {
		t.Fatal("local removal failed")
	}
	if output.Len() != 0 || diagnostics.Len() != 0 {
		t.Fatal("removal produced unexpected output")
	}
	data, err := os.ReadFile(filepath.Join(home, "stations.json"))
	if err != nil || strings.TrimSpace(string(data)) != "{}" {
		t.Fatal("binding not removed")
	}
}

func TestDaemonUsageAndStartupNeverTouchCredentials(t *testing.T) {
	home := daemonHome(t)
	for _, args := range [][]string{
		{"daemon", "--help"}, {"daemon", "serve", "--help"}, {"daemon", "register-station", "--help"},
		{"daemon"}, {"daemon", "engine"}, {"daemon", "enroll"}, {"daemon", "migrate"},
		{"daemon", "serve", "--home", home}, // absent registry is a closed local refusal
		{"daemon", "serve", "--home", "relative"},
		{"daemon", "register-station", "--home", home, "--station", "s", "--identity", "i", "--uid", "0", "--secret-out", "forbidden"},
		{"daemon", "register-station", "--home", home, "--station", "s", "--identity", "i", "--uid", "0"},
		{"daemon", "register-station", "--home", home, "--station", "s", "--identity", "i", "--uid", "-1", "--emit", "-"},
		{"daemon", "register-station", "--home", home, "--station", "s", "--identity", "i", "--uid", "4294967296", "--emit", "-"},
		{"daemon", "register-station", "--home", home, "--station", "s", "--identity", "i", "--uid", "00", "--emit", "-"},
		{"daemon", "register-station", "--home", home, "--station", "s", "--identity", "i", "--uid", "0", "--emit", "file"},
	} {
		var out, diag bytes.Buffer
		code := localDaemonApp(&out, &diag).Run(context.Background(), args)
		if args[len(args)-1] == "--help" {
			if code != ExitOK {
				t.Fatal("help refused")
			}
		} else if code == ExitOK {
			t.Fatal("invalid invocation succeeded")
		}
		tuitestkit.NotContainsStr(t, diag.String(), "forbidden")
		if args[len(args)-1] != "--help" && out.Len() != 0 {
			t.Fatal("invalid invocation emitted data")
		}
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("invalid usage created registry")
	}
}

type daemonBrokenOutput struct{}

func (daemonBrokenOutput) Write(p []byte) (int, error) {
	return 0, errors.New("untrusted writer text")
}
func TestDaemonOutputFailureIsClosed(t *testing.T) {
	home := daemonHome(t)
	var diag bytes.Buffer
	code := localDaemonApp(daemonBrokenOutput{}, &diag).Run(context.Background(), []string{"daemon", "register-station", "--home", home, "--station", "s", "--identity", "i", "--uid", strconv.Itoa(os.Geteuid()), "--emit", "-"})
	if code != ExitError {
		t.Fatal("output error was hidden")
	}
	tuitestkit.NotContainsStr(t, diag.String(), "untrusted writer text")
	tuitestkit.ContainsStr(t, diag.String(), "registration may have committed")
	if _, err := os.Stat(filepath.Join(home, "stations.json")); err != nil {
		t.Fatal("committed registration disappeared")
	}
}

func TestDaemonOSSocketDefault(t *testing.T) {
	expected := "/run/a2a/daemon.sock"
	if runtime.GOOS == "darwin" {
		expected = "/private/var/run/a2a/daemon.sock"
	}
	if daemon.DefaultSocket() != expected {
		t.Fatal("wrong OS default socket")
	}
}

func TestDaemonV1PathAndRotationCLI(t *testing.T) {
	home := daemonHome(t)
	var out, diag bytes.Buffer
	args := []string{"daemon", "register-station", "--home", home, "--station", "s", "--identity", "i", "--uid", strconv.Itoa(os.Geteuid()), "--emit", "-"}
	if localDaemonApp(&out, &diag).Run(context.Background(), args) != ExitOK {
		t.Fatal("initial registration failed")
	}
	original := append([]byte{}, out.Bytes()...)
	out.Reset()
	diag.Reset()
	if localDaemonApp(&out, &diag).Run(context.Background(), args) != ExitError || out.Len() != 0 {
		t.Fatal("CLI implicitly rotated existing binding")
	}
	if bytes.Contains(diag.Bytes(), original) {
		t.Fatal("refusal echoed enrollment")
	}
	out.Reset()
	diag.Reset()
	if localDaemonApp(&out, &diag).Run(context.Background(), append(args, "--rotate")) != ExitOK {
		t.Fatal("explicit CLI rotation failed")
	}
	// Path validation happens before a new binding is committed or emitted.
	for _, path := range []string{"/" + strings.Repeat("a", 103), "/" + strings.Repeat("é", 52), "/a\r", "/a\n", "/a\x00", "/\uFEFFa", "/\xff"} {
		out.Reset()
		diag.Reset()
		if localDaemonApp(&out, &diag).Run(context.Background(), append(args, "--rotate", "--socket", path)) != ExitUsage || out.Len() != 0 {
			t.Fatal("bad v1 path was emitted")
		}
	}
	out.Reset()
	diag.Reset()
	if localDaemonApp(&out, &diag).Run(context.Background(), append(args, "--rotate", "--socket", "/"+strings.Repeat("a", 102))) != ExitOK {
		t.Fatal("103-byte socket path refused")
	}
	if bytes.Count(out.Bytes(), []byte{'\n'}) != 1 || !bytes.HasSuffix(out.Bytes(), []byte{'\n'}) {
		t.Fatal("v1 emission missing closed LF-terminated line")
	}
	var payload map[string]string
	if json.Unmarshal(out.Bytes(), &payload) != nil || len(payload) != 4 {
		t.Fatal("v1 emission schema differs")
	}
	if daemon.ValidateSocketPath(payload["socket"]) != nil {
		t.Fatal("emitted path not valid v1 bytes")
	}
}
