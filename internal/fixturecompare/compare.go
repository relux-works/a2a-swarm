// Package fixturecompare records read-only diagnostics. Its results are never
// release verification and are not consumed by the release pipeline.
package fixturecompare

import (
	"bytes"
	"crypto/sha256"
	"debug/macho"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const Label = "NOT RELEASE VERIFICATION"

var names = [...]string{"a2a", "a2a-keychain-status"}

// StandaloneEnvironment admits only empty values for the markers used by the
// release workflows and signing entry points. There is no override in CI.
func StandaloneEnvironment(getenv func(string) string) error {
	for _, key := range []string{
		"CI", "GITHUB_ACTIONS", "RUNNER_ENVIRONMENT", "RUNNER_TEMP",
		"RELEASE_ADMITTED_TAG_OBJECT", "RELEASE_TAG",
		"MACOS_SIGN_P12", "MACOS_SIGN_P12_PASSWORD", "ASC_ISSUER_ID", "ASC_KEY_ID", "ASC_KEY_P8",
	} {
		if getenv(key) != "" {
			return errors.New("fixture-compare: standalone environment required; " + Label)
		}
	}
	return nil
}

type Image struct {
	Name                    string `json:"name"`
	SHA256                  string `json:"sha256"`
	DR                      string `json:"designated_requirement"`
	CompiledDR              string `json:"compiled_requirement_sha256"`
	HashVerified            bool   `json:"hash_verified"`
	StrictSignatureVerified bool   `json:"strict_signature_verified"`
}

type Finding struct {
	Code string `json:"code"`
	Side string `json:"side,omitempty"`
	Name string `json:"name"`
	Old  *Image `json:"old,omitempty"`
	New  *Image `json:"new,omitempty"`
}

type Report struct {
	Schema          string     `json:"schema"`
	Label           string     `json:"label"`
	ReleaseVerified bool       `json:"release_verified"`
	Completed       bool       `json:"completed"`
	Pairs           [2][]Image `json:"pairs"`
	Findings        []Finding  `json:"findings"`
	Error           string     `json:"error,omitempty"`
}

type signature struct {
	raw                       string
	compiled                  []byte
	timestamp, adHoc, runtime bool
}

type input struct {
	dir   string
	pins  []byte
	bytes [2][]byte
	stats [3]os.FileInfo
}

func hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Compare verifies pinned snapshots only inside a fresh evidence directory.
// Native inspection never executes an image, signs it or opens a Keychain.
func Compare(old, next, evidence string) (Report, error) {
	if err := StandaloneEnvironment(os.Getenv); err != nil {
		return Report{Label: Label}, err
	}
	return compare(old, next, evidence, inspect)
}

func compare(old, next, evidence string, verify func(string) (signature, error)) (report Report, err error) {
	report = Report{Schema: "a2a-fixture-comparison/v1", Label: Label, Findings: []Finding{}}
	var inputs [2]input
	for i, dir := range []string{old, next} {
		if inputs[i], err = readInput(dir); err != nil {
			return report, err
		}
	}
	evidence, err = evidencePath(evidence, inputs)
	if err != nil {
		return report, err
	}
	// An existing directory is refused, so evidence files cannot overwrite a
	// symlink or a hard link planted by an earlier run.
	if err = os.Mkdir(evidence, 0700); err != nil {
		return report, errors.New("fixture-evidence-must-be-new")
	}
	defer func() {
		for _, in := range inputs {
			if check := unchanged(in); check != nil {
				err = errors.Join(err, check)
			}
		}
		if err != nil {
			report.Completed = false
			report.Error = err.Error()
		}
		b, encodeErr := json.MarshalIndent(report, "", "  ")
		if encodeErr == nil {
			encodeErr = os.WriteFile(filepath.Join(evidence, "fixture-comparison.json"), append(b, '\n'), 0600)
		}
		err = errors.Join(err, encodeErr)
	}()
	var compiled [2][2][]byte
	for side, in := range inputs {
		sideName := []string{"N", "N+1"}[side]
		dir := filepath.Join(evidence, fmt.Sprint(side))
		if err = os.Mkdir(dir, 0700); err != nil {
			return report, err
		}
		if err = os.WriteFile(filepath.Join(dir, "SHA256SUMS"), in.pins, 0600); err != nil {
			return report, err
		}
		for i, name := range names {
			path := filepath.Join(dir, name)
			if err = os.WriteFile(path, in.bytes[i], 0500); err != nil {
				return report, err
			}
			sig, check := verify(path)
			if check != nil {
				return report, fmt.Errorf("%s/%s: %w", sideName, name, check)
			}
			// A missing native result is a failure, never empty-DR equality.
			if len(sig.compiled) == 0 || sig.raw == "" {
				return report, errors.New("fixture-designated-requirement-missing")
			}
			snapshot, check := os.ReadFile(path)
			if check != nil || !bytes.Equal(snapshot, in.bytes[i]) {
				return report, errors.New("fixture-snapshot-changed")
			}
			id := Image{Name: name, SHA256: hash(snapshot), DR: sig.raw, CompiledDR: hash(sig.compiled), HashVerified: true, StrictSignatureVerified: true}
			report.Pairs[side] = append(report.Pairs[side], id)
			compiled[side][i] = sig.compiled
			for _, finding := range []struct {
				present bool
				code    string
			}{
				{!sig.timestamp, "timestamp-missing"},
				{sig.adHoc && i == 1, "ad-hoc-companion"},
				{sig.adHoc && i == 0, "ad-hoc-signature"},
				{!sig.runtime, "hardened-runtime-missing"},
			} {
				if finding.present {
					report.Findings = append(report.Findings, Finding{Code: finding.code, Side: sideName, Name: name})
				}
			}
			if err = os.WriteFile(path+".requirement.bin", sig.compiled, 0600); err != nil {
				return report, err
			}
			if err = os.WriteFile(path+".requirement.txt", []byte(sig.raw+"\n"), 0600); err != nil {
				return report, err
			}
		}
	}
	for i, name := range names {
		if !bytes.Equal(compiled[0][i], compiled[1][i]) {
			report.Findings = append(report.Findings, Finding{Code: "requirements-differ", Name: name, Old: &report.Pairs[0][i], New: &report.Pairs[1][i]})
		}
	}
	report.Completed = true
	return report, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("fixture-read")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("fixture-read")
	}
	return b, nil
}

