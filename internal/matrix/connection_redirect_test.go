package matrix

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Proves cross-origin redirects never reach the destination, for bearer calls
// or registration bodies. Same-origin ordinary reads remain functional.
func TestConnectionRedirectCredentialOrigin(t *testing.T) {
	var destination atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destination.Add(1)
		w.Write([]byte(`{"user_id":"@alice:test.invalid","access_token":"test"}`))
	}))
	defer other.Close()
	for _, register := range []bool{false, true} {
		t.Run(map[bool]string{false: "bearer", true: "registration"}[register], func(t *testing.T) {
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
			}))
			defer source.Close()
			c := New(source.URL, "test-token")
			var e error
			if register {
				_, e = c.Register(context.Background(), "alice", "test-password", "test-bootstrap", "test")
			} else {
				_, e = c.Whoami(context.Background())
			}
			if e == nil || destination.Load() != 0 {
				t.Fatal("cross-origin credential redirect admitted")
			}
		})
	}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/same" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"user_id":"@alice:test.invalid","device_id":"test"}`))
			return
		}
		http.Redirect(w, r, "/same", http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	if _, e := New(source.URL, "test-token").Whoami(context.Background()); e != nil {
		t.Fatal(e)
	}
}
