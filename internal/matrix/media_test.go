package matrix

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Proves ParseMXC accepts exactly mxc://<hostname[:port]>/<[A-Za-z0-9_-]+>
// and rejects everything that could inject path segments, queries, or
// traversal into the download URL.
func TestParseMXC(t *testing.T) {
	valid := map[string]MXC{
		"mxc://a2a.local/AbC_09-x":      {"a2a.local", "AbC_09-x"},
		"mxc://localhost:8448/id":       {"localhost:8448", "id"},
		"mxc://127.0.0.1/id":            {"127.0.0.1", "id"},
		"mxc://[::1]:8008/id":           {"[::1]:8008", "id"},
		"mxc://matrix-1.example.org/-x": {"matrix-1.example.org", "-x"},
	}
	for in, want := range valid {
		got, err := ParseMXC(in)
		if err != nil || got != want || got.String() != in {
			t.Errorf("ParseMXC(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	invalid := []string{
		"", "mxc://", "mxc:///id", "mxc://a2a.local", "mxc://a2a.local/",
		"http://a2a.local/id", "MXC://a2a.local/id", "mxc:a2a.local/id",
		"mxc://a2a.local/a/b", "mxc://a2a.local/../x", "mxc://a2a.local/..",
		"mxc://../id", "mxc://./id", "mxc://a2a..local/id", "mxc://.a2a.local/id", "mxc://a2a.local./id",
		"mxc://a2a.local/id?x=1", "mxc://a2a.local/id#f", "mxc://a2a.local/id%2F", "mxc://a2a.local/i d",
		"mxc://user@a2a.local/id", "mxc://a2a.local:port/id", "mxc://a2a.local:123456/id",
		"mxc://a2a.local/id\x00", "mxc://a2a.local/id\n", "mxc://a2a.local\\x/id",
		"mxc://a2a.local/" + strings.Repeat("a", 256),
	}
	for _, in := range invalid {
		if got, err := ParseMXC(in); err == nil {
			t.Errorf("ParseMXC(%q) = %+v, want error", in, got)
		}
	}
}

// Proves Download sends no request for a malformed mxc, and that a valid one
// hits the authenticated v1 endpoint with the token only in the header.
func TestDownloadRequest(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.String())
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte("data"))
	}))
	defer srv.Close()
	c := New(srv.URL, "tok")
	if _, err := c.Download(context.Background(), "mxc://a2a.local/../../x"); err == nil {
		t.Fatal("malformed mxc accepted")
	}
	if len(paths) != 0 {
		t.Fatalf("requests sent: %v", paths)
	}
	rc, err := c.Download(context.Background(), "mxc://a2a.local/abc")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if string(data) != "data" || len(paths) != 1 || paths[0] != "/_matrix/client/v1/media/download/a2a.local/abc" {
		t.Fatalf("data %q, paths %v", data, paths)
	}
}
