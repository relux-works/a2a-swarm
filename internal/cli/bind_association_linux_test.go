//go:build linux

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/association"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
	"golang.org/x/sys/unix"
)

type associationNoHTTP struct{}

func (associationNoHTTP) RoundTrip(*http.Request) (*http.Response, error) {
	panic("association consulted network")
}

func TestAssociationChild(t *testing.T) {
	if os.Getenv("SYNTHETIC_ASSOCIATION_CHILD") != "1" {
		return
	}
	a := NewApp()
	a.Now = func() time.Time { return time.UnixMilli(1800000000000) }
	a.HTTP = &http.Client{Transport: associationNoHTTP{}}
	a.keychain = func(config.Paths) *keychainStore { panic("association consulted keychain") }
	args := []string{"bind", "association", "--private-intent-fd=3", "--json"}
	if os.Getenv("SYNTHETIC_BAD_ARGS") == "1" {
		args = append(args, "--raw-state")
	}
	os.Exit(a.Run(context.Background(), args))
}

func associationDescriptor(t *testing.T, b []byte) *os.File {
	t.Helper()
	fd, e := unix.MemfdCreate("synthetic-association", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if e != nil {
		t.Fatal(e)
	}
	w := os.NewFile(uintptr(fd), "synthetic-writer")
	defer w.Close()
	if e = w.Chmod(0600); e != nil {
		t.Fatal(e)
	}
	raw := make([]byte, len(b)+4)
	binary.BigEndian.PutUint32(raw, uint32(len(b)))
	copy(raw[4:], b)
	if _, e = w.Write(raw); e != nil {
		t.Fatal(e)
	}
	if _, e = unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_SEAL|unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK); e != nil {
		t.Fatal(e)
	}
	f, e := os.Open(fmt.Sprintf("/proc/self/fd/%d", fd))
	if e != nil {
		t.Fatal(e)
	}
	return f
}

