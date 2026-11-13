//go:build linux || darwin

package wakemute_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/wakemute"
	"github.com/relux-works/a2a-swarm/internal/wakemute/fixture"
)

// Real PTY descriptors supply the adjacent positive. Pipes, regular redirected
// files and every fd/consent mismatch refuse without creating active records.
// These are synthetic fixture terminals, not a human-presence attestation.
func TestScopeMuteActualTTYAndConfirmation(t *testing.T) {
	for _, kind := range []string{"valid", "stdin-pipe", "stdout-pipe", "stdin-redirect", "stdout-redirect", "tmux", "screen", "decline", "eof", "wrong-id", "wrong-host", "wrong-root", "wrong-connection", "reused", "timeout", "replaced-root", "changed-after-confirm"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("TMUX", "")
			t.Setenv("STY", "")
			root := filepath.Join(t.TempDir(), "root")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			scope := fixture.Scope(root)
			fixture.Enroll(t, scope)
			op, err := wakemute.NewOperation(scope, "consent")
			if err != nil {
				t.Fatal(err)
			}
			master, slave := fixture.PTY(t)
			in, out := slave, slave
			switch kind {
			case "stdin-pipe":
				r, w, _ := os.Pipe()
				defer r.Close()
				defer w.Close()
				in = r
				io.WriteString(w, wakemute.Challenge(op, "mute")+"\n")
			case "stdout-pipe":
				r, w, _ := os.Pipe()
				defer r.Close()
				defer w.Close()
				out = w
				io.WriteString(master, wakemute.Challenge(op, "mute")+"\n")
			case "stdin-redirect", "stdout-redirect":
				f, e := os.CreateTemp(t.TempDir(), "redirect")
				if e != nil {
					t.Fatal(e)
				}
				defer f.Close()
				if kind == "stdin-redirect" {
					in = f
					io.WriteString(f, wakemute.Challenge(op, "mute")+"\n")
					f.Seek(0, 0)
				} else {
					out = f
					io.WriteString(master, wakemute.Challenge(op, "mute")+"\n")
				}
			case "tmux":
				t.Setenv("TMUX", "fixture-multiplexer")
			case "screen":
				t.Setenv("STY", "fixture-multiplexer")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if kind == "timeout" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 25*time.Millisecond)
				defer cancel()
			}
			writerDone := make(chan error, 1)
			needResponse := kind == "valid" || kind == "decline" || kind == "eof" || strings.HasPrefix(kind, "wrong-") || kind == "reused" || kind == "replaced-root" || kind == "changed-after-confirm"
			if needResponse {
				go func() {
					reader := bufio.NewReader(master)
					for {
						line, e := reader.ReadString('\n')
						if e != nil {
							writerDone <- e
							return
						}
						if !strings.HasPrefix(line, "Type exactly: ") {
							continue
						}
						response := strings.TrimSpace(strings.TrimPrefix(line, "Type exactly: "))
						other := op
						switch kind {
						case "decline":
							response = "no"
						case "eof":
							response = "\x04"
						case "wrong-id":
							other.ID = strings.Repeat("0", 32)
							response = wakemute.Challenge(other, "mute")
						case "wrong-host":
							other.Host = "foreign"
							response = wakemute.Challenge(other, "mute")
						case "wrong-root":
							other.Scope = fixture.Scope(t.TempDir())
							response = wakemute.Challenge(other, "mute")
						case "wrong-connection":
							other.Scope = wakemute.Scope{{Root: root, Connections: []wakemute.Connection{{Name: "other", Path: "connections/other"}}}}
							response = wakemute.Challenge(other, "mute")
						case "reused":
							other, _ = wakemute.NewOperation(scope, "other-run")
							response = wakemute.Challenge(other, "mute")
						case "replaced-root":
							if e = os.Rename(root, root+"-old"); e == nil {
								e = os.Mkdir(root, 0700)
							}
							if e != nil {
								writerDone <- e
								return
							}
						}
						if kind != "eof" {
							response += "\n"
						}
						_, e = io.WriteString(master, response)
						writerDone <- e
						return
					}
				}()
			}
			before := ledger(t, root)
			approved, err := wakemute.Confirm(ctx, in, out, op, "mute")
			if needResponse {
				if e := <-writerDone; e != nil {
					t.Fatal(e)
				}
			}
			if kind == "valid" || kind == "changed-after-confirm" {
				if err != nil {
					t.Fatal(err)
				}
				if kind == "changed-after-confirm" {
					approved.Run = "broadened"
				}
				result, e := wakemute.Mute(fixture.Context(t), approved)
				if kind == "valid" {
					if e != nil || result.Status != "muted" {
						t.Fatalf("valid consent: %v", e)
					}
					fixture.Unmute(t, *result.Operation)
					return
				}
				if !errors.Is(e, wakemute.ErrAuthority) {
					t.Fatalf("changed consent admitted: %v", e)
				}
			} else if err == nil {
				t.Fatal("invalid TTY/consent accepted")
			}
			if kind != "replaced-root" && string(before) != string(ledger(t, root)) {
				t.Fatal("refused consent wrote state")
			}
			if kind == "replaced-root" {
				if !errors.Is(err, wakemute.ErrUnsafe) {
					t.Fatalf("replacement refused for wrong reason: %v", err)
				}
				names, _ := os.ReadDir(root)
				if len(names) != 0 {
					t.Fatal("replacement was mutated")
				}
			}
		})
	}
}

// Constructing an operation or copying its public JSON never grants mutation
// authority. Replacing any one root after confirmation refuses the whole set.
func TestScopeMuteNoBareAuthorityAndRootReplacement(t *testing.T) {
	a, b := t.TempDir(), filepath.Join(t.TempDir(), "selected")
	os.Mkdir(b, 0700)
	scope := append(fixture.Scope(a), fixture.Scope(b)...)
	fixture.Enroll(t, scope)
	bare, _ := wakemute.NewOperation(scope, "bare")
	before := ledger(t, a)
	if _, err := wakemute.Mute(fixture.Context(t), bare); !errors.Is(err, wakemute.ErrAuthority) {
		t.Fatal("public operation authorized mute")
	}
	approved := fixture.Confirm(t, scope, "replace-after", "mute")
	os.Rename(b, b+"-old")
	os.Mkdir(b, 0700)
	for _, name := range []string{wakemute.Marker, wakemute.Ledger} {
		data, _ := os.ReadFile(filepath.Join(b+"-old", name))
		os.WriteFile(filepath.Join(b, name), data, 0600)
	}
	if _, err := wakemute.Mute(fixture.Context(t), approved); !errors.Is(err, wakemute.ErrAuthority) {
		t.Fatalf("replacement: %v", err)
	}
	if string(before) != string(ledger(t, a)) {
		t.Fatal("one replaced root allowed partial mutation")
	}
}
