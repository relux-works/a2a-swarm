package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Proves local enrollment uses only its explicit input, preserves identity
// bytes and closes malformed input; no Daemon socket is contacted.
func TestDaemonEnrollLocalCLI(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("atomic directory publication unavailable")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".a2a")
	payload, _ := json.Marshal(map[string]string{"socket": "/run/a2a/daemon.sock", "identity": "enrolled", "station": "station-1", "secret_b64": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{31}, 32))})
	var stdout, stderr bytes.Buffer
	a := &App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader(string(payload) + "\n"), Getenv: func(string) string { t.Fatal("enrollment consulted ambient configuration"); return "" }}
	if a.Run(context.Background(), []string{"--as", "different", "daemon", "enroll", "--dir", dir, "--from", "-"}) != ExitOK {
		t.Fatal("enrollment command failed")
	}
	if stdout.String() != "Enrollment installed.\n" || stderr.Len() != 0 {
		t.Fatal("command output was not fixed success")
	}
	identity, err := os.ReadFile(filepath.Join(dir, "identity"))
	if err != nil || string(identity) != "enrolled\n" {
		t.Fatal("payload identity was substituted or malformed")
	}
	stdout.Reset()
	stderr.Reset()
	a.Stdin = strings.NewReader("{\n")
	if a.Run(context.Background(), []string{"daemon", "enroll", "--dir", dir, "--from", "-"}) != ExitError {
		t.Fatal("malformed input accepted")
	}
	if stdout.Len() != 0 || stderr.String() != "a2a: daemon enroll: invalid enrollment input\n" {
		t.Fatal("refusal was not fixed")
	}
}

// Proves enrollment usage refusals are fixed and do not echo arguments;
// successful dispatch is covered by TestDaemonEnrollLocalCLI.
func TestDaemonEnrollUsageClosed(t *testing.T) {
	for _, args := range [][]string{nil, {"--dir", "/fixture/.a2a"}, {"--dir", "/fixture/.a2a", "--from", "file"}, {"--dir", "/fixture/.a2a", "--from", "-", "extra"}, {"--secret", "do-not-echo"}} {
		var stdout, stderr bytes.Buffer
		a := &App{Stdout: &stdout, Stderr: &stderr}
		err := cmdDaemonEnroll(context.Background(), a, args)
		if err == nil || err.Error() != "usage: daemon enroll --dir HOME/.a2a --from - [--rotate]" {
			t.Fatal("usage did not fail with fixed text")
		}
		if stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatal("usage echoed arguments")
		}
	}
}
