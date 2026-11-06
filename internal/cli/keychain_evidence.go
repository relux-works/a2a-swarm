package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

// Only finite observations cross the secret-store boundary. Exit is a process
// low byte, never a reconstructed OSStatus. Several observations may coexist.
type secretEvidence struct {
	Instrument string   `json:"instrument"`
	Codes      []string `json:"codes"`
	Exit       *int     `json:"process_exit,omitempty"`
}
type secretFailure struct {
	evidence secretEvidence
	message  string
	sentinel error
}

func (e *secretFailure) Error() string { return e.message }
func (e *secretFailure) Unwrap() error { return e.sentinel }
func observedSecretFailure(message, instrument string, err, interrupted error) error {
	e := secretEvidence{Instrument: instrument}
	var prior *secretFailure
	if errors.As(err, &prior) {
		e = prior.evidence
	} else {
		if errors.Is(err, exec.ErrWaitDelay) {
			e.Codes = append(e.Codes, "drain")
		}
		if errors.Is(err, errSecurityProcessIO) {
			e.Codes = append(e.Codes, "io")
		}
		if errors.Is(err, errSecurityProcessCleanup) {
			e.Codes = append(e.Codes, "cleanup")
		}
		var exit *exec.ExitError
		var path *os.PathError
		if errors.As(err, &exit) {
			code := exit.ExitCode()
			e.Exit = &code
			c := "ordinary-exit"
			if interactionFailure(exit) {
				c = "interaction-exit"
			}
			e.Codes = append(e.Codes, c)
		} else if errors.As(err, &path) {
			e.Codes = append(e.Codes, "process-start")
		}
	}
	if errors.Is(interrupted, context.DeadlineExceeded) {
		e.Codes = append(e.Codes, "deadline")
	}
	if errors.Is(interrupted, context.Canceled) {
		e.Codes = append(e.Codes, "cancelled")
	}
	if len(e.Codes) == 0 {
		e.Codes = []string{"unavailable"}
	}
	return &secretFailure{evidence: e, message: message}
}
func fixedSecretFailure(message, instrument, code string, sentinel error) error {
	return &secretFailure{evidence: secretEvidence{Instrument: instrument, Codes: []string{code}}, message: message, sentinel: sentinel}
}
func registrationFailureEvidence(err error, empty bool) secretEvidence {
	var failure *secretFailure
	if errors.As(err, &failure) {
		return failure.evidence
	}
	if errors.Is(err, errKeychainMissing) {
		return secretEvidence{Instrument: "security", Codes: []string{"missing"}}
	}
	if err == nil && empty {
		return secretEvidence{Instrument: "security", Codes: []string{"empty"}}
	}
	return secretEvidence{Instrument: "unknown", Codes: []string{"unavailable"}}
}

func exitSecretFailure(sentinel error, code string, exit int) error {
	return &secretFailure{evidence: secretEvidence{Instrument: "security", Codes: []string{code}, Exit: &exit}, message: sentinel.Error(), sentinel: sentinel}
}