func readInput(dir string) (in input, err error) {
	in.dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return in, errors.New("fixture-directory-missing")
	}
	in.dir, err = filepath.Abs(in.dir)
	if err != nil {
		return in, err
	}
	entries, err := os.ReadDir(in.dir)
	if err != nil || len(entries) != 3 {
		return in, errors.New("fixture-members")
	}
	for i, name := range []string{names[0], names[1], "SHA256SUMS"} {
		path := filepath.Join(in.dir, name)
		st, err := os.Lstat(path)
		limit := int64(256 << 20)
		if i == 2 {
			limit = 4096
		}
		if err != nil || !st.Mode().IsRegular() || st.Size() <= 0 || st.Size() > limit || (i < 2 && st.Mode().Perm()&0111 == 0) {
			return in, errors.New("fixture-member")
		}
		in.stats[i] = st
		b, err := readBounded(path, limit)
		if err != nil {
			return in, err
		}
		if i < 2 {
			in.bytes[i] = b
		} else {
			in.pins = b
		}
	}
	pins, err := parsePins(in.pins)
	if err != nil {
		return in, err
	}
	for i, name := range names {
		if hash(in.bytes[i]) != pins[name] {
			return in, errors.New("fixture-hash-mismatch")
		}
	}
	return in, unchanged(in)
}

func parsePins(b []byte) (map[string]string, error) {
	pins := map[string]string{}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	for _, line := range lines {
		h, name, ok := strings.Cut(line, "  ")
		decoded, err := hex.DecodeString(h)
		if !ok || len(decoded) != sha256.Size || err != nil || strings.ToLower(h) != h || (name != names[0] && name != names[1]) || pins[name] != "" {
			return nil, errors.New("fixture-hash-manifest")
		}
		pins[name] = h
	}
	if len(pins) != 2 {
		return nil, errors.New("fixture-hash-manifest")
	}
	return pins, nil
}

func unchanged(in input) error {
	entries, err := os.ReadDir(in.dir)
	if err != nil || len(entries) != 3 {
		return errors.New("fixture-input-changed")
	}
	for i, name := range []string{names[0], names[1], "SHA256SUMS"} {
		path := filepath.Join(in.dir, name)
		st, err := os.Lstat(path)
		before := in.stats[i]
		if err != nil || !os.SameFile(before, st) || st.Mode() != before.Mode() || !st.ModTime().Equal(before.ModTime()) || st.Size() != before.Size() {
			return errors.New("fixture-input-changed")
		}
		b, err := readBounded(path, before.Size())
		want := in.pins
		if i < 2 {
			want = in.bytes[i]
		}
		if err != nil || !bytes.Equal(b, want) {
			return errors.New("fixture-input-changed")
		}
	}
	return nil
}

func evidencePath(path string, inputs [2]input) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", errors.New("fixture-evidence-parent-missing")
	}
	for _, in := range inputs {
		inputStat, err := os.Stat(in.dir)
		if err != nil {
			return "", err
		}
		// SameFile catches aliases and case-insensitive directory spellings.
		for p := parent; ; p = filepath.Dir(p) {
			st, err := os.Stat(p)
			if err != nil {
				return "", err
			}
			if os.SameFile(inputStat, st) {
				return "", errors.New("fixture-evidence-overlaps-input")
			}
			if filepath.Dir(p) == p {
				break
			}
		}
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

// inspectExecutable checks the supported image shape on the snapshot, never the
// input path. The signature still needs independent native strict validation.
func inspectExecutable(path string) error {
	f, err := macho.Open(path)
	if err != nil {
		return errors.New("fixture-mach-o-required")
	}
	defer f.Close()
	if f.Cpu != macho.CpuArm64 || f.Type != macho.TypeExec {
		return errors.New("fixture-darwin-arm64-executable-required")
	}
	return nil
}
