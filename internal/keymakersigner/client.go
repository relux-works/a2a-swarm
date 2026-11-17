// Package keymakersigner implements the signed-post signer over the explicitly
// pinned native control socket. It owns no key and never retries a request.
package keymakersigner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/relux-works/a2a-swarm/internal/signedpost"
	"github.com/relux-works/swarma-envelope/record"
	"github.com/relux-works/swarma-keymaker/messageauthor"
)

const (
	requestBudget  = 2 * time.Second
	helloRequestID = "00000000000000000000000000000001"
)

// A protocol/result error deliberately falls outside the signer's typed seam:
// the sender classifies it as signer_result_invalid without received details.
var errResultInvalid = errors.New("signer result invalid")

// Tests may replace entropy without changing the production random source.
var readRandom = rand.Read

// Endpoint is installed configuration, not discovery or an environment default.
// Mode must be 0660; UID and GID are the holder and message-author client group.
type Endpoint struct {
	Path     string
	UID, GID uint32
	Mode     fs.FileMode
}

type retainedID struct {
	id    string
	until time.Time
}

// Client is safe for concurrent use. At most 64 live signing request identities
// are retained for the maximum message lifetime. Capacity refuses rather than
// evicting a live id and accidentally requesting fresh signing on a retry.
type Client struct {
	endpoint Endpoint
	mu       sync.Mutex
	ids      map[string]retainedID
}

var _ signedpost.Signer = (*Client)(nil)

func New(endpoint Endpoint) *Client { return &Client{endpoint: endpoint} }

func randomID() (string, error) {
	for {
		var b [16]byte
		if _, err := readRandom(b[:]); err != nil {
			return "", signedpost.ErrUnavailable
		}
		id := hex.EncodeToString(b[:])
		// The holder retains by principal and ID; reserve hello's ID even
		// when the random source happens to draw those exact bytes.
		if id != helloRequestID {
			return id, nil
		}
	}
}

func (c *Client) signingID(preimage []byte) (string, error) {
	fingerprint := signedpost.RequestID(preimage)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, v := range c.ids {
		if !now.Before(v.until) {
			delete(c.ids, k)
		}
	}
	if v, ok := c.ids[fingerprint]; ok {
		return v.id, nil
	}
	if len(c.ids) >= 64 {
		return "", signedpost.ErrUnavailable
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	if c.ids == nil {
		c.ids = make(map[string]retainedID)
	}
	c.ids[fingerprint] = retainedID{id, now.Add(signedpost.MaxLifetime)}
	return id, nil
}

// Identity uses one connection and returns only codec-checked K/T/A whose
// attestation content id matches. This does not verify an issuer or currentness.
func (c *Client) Identity(ctx context.Context) (signedpost.Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*requestBudget)
	defer cancel()
	q, frame, err := request(messageauthor.IdentityMethod, "", nil)
	if err != nil {
		return signedpost.Identity{}, err
	}
	conn, close, err := c.open(ctx)
	if err != nil {
		return signedpost.Identity{}, err
	}
	defer close()
	return identity(ctx, conn, q, frame)
}

