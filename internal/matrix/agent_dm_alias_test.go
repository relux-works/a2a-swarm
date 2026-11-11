package matrix

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPairAliasAbsenceEvidence(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		body          string
		absent, valid bool
	}{
		{"opaque", 200, `{"room_id":"!opaque"}`, false, true},
		{"qualified", 200, `{"room_id":"!room:server.invalid"}`, false, true},
		{"absent", 404, `{"errcode":"M_NOT_FOUND"}`, true, false},
		{"server-error", 500, `{"errcode":"M_NOT_FOUND"}`, false, false},
		{"denied", 403, `{"errcode":"M_FORBIDDEN"}`, false, false},
		{"duplicate", 404, `{"errcode":"M_NOT_FOUND","errcode":"M_NOT_FOUND"}`, false, false},
		{"trailing", 404, `{"errcode":"M_NOT_FOUND"}{}`, false, false},
		{"bad-room", 200, `{"room_id":"!bad room"}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("mutation")
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			room, err := New(srv.URL, "fixture").ResolvePairAlias(context.Background(), "#pair:fixture.invalid")
			if (err == nil) != tc.valid || errors.Is(err, ErrUnknownAlias) != tc.absent {
				t.Fatalf("room=%q err=%v absent=%t", room, err, tc.absent)
			}
		})
	}
}
