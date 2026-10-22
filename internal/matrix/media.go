package matrix

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// MediaRequestTimeout bounds one media upload or download (bodies can be
// tens of MiB, so DefaultRequestTimeout would be too short).
const MediaRequestTimeout = 10 * time.Minute

// maxMediaIDLen bounds the media id of an mxc URI.
const maxMediaIDLen = 255

var (
	// mxcServerRE is hostname[:port]: a DNS name (labels of [A-Za-z0-9-],
	// so "." and ".." never match), an IPv4 address, or a bracketed IPv6 literal.
	mxcServerRE = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*)(:[0-9]{1,5})?$`)
	mxcMediaRE  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// MXC is a parsed and validated mxc://<server>/<media id> URI.
type MXC struct {
	Server  string
	MediaID string
}

func (m MXC) String() string { return "mxc://" + m.Server + "/" + m.MediaID }

// ParseMXC strictly parses mxc://<server>/<media id>. The server must be
// hostname[:port] and the media id [A-Za-z0-9_-]+, so both are safe to use as
// URL path segments (no traversal, no query, no extra segments).
func ParseMXC(s string) (MXC, error) {
	rest, ok := strings.CutPrefix(s, "mxc://")
	if !ok {
		return MXC{}, fmt.Errorf("invalid mxc URI: must start with mxc://")
	}
	server, id, ok := strings.Cut(rest, "/")
	if !ok || !mxcServerRE.MatchString(server) || len(server) > 255 {
		return MXC{}, fmt.Errorf("invalid mxc URI: bad server name")
	}
	if !mxcMediaRE.MatchString(id) || len(id) > maxMediaIDLen {
		return MXC{}, fmt.Errorf("invalid mxc URI: bad media id")
	}
	return MXC{Server: server, MediaID: id}, nil
}

func withMediaTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, MediaRequestTimeout)
}

// MediaConfig returns the server's maximum upload size in bytes (m.upload.size),
// or 0 if the server does not state one.
func (c *Client) MediaConfig(ctx context.Context) (int64, error) {
	var out struct {
		UploadSize int64 `json:"m.upload.size"`
	}
	err := c.call(ctx, http.MethodGet, v1+"/media/config", nil, nil, &out)
	return out.UploadSize, err
}

// Upload streams size bytes of body to the media repository and returns the
// validated mxc URI of the new media.
func (c *Client) Upload(ctx context.Context, filename, contentType string, body io.Reader, size int64) (string, error) {
	if size <= 0 {
		return "", fmt.Errorf("upload: size must be positive")
	}
	ctx, cancel := withMediaTimeout(ctx)
	defer cancel()
	q := url.Values{}
	if filename != "" {
		q.Set("filename", filename)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	resp, err := c.send(ctx, http.MethodPost, "/_matrix/media/v3/upload", q, io.LimitReader(body, size), contentType, size)
	if err != nil {
		return "", err
	}
	var out struct {
		ContentURI string `json:"content_uri"`
	}
	// The historical upload reader allowed a 64KiB response. Read one byte
	// beyond that boundary to distinguish an exactly full body from a prefix.
	v, err := readControl(ctx, resp, maxErrorBody+1)
	if err != nil {
		return "", err
	}
	if err = v.consume(ctx, &out, "content_uri"); err != nil {
		return "", err
	}
	m, err := ParseMXC(out.ContentURI)
	if err != nil {
		return "", fmt.Errorf("upload: server returned an %w", err)
	}
	return m.String(), nil
}

// Download opens the media behind mxc through the authenticated v1 client
// endpoint. The caller must close the body; it bounds how much it reads.
func (c *Client) Download(ctx context.Context, mxc string) (io.ReadCloser, error) {
	m, err := ParseMXC(mxc)
	if err != nil {
		return nil, err
	}
	ctx, cancel := withMediaTimeout(ctx)
	resp, err := c.send(ctx, http.MethodGet, v1+"/media/download/"+seg(m.Server)+"/"+seg(m.MediaID), nil, nil, "", -1)
	if err != nil {
		cancel()
		return nil, err
	}
	return &cancelBody{ReadCloser: resp.Body, cancel: cancel}, nil
}

// cancelBody releases the request context when the body is closed.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}
