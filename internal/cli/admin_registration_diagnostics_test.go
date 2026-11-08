package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// These controls exercise diagnostics only. They do not construct an admin
// session, issue a command, register an account or qualify the live route.
func TestAdminRegistrationRecognizedDiagnostics(t *testing.T) {
	for _, code := range []string{
		"M_INVALID_USERNAME", "M_EXCLUSIVE", "M_NOT_JSON", "M_MISSING_PARAM",
		"M_FORBIDDEN",
	} {
		t.Run(code, func(t *testing.T) {
			for _, status := range []int{400, 403} {
				t.Run(strconv.Itoa(status), func(t *testing.T) {
					for _, wrapped := range []bool{false, true} {
						name := "direct"
						if wrapped {
							name = "wrapped"
						}
						t.Run(name, func(t *testing.T) {
							var input error = &matrix.Error{
								Status: status, ErrCode: code,
								Message: "<UNTRUSTED-DETAIL>", Raw: []byte("<UNTRUSTED-BODY>"),
							}
							if wrapped {
								input = fmt.Errorf("<UNTRUSTED-WRAPPER>: %w", input)
							}
							want := "admin registration stage=register errcode=" + code +
								" HTTP=" + strconv.Itoa(status) + "; no automatic retry"
							assertAdminRegistrationDiagnostic(t, input, want)
						})
					}
				})
			}
		})
	}
}

func TestAdminRegistrationUntrustedDiagnostics(t *testing.T) {
	cases := []struct{ name, code string }{
		{"unknown", "<UNKNOWN-CODE>"},
		{"empty", ""},
	}
	for _, code := range []string{"M_INVALID_USERNAME", "M_EXCLUSIVE", "M_NOT_JSON", "M_MISSING_PARAM", "M_FORBIDDEN"} {
		cases = append(cases,
			struct{ name, code string }{code + "/prefix", "<UNTRUSTED-CODE>" + code},
			struct{ name, code string }{code + "/suffix", code + "<UNTRUSTED-CODE>"},
			struct{ name, code string }{code + "/control", code + "\n"},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := fmt.Errorf("<UNTRUSTED-WRAPPER>: %w", &matrix.Error{
				Status: 403, ErrCode: tc.code,
				Message: "<UNTRUSTED-DETAIL>", Raw: []byte("<UNTRUSTED-BODY>"),
			})
			assertAdminRegistrationDiagnostic(t, input,
				"admin registration stage=register errcode=SERVER_ERROR HTTP=403; no automatic retry")
		})
	}
}

func TestAdminRegistrationLocalDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"local", errors.New("<UNTRUSTED-LOCAL>")},
		{"wrapped-local", fmt.Errorf("<UNTRUSTED-WRAPPER>: %w", errors.New("<UNTRUSTED-LOCAL>"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertAdminRegistrationDiagnostic(t, tc.err,
				"admin registration stage=register errcode=LOCAL_REFUSAL HTTP=0; no automatic retry")
		})
	}
}

func assertAdminRegistrationDiagnostic(t *testing.T, input error, want string) {
	t.Helper()
	got := adminRegistrationError("register", input)
	if got == nil || got.Error() != want {
		t.Fatal("fixed registration diagnostic differs from expected code, status or wording")
	}
	var stdout, stderr bytes.Buffer
	a := &App{Stdout: &stdout, Stderr: &stderr}
	if a.fail(got) != ExitError {
		t.Fatal("registration diagnostic returned an unexpected exit code")
	}
	if stdout.Len() != 0 || stderr.String() != "a2a: "+want+"\n" {
		t.Fatal("captured registration diagnostic differs from fixed output")
	}
	for _, text := range []string{got.Error(), stdout.String(), stderr.String()} {
		for _, label := range []string{
			"<UNTRUSTED-DETAIL>", "<UNTRUSTED-BODY>", "<UNTRUSTED-WRAPPER>",
			"<UNTRUSTED-CODE>", "<UNTRUSTED-LOCAL>", "<UNKNOWN-CODE>",
		} {
			if strings.Contains(text, label) {
				t.Fatal("untrusted diagnostic content escaped suppression")
			}
		}
	}
}

type adminDiagnosticEffectSpy struct {
	transport, roomSends, reads, creates int
}

func (s *adminDiagnosticEffectSpy) RoundTrip(r *http.Request) (*http.Response, error) {
	s.transport++
	if r.URL != nil && strings.Contains(r.URL.Path, "/send/") {
		s.roomSends++
	}
	return nil, errors.New("<INERT-REFUSAL>")
}

func (s *adminDiagnosticEffectSpy) Read(context.Context, config.Paths, config.SecretReference) (string, error) {
	s.reads++
	return "", errors.New("<INERT-REFUSAL>")
}

func (s *adminDiagnosticEffectSpy) Create(context.Context, config.Paths, config.SecretReference, string) error {
	s.creates++
	return errors.New("<INERT-REFUSAL>")
}

func TestAdminRegistrationDiagnosticHasNoRegistrationEffects(t *testing.T) {
	// Only the error formatter and normal diagnostic sink are invoked. Spies
	// refuse in memory; they cannot send requests or access credential storage.
	root := t.TempDir()
	var spy adminDiagnosticEffectSpy
	var stdout, stderr bytes.Buffer
	envReads, credentialOps := 0, 0
	a := &App{
		Stdout: &stdout, Stderr: &stderr,
		HTTP: &http.Client{Transport: &spy}, connectionSecretStore: &spy,
		Getenv:   func(string) string { envReads++; return root },
		keychain: func(config.Paths) *keychainStore { credentialOps++; return nil },
	}
	input := fmt.Errorf("<UNTRUSTED-WRAPPER>: %w", &matrix.Error{
		Status: 400, ErrCode: "M_INVALID_USERNAME",
		Message: "<UNTRUSTED-DETAIL>", Raw: []byte("<UNTRUSTED-BODY>"),
	})
	if a.fail(adminRegistrationError("register", input)) != ExitError {
		t.Fatal("diagnostic-only invocation returned an unexpected exit code")
	}
	if spy.transport != 0 || spy.roomSends != 0 || spy.reads != 0 || spy.creates != 0 || envReads != 0 || credentialOps != 0 {
		t.Fatal("diagnostic-only invocation attempted a registration effect")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("diagnostic-only invocation changed isolated storage")
	}
	if stdout.Len() != 0 || stderr.String() != "a2a: admin registration stage=register errcode=M_INVALID_USERNAME HTTP=400; no automatic retry\n" {
		t.Fatal("diagnostic-only invocation did not retain fixed output")
	}
}
