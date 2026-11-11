// Package darwinrelease is a product-specific release gate, not a runtime backend.
package darwinrelease

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var stableTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var objectID = regexp.MustCompile(`^[0-9a-f]{40}$`)
var ErrPolicy = errors.New("release policy refused")

const SignerFingerprint = "SHA256:63rp3DJz29F8BZCMoD8HbG+fgPaIat2wBRse0vRp34E"

// Source authority is separate from the single release-tag signer. These are
// existing approved public source keys; an edit cannot silently broaden trust.
var sourceSignerFingerprints = []string{
	"SHA256:1pl4mNUEP62BtX/d2Z28O0B77VlDUqzNTpsPitQzNiA",
	"SHA256:rE/sRlfdFt/vT5y5yukDCsq4aFtC48pbWsq9z4hm8MA",
	"SHA256:63rp3DJz29F8BZCMoD8HbG+fgPaIat2wBRse0vRp34E",
}

var SigningFields = []string{"MACOS_SIGN_P12", "MACOS_SIGN_P12_PASSWORD", "ASC_ISSUER_ID", "ASC_KEY_ID", "ASC_KEY_P8"}
var Binaries = []string{"a2a", "a2a-keychain-status"}

type Policy struct {
	Repository               string   `json:"repository"`
	HistoryBase              string   `json:"history_base"`
	SignerFingerprint        string   `json:"signer_fingerprint"`
	SourceSignerFingerprints []string `json:"source_signer_fingerprints"`
	Reviewer                 string   `json:"reviewer"`
	RequiredChecks           []string `json:"required_checks"`
	GoVersion                string   `json:"go_version"`
	XcodeVersion             string   `json:"xcode_version"`
	SwiftVersion             string   `json:"swift_version"`
}

func LoadPolicy(root string) (Policy, error) {
	var p Policy
	b, e := os.ReadFile(filepath.Join(root, "release/policy.json"))
	if e != nil {
		return p, ErrPolicy
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || p.Repository != "relux-works/a2a-swarm" || !objectID.MatchString(p.HistoryBase) || p.SignerFingerprint != SignerFingerprint || !slices.Equal(p.SourceSignerFingerprints, sourceSignerFingerprints) || p.Reviewer != "reluxbot" || len(p.RequiredChecks) == 0 || p.GoVersion == "" || p.XcodeVersion == "" || p.SwiftVersion == "" {
		return p, ErrPolicy
	}
	if d.Decode(new(any)) != io.EOF {
		return p, ErrPolicy
	}
	seen := map[string]bool{}
	for _, n := range p.RequiredChecks {
		if n == "" || seen[n] {
			return p, ErrPolicy
		}
		seen[n] = true
	}
	return p, nil
}

// SigningConfiguration never returns field values or makes native calls.
func SigningConfiguration(getenv func(string) string) error {
	for _, n := range SigningFields {
		if strings.TrimSpace(getenv(n)) == "" {
			return errors.New("release signing configuration requires all five nonempty fields")
		}
	}
	return nil
}

func CheckVersion(tag, version string) error {
	if !stableTag.MatchString(tag) || tag != "v"+strings.TrimSpace(version) {
		return errors.New("release tag/version refused")
	}
	return nil
}
func Requirement(name string) (string, error) {
	if name != "a2a" && name != "a2a-keychain-status" {
		return "", ErrPolicy
	}
	return `anchor apple generic and identifier "works.relux.` + name + `" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = "262RZ595FP"`, nil
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func writeJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return os.WriteFile(path, append(b, '\n'), 0600)
}