// Sign builds both request frames before dialing. On its own connection it
// obtains a fresh identity, then immediately sends the exact original preimage.
// Only the caller may retry: an equal preimage retains its random native id.
func (c *Client) Sign(ctx context.Context, preimage []byte) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*requestBudget)
	defer cancel()
	if len(preimage) > messageauthor.MaxPreimage {
		return nil, nil, signedpost.ErrUnqualified
	}
	preimage = bytes.Clone(preimage)
	// Parse and admit P before connecting. After the fresh identity arrives,
	// only its local qualification and byte-equality checks precede sign.
	key, attestationID, err := preimageIdentity(preimage)
	if err != nil {
		return nil, nil, err
	}
	id, err := c.signingID(preimage)
	if err != nil {
		return nil, nil, err
	}
	sq, sf, err := request(messageauthor.SignMethod, id, preimage)
	if err != nil {
		return nil, nil, err
	}
	iq, inf, err := request(messageauthor.IdentityMethod, "", nil)
	if err != nil {
		return nil, nil, err
	}
	conn, close, err := c.open(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer close()
	own, err := identity(ctx, conn, iq, inf)
	if err != nil {
		return nil, nil, err
	}
	// These are the exact textual K/T bytes admitted from P, not normalized
	// equivalents. Close on either change; the sender must prepare anew.
	if own.Key != key || own.AttestationID != attestationID {
		return nil, nil, signedpost.ErrIdentityChanged
	}
	r, err := exchange(ctx, conn, sq, sf)
	if err != nil {
		return nil, nil, err
	}
	if r.Signature.KeyID != own.Key {
		return nil, nil, errResultInvalid
	}
	// VerifySignature owns SHA-256 of the original preimage, exactly once.
	if record.VerifySignature(own.Key, r.Signature.Signature, preimage) != nil {
		return nil, nil, errResultInvalid
	}
	if ctx.Err() != nil {
		return nil, nil, signedpost.ErrUnavailable
	}
	return []byte(r.Signature.KeyID), r.Signature.Signature, nil
}

func preimageIdentity(preimage []byte) (string, string, error) {
	_, payload, ok := bytes.Cut(preimage, []byte{'\n'})
	if !ok {
		return "", "", signedpost.ErrUnqualified
	}
	parsed, err := record.Parse(payload, messageauthor.MaxPreimage, 12)
	if err != nil {
		return "", "", signedpost.ErrUnqualified
	}
	object, _ := parsed.(map[string]any)
	body, _ := object["body"].(map[string]any)
	key, _ := object["key"].(string)
	attestationID, _ := body["author_attestation"].(string)
	if _, err := record.AdmitSigner(preimage, record.IdentityPolicy{Check: true, MultiAgent: true, CallerKey: key, CallerAttestationID: attestationID}); err != nil {
		return "", "", signedpost.ErrUnqualified
	}
	return key, attestationID, nil
}

func identity(ctx context.Context, conn net.Conn, q messageauthor.Request, frame []byte) (signedpost.Identity, error) {
	r, err := exchange(ctx, conn, q, frame)
	if err != nil {
		return signedpost.Identity{}, err
	}
	v := r.Identity
	id := signedpost.Identity{Key: v.KeyID, AttestationID: v.AuthorAttestation, Attestation: v.Attestation}
	if signedpost.CheckIdentityShape(id) != nil || signedpost.CheckAttestation(id) != nil {
		return signedpost.Identity{}, signedpost.ErrUnqualified
	}
	if ctx.Err() != nil {
		return signedpost.Identity{}, signedpost.ErrUnavailable
	}
	return id, nil
}

func request(method messageauthor.Method, id string, preimage []byte) (messageauthor.Request, []byte, error) {
	var err error
	if id == "" {
		id, err = randomID()
	}
	if err != nil {
		return messageauthor.Request{}, nil, err
	}
	if id == helloRequestID {
		return messageauthor.Request{}, nil, errResultInvalid
	}
	q := messageauthor.Request{ID: id, Method: method, TimeoutMS: 1500, Preimage: preimage}
	frame, err := messageauthor.EncodeRequestFrame(q)
	if err != nil {
		return messageauthor.Request{}, nil, signedpost.ErrUnqualified
	}
	return q, frame, nil
}

func mapRefusal(r *messageauthor.Refusal) error {
	if r.Retryable {
		return signedpost.ErrUnavailable
	}
	switch r.Code {
	case "key_not_enrolled", "message_author_unqualified":
		return signedpost.ErrUnqualified
	default:
		return &signedpost.RefusedError{HolderCode: r.Code}
	}
}

