package redact

import (
	"strings"
	"testing"
)

// Proves that registered secrets and secret-shaped fragments are replaced, and
// that ordinary text (the positive control) is left untouched.
func TestString(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		secrets []string
		want    string
	}{
		{"known secret", "token abcdef123456 rejected", []string{"abcdef123456"}, "token <redacted> rejected"},
		{"matrix token shape", "got syt_aXQtMTIz_AbCdEf_0Z9 back", nil, "got <redacted> back"},
		{"bearer header", "Authorization: Bearer abc.def-ghi", nil, "Authorization: Bearer <redacted>"},
		{"json field", `{"access_token":"s3cr3t-value","user_id":"@a:b"}`, nil, `{"access_token":"<redacted>","user_id":"@a:b"}`},
		{"password json", `{"password": "hunter2hunter2"}`, nil, `{"password": "<redacted>"}`},
		{"query string", "GET /sync?access_token=zzzzzz&since=1", nil, "GET /sync?access_token=<redacted>&since=1"},
		{"longest first", "AAAAAAAABBBB", []string{"AAAAAAAA", "AAAAAAAABBBB"}, "<redacted>"},
		{"plain text untouched", "room #lobby not found", []string{"abcdef123456"}, "room #lobby not found"},
		{"short secret ignored", "a b c", []string{"a"}, "a b c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := String(tc.in, tc.secrets...); got != tc.want {
				t.Fatalf("String(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Proves that a Set scrubs every secret it was given, and that a nil Set still
// removes secret-shaped fragments instead of panicking.
func TestSet(t *testing.T) {
	var s Set
	s.Add("first-secret", "", "second-secret")
	got := s.Scrub("first-secret and second-secret")
	if strings.Contains(got, "secret") {
		t.Fatalf("secrets leaked: %q", got)
	}
	var nilSet *Set
	if got := nilSet.Scrub("Bearer xyzxyzxyz"); strings.Contains(got, "xyzxyzxyz") {
		t.Fatalf("nil set leaked: %q", got)
	}
}
