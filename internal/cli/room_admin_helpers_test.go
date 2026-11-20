package cli

import (
	"io"
	"net/http"
	"strings"
)

// Shared fixture reply for the typed-property admin tests.

func propertyCLIReply(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r, Header: make(http.Header)}
}