func exchange(ctx context.Context, conn net.Conn, q messageauthor.Request, frame []byte) (messageauthor.Response, error) {
	if err := deadline(ctx, conn); err != nil {
		return messageauthor.Response{}, err
	}
	if err := writeFrame(conn, frame); err != nil {
		return messageauthor.Response{}, signedpost.ErrUnavailable
	}
	frame, err := readFrame(conn)
	if err != nil {
		return messageauthor.Response{}, err
	}
	if ctx.Err() != nil {
		return messageauthor.Response{}, signedpost.ErrUnavailable
	}
	// Context is ours, never inferred from received ID or result members.
	r, err := messageauthor.DecodeResponseFrame(q, frame)
	if err != nil {
		return messageauthor.Response{}, errResultInvalid
	}
	if r.Refusal != nil {
		return messageauthor.Response{}, mapRefusal(r.Refusal)
	}
	return r, nil
}

func deadline(ctx context.Context, conn net.Conn) error {
	if ctx.Err() != nil {
		return signedpost.ErrUnavailable
	}
	d := time.Now().Add(requestBudget)
	if overall, ok := ctx.Deadline(); ok && overall.Before(d) {
		d = overall
	}
	if conn.SetDeadline(d) != nil {
		return signedpost.ErrUnavailable
	}
	return nil
}

func writeFrame(w io.Writer, frame []byte) error {
	for len(frame) > 0 {
		n, err := w.Write(frame)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		frame = frame[n:]
	}
	return nil
}

func readFrame(r io.Reader) ([]byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, signedpost.ErrUnavailable
	}
	n, err := messageauthor.FrameLength(h)
	if err != nil {
		return nil, errResultInvalid
	}
	frame := make([]byte, 4+n)
	copy(frame, h[:])
	if _, err := io.ReadFull(r, frame[4:]); err != nil {
		return nil, signedpost.ErrUnavailable
	}
	return frame, nil
}

func (c *Client) pinnedSocket() (os.FileInfo, error) {
	e := c.endpoint
	if !filepath.IsAbs(e.Path) || filepath.Clean(e.Path) != e.Path || e.Mode != 0o660 {
		return nil, signedpost.ErrUnavailable
	}
	parent, err := os.Lstat(filepath.Dir(e.Path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0o022 != 0 || !ownedDirectory(parent, e.UID) {
		return nil, signedpost.ErrUnavailable
	}
	st, err := os.Lstat(e.Path)
	if err != nil || st.Mode()&os.ModeSocket == 0 || st.Mode().Perm() != e.Mode || st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || !ownedSocket(st, e.UID, e.GID) {
		return nil, signedpost.ErrUnavailable
	}
	return st, nil
}

func (c *Client) open(ctx context.Context) (net.Conn, func(), error) {
	// Every filesystem pin is checked before dialing, and kernel credentials
	// and the endpoint inode are checked before even a hello byte is written.
	before, err := c.pinnedSocket()
	if err != nil {
		return nil, nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.endpoint.Path)
	if err != nil {
		return nil, nil, signedpost.ErrUnavailable
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	close := func() { stop(); conn.Close() }
	after, err := c.pinnedSocket()
	if err != nil || !os.SameFile(before, after) || !peerUID(conn, c.endpoint.UID) {
		close()
		return nil, nil, signedpost.ErrUnavailable
	}
	if err := deadline(ctx, conn); err != nil {
		close()
		return nil, nil, err
	}
	body := []byte(`{"method":"hello","request_id":"` + helloRequestID + `","versions":[1]}`)
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	if writeFrame(conn, frame) != nil {
		close()
		return nil, nil, signedpost.ErrUnavailable
	}
	reply, err := readFrame(conn)
	want := []byte(`{"request_id":"` + helloRequestID + `","result":{"version":1}}`)
	if err != nil || !bytes.Equal(reply[4:], want) {
		close()
		return nil, nil, signedpost.ErrUnavailable
	}
	return conn, close, nil
}
