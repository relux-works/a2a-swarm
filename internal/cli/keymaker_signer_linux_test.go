package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/signedpost"
	"github.com/relux-works/swarma-keymaker/messageauthor"
)

// nativePostHolder speaks real codec frames and signs only the public envelope
// fixture, via the existing fake signer. It never opens a configured host socket.
func nativePostHolder(t *testing.T, refusal string) config.MessageAuthor {
	t.Helper()
	dir, err := os.MkdirTemp(os.Getenv("TMPDIR"), "post-ks-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s")
	l, err := net.Listen("unix", path)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	if os.Chmod(path, 0o660) != nil {
		t.Fatal("socket mode")
	}
	identity := signedPostIdentity(t)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			// The caller is sequential; process one connection at a time.
			func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				f, err := nativePostRead(conn)
				if err != nil {
					return
				}
				want := []byte(`{"method":"hello","request_id":"00000000000000000000000000000001","versions":[1]}`)
				if !bytes.Equal(f, nativePostFrame(want)) {
					t.Error("hello must match the fixed canonical first frame")
					return
				}
				if _, err := conn.Write(nativePostFrame([]byte(`{"request_id":"00000000000000000000000000000001","result":{"version":1}}`))); err != nil {
					return
				}
				for {
					conn.SetDeadline(time.Now().Add(2 * time.Second))
					f, err = nativePostRead(conn)
					if err != nil {
						return
					}
					q, err := messageauthor.DecodeRequestFrame(f)
					if err != nil {
						t.Error("native request decode")
						return
					}
					if q.ID == "00000000000000000000000000000001" || q.TimeoutMS != 1500 {
						t.Error("native request ID or timeout")
						return
					}
					r := messageauthor.Response{ID: q.ID}
					if refusal != "" {
						r.Refusal = &messageauthor.Refusal{Code: refusal}
					} else if q.Method == messageauthor.IdentityMethod {
						r.Identity = &messageauthor.IdentityResult{KeyID: identity.Key, AuthorAttestation: identity.AttestationID, Attestation: identity.Attestation}
					} else {
						signer := &postFakeSigner{identity: identity}
						k, s, err := signer.Sign(context.Background(), q.Preimage)
						if err != nil {
							t.Error("public fixture sign")
							return
						}
						r.Signature = &messageauthor.SignatureResult{KeyID: string(k), Signature: s}
					}
					f, err = messageauthor.EncodeResponseFrame(q, r)
					if err != nil {
						t.Error("native response encode")
						return
					}
					if _, err = conn.Write(f); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { l.Close(); wg.Wait(); os.RemoveAll(dir) })
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	return config.MessageAuthor{SocketPath: path, UID: &uid, GID: &gid, Mode: "0660", Audience: signedPostAudience}
}

func nativePostFrame(b []byte) []byte {
	f := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(f, uint32(len(b)))
	copy(f[4:], b)
	return f
}
func nativePostRead(r io.Reader) ([]byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	n, err := messageauthor.FrameLength(h)
	if err != nil {
		return nil, err
	}
	f := make([]byte, n+4)
	copy(f, h[:])
	_, err = io.ReadFull(r, f[4:])
	return f, err
}

