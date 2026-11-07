package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type registrationDiagnosticTransport func(*http.Request) (*http.Response, error)

func (f registrationDiagnosticTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestNamedRegistrationRejectionDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"forbidden", `{"errcode":"M_FORBIDDEN","error":"private-server-detail"}`, "M_FORBIDDEN", 403},
		{"name-in-use", `{"errcode":"M_USER_IN_USE"}`, "M_USER_IN_USE", 400},
		{"invalid-name", `{"errcode":"M_INVALID_USERNAME"}`, "M_INVALID_USERNAME", 400},
		{"rate-limit", `{"errcode":"M_LIMIT_EXCEEDED"}`, "M_LIMIT_EXCEEDED", 429},
		{"unknown-code", `{"errcode":"M_PRIVATE_SERVER_DETAIL","error":"private-server-detail"}`, "", 403},
		{"malformed", `{"errcode":"M_FORBIDDEN",`, "", 403},
		{"duplicate-code", `{"errcode":"M_FORBIDDEN","errcode":"M_USER_IN_USE"}`, "", 403},
		{"server-failure", `{"errcode":"M_FORBIDDEN","error":"private-server-detail"}`, "", 503},
		{"transport-failure", "", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNamedFixture(t, false)
			a := f.app()
			a.HTTP = &http.Client{Transport: registrationDiagnosticTransport(func(r *http.Request) (*http.Response, error) {
				f.httpCalls.Add(1)
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/register") {
					t.Fatal("unexpected request")
				}
				if tc.status == 0 {
					return nil, errors.New("private-transport-detail")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), ContentLength: int64(len(tc.body)), Header: make(http.Header), Request: r}, nil
			})}
			args := []string{"--profile", "alpha", "register", "alice", "--authorize-registration"}
			if a.Run(context.Background(), args) == 0 {
				t.Fatal("rejection succeeded")
			}
			p := f.paths("alpha", false)
			journalPath := filepath.Join(p.Base, "registration.pending.json")
			raw, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Stage      string `json:"stage"`
				HTTPStatus int    `json:"http_status"`
				ErrCode    string `json:"errcode"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			wantStage, wantStatus := "remote-outcome-ambiguous", 0
			if tc.status >= 400 && tc.status < 500 {
				wantStage, wantStatus = "remote-rejected", tc.status
			}
			if got.Stage != wantStage || got.HTTPStatus != wantStatus || got.ErrCode != tc.code {
				t.Errorf("diagnostics = %+v; want stage=%s status=%d code=%s", got, wantStage, wantStatus, tc.code)
			}
			wantOutput := "registration " + wantStage
			if wantStatus != 0 {
				wantOutput += " (HTTP " + strconv.Itoa(wantStatus)
				if tc.code != "" {
					wantOutput += "; " + tc.code
				}
				wantOutput += ")"
			}
			if !strings.Contains(f.err.String(), wantOutput+"; owner recovery required (no automatic retry)") {
				t.Errorf("missing bounded CLI diagnostics: %s", f.err.String())
			}
			passwordRef := config.SecretReference{Service: "test.alpha.password", Account: "@alice:alpha.invalid"}
			password := f.store.items[passwordRef]
			if password == "" || len(f.store.creates) != 1 || f.store.creates[0] != passwordRef {
				t.Fatal("recovery password was not the only created item")
			}
			for _, secret := range []string{password, "isolated-bootstrap-alpha", "private-server-detail", "private-transport-detail", "M_PRIVATE_SERVER_DETAIL"} {
				if strings.Contains(string(raw)+f.out.String()+f.err.String(), secret) {
					t.Fatal("private value leaked")
				}
			}
			if _, err := os.Lstat(p.AgentFile("alice")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejected registration published metadata")
			}
			info, err := os.Stat(journalPath)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("journal is not private")
			}
			if f.httpCalls.Load() != 1 {
				t.Fatal("registration made more than one request")
			}
			retry := f.app()
			retry.HTTP = a.HTTP
			if retry.Run(context.Background(), args) == 0 || f.httpCalls.Load() != 1 || len(f.store.creates) != 1 || f.store.items[passwordRef] != password {
				t.Fatal("pending rejection repeated a registration effect")
			}
			after, err := os.ReadFile(journalPath)
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatal("re-invocation changed original journal")
			}
		})
	}
}
