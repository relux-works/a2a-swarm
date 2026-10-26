package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func validRegistrationTransition(from, to string) bool {
	switch from {
	case "":
		return to == "preflight"
	case "continuation-admitted":
		if to == "secret-preflight-cooldown" {
			return true
		}
		fallthrough
	case "preflight":
		return to == "secret-preflight-refused" || to == "bootstrap-unavailable" || to == "password-storage-pending"
	case "bootstrap-unavailable", "secret-preflight-cooldown":
		return to == "continuation-admitted"
	case "password-storage-pending":
		return to == "password-store-failed" || to == "remote-outcome-pending"
	case "remote-outcome-pending":
		return to == "remote-outcome-ambiguous" || to == "remote-rejected" || to == "remote-response-mismatch" || to == "remote-created-token-storage-pending"
	case "remote-created-token-storage-pending":
		return to == "remote-created-token-store-failed" || to == "remote-created-metadata-publication-pending"
	case "remote-created-metadata-publication-pending":
		return to == "complete"
	}
	return false
}

// Only the typed marker refusal is transient; unknown or mixed failures stay fenced.
func registrationCooldownEvidence(e *secretEvidence) bool {
	return e != nil && (e.Instrument == "probe" || e.Instrument == "security") && e.Exit == nil && len(e.Codes) == 1 && e.Codes[0] == "cooldown"
}
func (j registrationJournal) continuationCandidate() bool {
	return j.Version == 1 && (j.Stage == "bootstrap-unavailable" || j.Stage == "secret-preflight-cooldown")
}
func validSecretEvidence(e *secretEvidence) bool {
	if e == nil || (e.Instrument != "security" && e.Instrument != "probe" && e.Instrument != "unknown") || len(e.Codes) == 0 || len(e.Codes) > 12 {
		return false
	}
	if e.Exit != nil && (*e.Exit < -1 || *e.Exit > 255) {
		return false
	}
	seen := map[string]bool{}
	for _, c := range e.Codes {
		switch c {
		case "drain", "io", "cleanup", "ordinary-exit", "interaction-exit", "process-start", "deadline", "cancelled", "unavailable", "native-locked", "cooldown", "output-limit", "invalid-secret", "missing", "duplicate", "empty":
		default:
			return false
		}
		if seen[c] {
			return false
		}
		seen[c] = true
	}
	return true
}
func readRegistrationJournal(path string, b config.ConnectionBinding) (registrationJournal, error) {
	j := registrationJournal{}
	f, e := openKeychainState(path)
	if e != nil {
		return j, errors.New("registration evidence unavailable")
	}
	defer f.Close()
	data, e := io.ReadAll(io.LimitReader(f, 65537))
	if e != nil || len(data) > 65536 || config.DecodeConnectionJSON(data, &j) != nil {
		return j, errors.New("malformed registration evidence")
	}
	// Consumer fingerprints intentionally exclude bootstrap references. Recovery
	// additionally pins that original admission input; changing it is not recovery.
	originalRef, selectedRef := j.Binding.Connection.RegistrationTokenRef, b.Connection.RegistrationTokenRef
	if j.Binding.Connection.Validate() != nil || j.Binding.Fingerprint() != b.Fingerprint() || originalRef == nil || selectedRef == nil || *originalRef != *selectedRef {
		return j, errors.New("registration binding mismatch")
	}
	if j.Version == 0 && j.Attempt == "" && len(j.History) == 0 {
		// Original receipts have no ownership/history proof. Never upgrade them.
		if !knownRegistrationStage(j.Stage) {
			return j, errors.New("unknown registration stage")
		}
		return j, nil
	}
	id, e := hex.DecodeString(j.Attempt)
	if j.Version != 1 || e != nil || len(id) != 16 || hex.EncodeToString(id) != j.Attempt || len(j.History) == 0 || len(j.History) > 128 {
		return j, errors.New("invalid registration attempt")
	}
	previous := ""
	for _, event := range j.History {
		if !validRegistrationTransition(previous, event.Stage) {
			return j, errors.New("invalid registration history")
		}
		switch event.Stage {
		case "bootstrap-unavailable":
			if !validSecretEvidence(event.Failure) {
				return j, errors.New("invalid registration history")
			}
		case "secret-preflight-cooldown":
			if !registrationCooldownEvidence(event.Failure) {
				return j, errors.New("invalid registration history")
			}
		default:
			if event.Failure != nil {
				return j, errors.New("invalid registration history")
			}
		}
		previous = event.Stage
	}
	if j.Stage != previous {
		return j, errors.New("registration history mismatch")
	}
	return j, nil
}
func knownRegistrationStage(s string) bool {
	switch s {
	case "preflight", "secret-preflight-refused", "bootstrap-unavailable", "password-storage-pending", "password-store-failed", "remote-outcome-pending", "remote-outcome-ambiguous", "remote-rejected", "remote-response-mismatch", "remote-created-token-storage-pending", "remote-created-token-store-failed", "remote-created-metadata-publication-pending", "complete":
		return true
	}
	return false
}
func cmdConnectionRegistration(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 {
		return usagef("connection registration inspect|continue NAME")
	}
	fs := newFlagSet("connection registration")
	diagnostic := fs.Bool("authorize-diagnostic", false, "explicit admission for read-only local inspection")
	authorized := fs.Bool("authorize-registration", false, "separately owner-admitted continuation")
	attempt := fs.String("attempt", "", "original attempt ID")
	revision := fs.Int("revision", 0, "observed history length")
	pos, e := parseInterspersed(fs, args[1:])
	if e != nil {
		return e
	}
	inspect := args[0] == "inspect"
	if (!inspect && args[0] != "continue") || len(pos) != 1 {
		return usagef("connection registration inspect NAME --authorize-diagnostic | continue NAME --attempt ID --revision N --authorize-registration")
	}
	if inspect && (!*diagnostic || *authorized || *attempt != "" || *revision != 0) || !inspect && (!*authorized || *diagnostic || *attempt == "" || *revision < 1) {
		return errors.New("registration operation requires its separate explicit admission")
	}
	name := pos[0]
	if a.human {
		return errors.New("agent registration cannot use a human identity")
	}
	if e = config.ValidateName(name); e != nil {
		return errors.New("invalid registration identity")
	}
	for _, bound := range []string{a.as, a.Getenv(config.EnvAgent)} {
		if bound != "" && bound != name {
			return errors.New("registration does not match this session's identity")
		}
	}
	c, e := a.selectedConnection()
	if e != nil {
		return e
	}
	if c == nil {
		return errors.New("registration inspection/recovery requires a named connection")
	}
	b, e := c.Binding(name, false)
	if e != nil {
		return e
	}
	p, e := a.pathsFor(name)
	if e != nil {
		return e
	}
	st, e := os.Lstat(p.Base)
	if e != nil || !st.IsDir() || st.Mode().Perm() != 0700 {
		return errors.New("invalid private registration directory")
	}
	path := filepath.Join(p.Base, "registration.pending.json")
	// Read before lock only to identify legacy evidence. New evidence is reread
	// under the existing ownership lock. Missing locks are never manufactured.
	j, e := readRegistrationJournal(path, b)
	if e != nil {
		return e
	}
	if j.Version == 1 {
		unlock, e := lockRegistration(filepath.Join(p.Base, "registration.lock"), false)
		if e != nil {
			return errors.New("registration ownership unavailable or concurrent")
		}
		defer unlock()
		j, e = readRegistrationJournal(path, b)
		if e != nil {
			return e
		}
	}
	if inspect {
		failure := secretEvidence{Instrument: "unknown", Codes: []string{"unavailable"}}
		if len(j.History) > 0 && j.History[len(j.History)-1].Failure != nil {
			failure = *j.History[len(j.History)-1].Failure
		}
		// Projection excludes arbitrary journal fields, helper output and secret data.
		return json.NewEncoder(a.Stdout).Encode(struct {
			Attempt   string         `json:"attempt,omitempty"`
			Revision  int            `json:"revision"`
			Stage     string         `json:"stage"`
			Failure   secretEvidence `json:"failure"`
			Candidate bool           `json:"pre_http_candidate"`
		}{j.Attempt, len(j.History), j.Stage, failure, j.continuationCandidate()})
	}
	if !j.continuationCandidate() || j.Attempt != *attempt || len(j.History) != *revision || len(j.History) > 120 {
		return errors.New("attempt is stale or lacks validated pre-HTTP continuation evidence")
	}
	for _, path := range []string{p.AgentFile(name), p.HumanFile(name)} {
		if _, e = os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
			return errors.New("destination identity metadata exists or cannot be inspected")
		}
	}
	entries, e := os.ReadDir(p.Base)
	if e != nil {
		return errors.New("destination state unavailable")
	}
	for _, entry := range entries {
		if entry.Name() != "registration.pending.json" && entry.Name() != "registration.lock" && entry.Name() != "keychain-state" {
			return errors.New("contradictory destination state")
		}
	}
	if e = j.record(path, "continuation-admitted", nil); e != nil {
		return errors.New("continuation admission could not be recorded")
	}
	// Existing exclusive secret-store preflights and cooldown gates still apply.
	_, _, e = a.finishNamedRegistration(ctx, name, p, c, &j, path)
	return e
}
