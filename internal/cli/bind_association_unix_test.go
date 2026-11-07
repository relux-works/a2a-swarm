//go:build unix

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/association"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
	"golang.org/x/sys/unix"
)

type associationNoHTTP struct{}

type associationFileEvidence struct {
	Mode     os.FileMode
	Size     int64
	Modified time.Time
	Hash     [32]byte
}

// Observe only this test's own synthetic tree, never opening poison FIFOs.
// Filesystem atime is deliberately excluded: reads may update it.
func associationEffectTree(t *testing.T, root string) map[string]associationFileEvidence {
	t.Helper()
	out := map[string]associationFileEvidence{}
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		st, err := os.Lstat(path)
		if err != nil {
			return err
		}
		e := associationFileEvidence{Mode: st.Mode(), Size: st.Size(), Modified: st.ModTime()}
		if st.Mode().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			e.Hash = sha256.Sum256(b)
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[name] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

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
	switch os.Getenv("SYNTHETIC_MODE") {
	case "bad-args":
		args = append(args, "--raw-state")
	case "conflicting-profile":
		args = append([]string{"--profile=first", "--profile=second"}, args...)
	case "human":
		args = append([]string{"--human", "--as=synthetic-own"}, args...)
	case "bad-fingerprint":
		args = append(args, "--connection-fingerprint=short")
	case "strict-global":
		args = append(args, "--as=synthetic-own", "--as=synthetic-own", "--verification-input")
	}
	os.Exit(a.Run(context.Background(), args))
}

// The only invocation is a fresh subprocess with controlled synthetic state.
// Poison FIFOs prove ordinary matrix.env/credential reads cannot be on the path.
func TestAssociationCLIDispatchEffectFree(t *testing.T) {
	for _, mode := range []string{"legacy", "named", "bad-args", "missing-lock", "foreign-slug", "conflicting-profile", "human", "bad-fingerprint", "strict-global", "poison-slug"} {
		t.Run(mode, func(t *testing.T) {
			root, e := filepath.EvalSymlinks(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
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
			if mode == "poison-slug" {
				hostPath := filepath.Join(root, "host")
				if e := os.Remove(hostPath); e != nil {
					t.Fatal(e)
				}
				if e := unix.Mkfifo(hostPath, 0600); e != nil {
					t.Fatal(e)
				}
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
			binding := config.TerminalBinding{Generation: strings.Repeat("a", 32), SessionID: "SYNTHETIC_PRIVATE_SESSION", Host: machineLabel(), OwnerPID: 42001, OwnerBirth: target.OwnerBirth, DaemonPID: 41001, DaemonBirth: "SYNTHETIC_CONSUMER_BIRTH", Target: target}
			if mode == "foreign-slug" {
				binding.Host = "synthetic-host"
			}
			if machineLabel() == "synthetic-host" {
				t.Fatal("fixture must separate hostname from slug")
			}
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
			cmd.Env = append(cmd.Env, "SYNTHETIC_MODE="+mode)
			cmd.ExtraFiles = []*os.File{fd}
			effectsBefore := associationEffectTree(t, root)
			var out, errbuf bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &errbuf
			e = cmd.Run()
			if !reflect.DeepEqual(effectsBefore, associationEffectTree(t, root)) {
				t.Fatal("association changed synthetic tree bytes, mode, mtime or entries")
			}
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
			if mode == "bad-args" || mode == "conflicting-profile" || mode == "human" || mode == "bad-fingerprint" || mode == "strict-global" {
				want = "input_invalid"
				wantCode = 2
			}
			if mode == "missing-lock" {
				want = "snapshot_unavailable"
				wantCode = 2
			}
			if mode == "foreign-slug" {
				want = "stale_binding"
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
