package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func validRegistrationTransition(from, to string) bool {
	switch from {
	case "":
		return to == "preflight"
	case "continuation-admitted", "legacy-continuation-admitted":
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
	sum := sha256.Sum256(data)
	j.sourceSHA256 = hex.EncodeToString(sum[:])
	// Consumer fingerprints intentionally exclude bootstrap references. Recovery
	// additionally pins that original admission input; changing it is not recovery.
	originalRef, selectedRef := j.Binding.Connection.RegistrationTokenRef, b.Connection.RegistrationTokenRef
	if j.Binding.Connection.Validate() != nil || j.Binding.Fingerprint() != b.Fingerprint() || originalRef == nil || selectedRef == nil || *originalRef != *selectedRef {
		return j, errors.New("registration binding mismatch")
	}
	if j.Version == 0 && j.Attempt == "" && len(j.History) == 0 {
		// Accept only the actual two-field v0 schema. Optional modern fields, even
		// empty ones, cannot masquerade as the original receipt.
		var legacy struct {
			Binding config.ConnectionBinding `json:"binding"`
			Stage   string                   `json:"stage"`
		}
		if config.DecodeConnectionJSON(data, &legacy) != nil {
			return j, errors.New("invalid legacy registration evidence")
		}
		// encoding/json also accepts case aliases (including conflicting
		// Stage/stage keys). Require the original field names throughout the
		// binding without requiring its original whitespace or field order.
		var supplied, projected any
		if json.Unmarshal(data, &supplied) != nil {
			return j, errors.New("invalid legacy registration evidence")
		}
		canonical, _ := json.Marshal(legacy)
		if json.Unmarshal(canonical, &projected) != nil || !reflect.DeepEqual(supplied, projected) {
			return j, errors.New("invalid legacy registration evidence")
		}
		// This receipt has no historical ownership, revision or failure-cause proof.
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
	legacy := j.LegacySHA256 != ""
	if legacy && (!validRegistrationDigest(j.LegacySHA256) || j.History[0].Stage != "legacy-continuation-admitted") || !legacy && j.History[0].Stage != "preflight" {
		return j, errors.New("invalid registration provenance")
	}
	for i, event := range j.History {
		if !(legacy && i == 0 && event.Stage == "legacy-continuation-admitted") && !validRegistrationTransition(previous, event.Stage) {
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

// A legacy disposition is separate from the retained original bytes. Only its
// new admission root authorizes current cooperative ownership; no old lock or
// history is inferred. Every later read revalidates the source digest/binding.
func validRegistrationDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == sha256.Size && hex.EncodeToString(b) == s
}
func readRegistrationState(base string, b config.ConnectionBinding) (registrationJournal, string, error) {
	path := filepath.Join(base, "registration.pending.json")
	original, e := readRegistrationJournal(path, b)
	if e != nil {
		return original, path, e
	}
	if original.LegacySHA256 != "" {
		return original, path, errors.New("legacy disposition cannot replace original receipt")
	}
	nextPath := filepath.Join(base, "registration.continuation.json")
	if _, e = os.Lstat(nextPath); errors.Is(e, os.ErrNotExist) {
		return original, path, nil
	}
	if e != nil || original.Version != 0 || original.Stage != "bootstrap-unavailable" {
		return original, path, errors.New("contradictory registration disposition")
	}
	next, e := readRegistrationJournal(nextPath, b)
	if e != nil || next.Version != 1 || next.LegacySHA256 != original.sourceSHA256 {
		return original, path, errors.New("invalid legacy disposition provenance")
	}
	return next, nextPath, nil
}
func registrationDestination(p config.Paths, name string, lock, disposition bool) error {
	for _, path := range []string{p.AgentFile(name), p.HumanFile(name)} {
		if _, e := os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
			return errors.New("destination identity metadata exists or cannot be inspected")
		}
	}
	entries, e := os.ReadDir(p.Base)
	if e != nil {
		return errors.New("destination state unavailable")
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "registration.pending.json":
		case "keychain-state":
			st, e := entry.Info()
			if e != nil || !st.IsDir() || st.Mode().Perm() != 0700 {
				return errors.New("invalid private credential state")
			}
		case "registration.lock":
			if lock {
				continue
			}
			return errors.New("registration ownership unknown or concurrent")
		case "registration.continuation.json":
			if disposition {
				continue
			}
			return errors.New("contradictory registration disposition")
		default:
			return errors.New("contradictory destination state")
		}
	}
	return nil
}
func cmdConnectionRegistration(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 {
		return usagef("connection registration inspect|continue|continue-legacy NAME")
	}
	fs := newFlagSet("connection registration")
	diagnostic := fs.Bool("authorize-diagnostic", false, "explicit admission for read-only local inspection")
	authorized := fs.Bool("authorize-registration", false, "separately owner-admitted continuation")
	attempt := fs.String("attempt", "", "original attempt or new disposition ID")
	revision := fs.Int("revision", 0, "observed history length")
	digest := fs.String("legacy-sha256", "", "exact original v0 receipt digest from inspection")
	pos, e := parseInterspersed(fs, args[1:])
	if e != nil {
		return e
	}
	inspect, legacy := args[0] == "inspect", args[0] == "continue-legacy"
	if (!inspect && !legacy && args[0] != "continue") || len(pos) != 1 {
		return usagef("connection registration inspect|continue|continue-legacy NAME")
	}
	if inspect && (!*diagnostic || *authorized || *attempt != "" || *revision != 0 || *digest != "") ||
		legacy && (!*authorized || *diagnostic || !validRegistrationDigest(*digest) || *attempt != "" || *revision != 0) ||
		!inspect && !legacy && (!*authorized || *diagnostic || *attempt == "" || *revision < 1 || *digest != "") {
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
	j, path, e := readRegistrationState(p.Base, b)
	if e != nil {
		return e
	}
	_, lockErr := os.Lstat(filepath.Join(p.Base, "registration.lock"))
	if j.Version == 0 && !inspect && !errors.Is(lockErr, os.ErrNotExist) {
		return errors.New("registration ownership unknown or concurrent")
	}
	// Inspection may read a legacy receipt alongside an existing regular lock,
	// but acquiring it now supplies no historical ownership or continuation right.
	if j.Version == 1 || inspect && !errors.Is(lockErr, os.ErrNotExist) {
		unlock, e := lockRegistration(filepath.Join(p.Base, "registration.lock"), false)
		if e != nil {
			return errors.New("registration ownership unavailable or concurrent")
		}
		defer unlock()
		j, path, e = readRegistrationState(p.Base, b)
		if e != nil {
			return e
		}
	}
	if inspect {
		failure := secretEvidence{Instrument: "unknown", Codes: []string{"unavailable"}}
		if len(j.History) > 0 && j.History[len(j.History)-1].Failure != nil {
			failure = *j.History[len(j.History)-1].Failure
		}
		legacyDigest := j.LegacySHA256
		if j.Version == 0 {
			legacyDigest = j.sourceSHA256
		}
		// Projection excludes arbitrary journal fields, helper output and secret data.
		return json.NewEncoder(a.Stdout).Encode(struct {
			Attempt      string         `json:"attempt,omitempty"`
			Revision     int            `json:"revision"`
			Stage        string         `json:"stage"`
			Failure      secretEvidence `json:"failure"`
			Candidate    bool           `json:"pre_http_candidate"`
			LegacySHA256 string         `json:"legacy_sha256,omitempty"`
		}{j.Attempt, len(j.History), j.Stage, failure, j.continuationCandidate(), legacyDigest})
	}
	if legacy {
		if j.Version != 0 || j.Stage != "bootstrap-unavailable" || j.sourceSHA256 != *digest {
			return errors.New("legacy receipt is stale or lacks pre-HTTP evidence")
		}
		if e = registrationDestination(p, name, false, false); e != nil {
			return e
		}
		// This lock starts NOW, under the separately admitted disposition. An existing
		// unexplained lock is a refusal, never adopted as historical ownership.
		unlock, e := lockRegistration(filepath.Join(p.Base, "registration.lock"), true)
		if e != nil {
			return errors.New("registration ownership unavailable or concurrent")
		}
		defer unlock()
		original, _, e := readRegistrationState(p.Base, b)
		if e != nil || original.Version != 0 || original.Stage != "bootstrap-unavailable" || original.sourceSHA256 != *digest {
			return errors.New("legacy receipt changed during admission")
		}
		if e = registrationDestination(p, name, true, false); e != nil {
			return e
		}
		var id [16]byte
		if _, e = rand.Read(id[:]); e != nil {
			return errors.New("legacy disposition ID unavailable")
		}
		j = registrationJournal{Binding: b, Version: 1, Attempt: hex.EncodeToString(id[:]), LegacySHA256: *digest}
		path = filepath.Join(p.Base, "registration.continuation.json")
		if e = j.record(path, "legacy-continuation-admitted", nil); e != nil {
			return errors.New("legacy admission could not be recorded")
		}
	} else {
		if !j.continuationCandidate() || j.Attempt != *attempt || len(j.History) != *revision || len(j.History) > 120 {
			return errors.New("attempt is stale or lacks validated pre-HTTP continuation evidence")
		}
		if e = registrationDestination(p, name, true, j.LegacySHA256 != ""); e != nil {
			return e
		}
		if e = j.record(path, "continuation-admitted", nil); e != nil {
			return errors.New("continuation admission could not be recorded")
		}
	}
	// Existing exclusive secret-store preflights and cooldown gates still apply.
	_, _, e = a.finishNamedRegistration(ctx, name, p, c, &j, path)
	return e
}