// Claim: configured fixture signing preserves event bytes; static errors have fixed unconfigured diagnostics, and every refusal publishes nothing.
func TestPostSignNativeConfiguration(t *testing.T) {
	for _, mode := range []string{"valid", "absent", "uid-absent", "gid-absent", "mode-absent", "wrong-mode", "metadata-missing", "metadata-malformed", "metadata-mode", "relative-path", "key_not_enrolled", "message_author_unqualified", "no-purpose-pin"} {
		t.Run(mode, func(t *testing.T) {
			e, room, _ := signedPostWorld(t)
			refusal := ""
			if mode == "key_not_enrolled" || mode == "message_author_unqualified" {
				refusal = mode
			}
			pin := nativePostHolder(t, refusal)
			switch mode {
			case "uid-absent":
				pin.UID = nil
			case "gid-absent":
				pin.GID = nil
			case "mode-absent":
				pin.Mode = ""
			case "wrong-mode":
				pin.Mode = "0600"
			case "relative-path":
				pin.SocketPath = "relative.sock"
			}
			paths := config.Paths{Base: e.dir}
			_, err := config.UpdateCredentials(paths, "author", func(c *config.Credentials) error {
				if mode != "absent" {
					c.MessageAuthor = &pin
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "metadata-missing":
				err = os.Remove(paths.AgentFile("author"))
			case "metadata-malformed":
				err = os.WriteFile(paths.AgentFile("author"), []byte("{"), 0o600)
			case "metadata-mode":
				err = os.Chmod(paths.AgentFile("author"), 0o644)
			}
			if err != nil {
				t.Fatal("metadata fixture setup")
			}
			configure := e.configure
			e.configure = func(a *App) {
				configure(a)
				a.postSigner = nil
				a.postAudience = ""
				if mode == "no-purpose-pin" {
					a.postQualified = nil
				}
			}
			before := e.eventCount()
			args := []string{"post", room, "--topic", "alpha", "--to", "recipient", "hello signed"}
			r := e.as("author", append(args, "--sign")...)
			if mode != "valid" {
				want := "signer_unconfigured"
				static := mode != "absent" && mode != "relative-path" && refusal == "" && mode != "no-purpose-pin"
				if mode == "relative-path" {
					want = "signer_unavailable"
				}
				if refusal != "" || mode == "no-purpose-pin" {
					want = "signer_unqualified"
				}
				r.failsWith(t, ExitError, "signed post refused: "+want)
				if static && r.stderr != "a2a: signed post refused: signer_unconfigured (message-author configuration invalid)\n" {
					t.Fatal("static configuration diagnostic not fixed")
				}
				if e.eventCount() != before || r.stdout != "" {
					t.Fatal("native refusal published")
				}
				return
			}
			id := strings.TrimSpace(r.ok(t).stdout)
			if e.eventCount() != before+1 {
				t.Fatal("publication count")
			}
			if got := e.verificationOf(t, "recipient", room, id); got != "verified: "+signedPostAuthor {
				t.Fatal("native fixture verification", got)
			}
			content := storedContent(t, e, id)
			encoded, ok := content[matrix.EnvelopeContentKey].(string)
			if !ok || encoded == "" {
				t.Fatal("envelope missing")
			}
			attestation, ok := content[signedpost.AttestationContentKey].(string)
			if !ok || attestation != base64.RawURLEncoding.EncodeToString(signedPostIdentity(t).Attestation) {
				t.Fatal("attestation bytes changed")
			}
			plainID := strings.TrimSpace(e.as("author", args...).ok(t).stdout)
			plain := storedContent(t, e, plainID)
			delete(content, matrix.EnvelopeContentKey)
			delete(content, signedpost.AttestationContentKey)
			if !reflect.DeepEqual(content, plain) {
				t.Fatal("carrier content changed")
			}
		})
	}
}

// Claim: the optional endpoint pins survive credential JSON round trips without appearing when absent.
func TestPostSignerConfigurationRoundTrip(t *testing.T) {
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprint(present), func(t *testing.T) {
			c := config.Credentials{UserID: "@fixture:test.invalid"}
			if present {
				c.MessageAuthor = &config.MessageAuthor{SocketPath: "/protected/control.sock", UID: &uid, GID: &gid, Mode: "0660", Audience: "service-a"}
			}
			b, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(b, []byte(`"message_author"`)) != present {
				t.Fatal("optional member presence")
			}
			var got config.Credentials
			if json.Unmarshal(b, &got) != nil || !reflect.DeepEqual(c, got) {
				t.Fatal("endpoint round trip")
			}
		})
	}
}
