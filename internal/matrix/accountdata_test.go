package matrix

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Proves account data is read and written at /user/{userId}/account_data/{type}
// with the user id escaped, the JSON body sent as is, and an unset type
// surfacing as M_NOT_FOUND (the negative control for "never set").
func TestAccountData(t *testing.T) {
	store := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.EscapedPath()
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			store[key] = string(b)
			w.Write([]byte(`{}`))
		case http.MethodGet:
			v, ok := store[key]
			if !ok {
				w.WriteHeader(404)
				w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"Account data not found"}`))
				return
			}
			w.Write([]byte(v))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "t")
	ctx := context.Background()
	var out struct {
		V int `json:"v"`
	}
	if err := c.GetAccountData(ctx, "@a:s", "ai.relux.a2a.subscriptions", &out); !IsCode(err, "M_NOT_FOUND") {
		t.Fatalf("unset type: err = %v", err)
	}
	if err := c.PutAccountData(ctx, "@a:s", "ai.relux.a2a.subscriptions", map[string]int{"v": 1}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store["/_matrix/client/v3/user/@a:s/account_data/ai.relux.a2a.subscriptions"]; !ok {
		t.Fatalf("stored under %v", store)
	}
	if err := c.GetAccountData(ctx, "@a:s", "ai.relux.a2a.subscriptions", &out); err != nil || out.V != 1 {
		t.Fatalf("get = %+v, %v", out, err)
	}
}