// The only invocation is a fresh subprocess with controlled synthetic state.
// Poison FIFOs prove ordinary matrix.env/credential reads cannot be on the path.
func TestAssociationCLIDispatchEffectFree(t *testing.T) {
	for _, mode := range []string{"legacy", "named", "bad-args", "missing-lock"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if e := os.Chmod(root, 0700); e != nil {
				t.Fatal(e)
			}
			name := "synthetic-own"
			p := config.Paths{Base: root}
			connection := "legacy"
			if e := unix.Mkfifo(filepath.Join(root, "matrix.env"), 0600); e != nil {
				t.Fatal(e)
			}
			if mode == "named" {
				c := config.Connection{ID: "synthetic-connection", URL: "https://matrix.example.invalid", ServerName: "example.invalid", Store: config.CredentialStore{Backend: "keychain", AccessTokenService: "synthetic.token", PasswordService: "synthetic.password"}}
				r := config.ConnectionRegistry{SchemaVersion: 1, Current: "synthetic", Profiles: map[string]config.Connection{"synthetic": c}}
				b, e := json.Marshal(r)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(filepath.Join(root, "connections.json"), b, 0600); e != nil {
					t.Fatal(e)
				}
				p, e = c.Paths(p, name, false)
				if e != nil {
					t.Fatal(e)
				}
				connection = c.ID
			}
			if e := os.MkdirAll(filepath.Join(p.Base, "state"), 0700); e != nil {
				t.Fatal(e)
			}
			// MkdirAll makes every newly created component private under this own root.
			if e := os.WriteFile(filepath.Join(root, "host"), []byte("synthetic-host\n"), 0600); e != nil {
				t.Fatal(e)
			}
			metadata := filepath.Join(p.Base, "agents")
			if p.MetadataBase != "" {
				metadata = filepath.Join(p.MetadataBase, "agents")
			}
			if e := os.MkdirAll(metadata, 0700); e != nil {
				t.Fatal(e)
			}
			if e := unix.Mkfifo(filepath.Join(metadata, name+".json"), 0600); e != nil {
				t.Fatal(e)
			}
			target := tmuxwake.Target{Socket: "/synthetic-private/own/socket", SocketID: "SYNTHETIC_SOCKET_ID", Pane: "%817263", PanePID: 43001, PaneBirth: "SYNTHETIC_PANE_BIRTH", OwnerPID: 42001, OwnerBirth: "SYNTHETIC_OWNER_BIRTH", Runtime: "codex"}
			binding := config.TerminalBinding{Generation: strings.Repeat("a", 32), SessionID: "SYNTHETIC_PRIVATE_SESSION", Host: "synthetic-host", OwnerPID: 42001, OwnerBirth: target.OwnerBirth, DaemonPID: 41001, DaemonBirth: "SYNTHETIC_CONSUMER_BIRTH", Target: target}
			state := map[string]any{"terminal": binding, "lease": map[string]any{"holder": binding.SessionID, "expires": time.UnixMilli(1800000060000).Format(time.RFC3339Nano)}, "principal_coverage": map[string]any{"retained": true}, "host_enrollment": map[string]any{"uninterpreted": true}, "notification": map[string]any{"retained": true}, "hooks_paused": true, "wake": map[string]any{"unknown": "retained"}}
			before, e := json.Marshal(state)
			if e != nil {
				t.Fatal(e)
			}
			statePath := p.StateFile(name)
			if e = os.WriteFile(statePath, before, 0600); e != nil {
				t.Fatal(e)
			}
			lockPath := filepath.Join(p.Base, "state", name+".lock")
			if mode != "missing-lock" {
				if e = os.WriteFile(lockPath, nil, 0600); e != nil {
					t.Fatal(e)
				}
			}
			sum := sha256.Sum256([]byte("complete synthetic launch intent bytes and all options"))
			req := map[string]any{"schema": association.Schema, "source_identity": name, "source_connection": connection, "launch_intent_sha256": hex.EncodeToString(sum[:]), "issued_at_ms": 1799999999999, "expires_at_ms": 1800000029999, "expected": map[string]any{"generation": binding.Generation, "consumer_pid": binding.DaemonPID, "consumer_birth": binding.DaemonBirth, "owner_pid": binding.OwnerPID, "owner_birth": binding.OwnerBirth, "runtime": target.Runtime, "session_id": binding.SessionID, "target": target}}
			body, e := json.Marshal(req)
			if e != nil {
				t.Fatal(e)
			}
			fd := associationDescriptor(t, body)
			defer fd.Close()
			exe, e := os.Executable()
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestAssociationChild$")
			cmd.Env = []string{"SYNTHETIC_ASSOCIATION_CHILD=1", "A2A_AGENT=" + name, "A2A_CONFIG_DIR=" + root, "HOME=" + root, "TMPDIR=" + root, "PATH=/usr/bin:/bin"}
			if mode == "bad-args" {
				cmd.Env = append(cmd.Env, "SYNTHETIC_BAD_ARGS=1")
			}
			cmd.ExtraFiles = []*os.File{fd}
			var out, errbuf bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &errbuf
			e = cmd.Run()
			code := 0
			if e != nil {
				if exit, ok := e.(*exec.ExitError); ok {
					code = exit.ExitCode()
				} else {
					t.Fatal(e)
				}
			}
			if ctx.Err() != nil {
				t.Fatal("command read a poison FIFO or exceeded synthetic subprocess bound")
			}
			if errbuf.Len() != 0 {
				t.Fatalf("unexpected stderr %q", errbuf.String())
			}
			var got struct {
				Result, Reason string
				Stored         any    `json:"stored"`
				Native         string `json:"native_evidence"`
			}
			if e = json.Unmarshal(out.Bytes(), &got); e != nil {
				t.Fatal(e)
			}
			want := "stored_tuple_equal"
			wantCode := 0
			if mode == "bad-args" {
				want = "input_invalid"
				wantCode = 2
			}
			if mode == "missing-lock" {
				want = "snapshot_unavailable"
				wantCode = 2
			}
			if code != wantCode || got.Reason != want {
				t.Fatalf("got %d/%s want %d/%s", code, got.Reason, wantCode, want)
			}
			if got.Native != "not-observed-by-this-interface" {
				t.Fatal("native claim")
			}
			after, e := os.ReadFile(statePath)
			if e != nil || !bytes.Equal(before, after) {
				t.Fatal("state effect")
			}
			if mode == "missing-lock" {
				if _, e = os.Lstat(lockPath); !os.IsNotExist(e) {
					t.Fatal("lock created")
				}
			}
			for _, v := range []string{binding.SessionID, binding.DaemonBirth, binding.OwnerBirth, target.Socket, target.SocketID, target.Pane, target.PaneBirth, name, connection} {
				if len(v) > 6 && strings.Contains(out.String(), v) {
					t.Fatal("private/context output")
				}
			}
		})
	}
}
