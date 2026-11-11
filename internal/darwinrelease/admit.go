package darwinrelease

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

type Admission struct {
	Tag       string           `json:"tag"`
	TagObject string           `json:"tag_object"`
	Commit    string           `json:"commit"`
	Version   string           `json:"version"`
	ReviewID  int64            `json:"review_id"`
	Checks    map[string]int64 `json:"checks"`
}

type runGit func(...string) (string, error)

func localGit(root string) runGit {
	return func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(cleanEnvironment(), "GIT_NO_REPLACE_OBJECTS=1", "GIT_CONFIG_NOSYSTEM=1")
		b, e := cmd.Output()
		if e != nil {
			return "", errors.New("release Git check refused")
		}
		return strings.TrimSpace(string(b)), nil
	}
}

// verifyGit uses real signed Git objects; neither tag text nor a status fixture
// substitutes for signature verification. root/allowlist must be reviewed source.
func verifyGit(root, tag, version, expectedObject string, p Policy, g runGit) (Admission, error) {
	a := Admission{Tag: tag, Version: strings.TrimSpace(version)}
	if e := checkSignerFile(filepath.Join(root, ".github/release-signers"), p.SignerFingerprint); e != nil {
		return a, e
	}
	if e := checkSignerEntries(filepath.Join(root, ".github/source-signers"), "source-maintainer", p.SourceSignerFingerprints); e != nil {
		return a, errors.New("source-signer-policy")
	}
	if CheckVersion(tag, version) != nil {
		return a, errors.New("tag-version")
	}
	ref := "refs/tags/" + tag
	typ, e := g("cat-file", "-t", ref)
	if e != nil || typ != "tag" {
		return a, errors.New("tag-annotated")
	}
	raw, e := g("cat-file", "tag", ref)
	if e != nil || !strings.Contains(raw, "\n-----BEGIN SSH SIGNATURE-----") {
		return a, errors.New("tag-signed")
	}
	header := strings.SplitN(raw, "\n\n", 2)[0]
	if !strings.Contains("\n"+header+"\n", "\ntag "+tag+"\n") {
		return a, errors.New("tag-name")
	}
	a.TagObject, e = g("rev-parse", ref)
	if e != nil || !objectID.MatchString(a.TagObject) || (expectedObject != "" && a.TagObject != expectedObject) {
		return a, errors.New("tag-moved")
	}
	a.Commit, e = g("rev-parse", ref+"^{commit}")
	if e != nil || !objectID.MatchString(a.Commit) {
		return a, errors.New("tag-commit")
	}
	head, e := g("rev-parse", "HEAD")
	if e != nil || head != a.Commit {
		return a, errors.New("tag-checkout")
	}
	status, e := g("status", "--porcelain", "--untracked-files=all")
	if e != nil || status != "" {
		return a, errors.New("source-dirty")
	}
	// Ignore user-configured signature programs and trust files, and force SSH.
	cfg := []string{"-c", "gpg.format=ssh", "-c", "gpg.ssh.program=ssh-keygen", "-c", "gpg.openpgp.program=false", "-c", "gpg.x509.program=false", "-c", "gpg.ssh.allowedSignersFile=" + filepath.Join(root, ".github/release-signers")}
	if _, e = g(append(append([]string{}, cfg...), "verify-tag", ref)...); e != nil {
		return a, errors.New("tag-signer")
	}
	if _, e = g("merge-base", "--is-ancestor", p.HistoryBase, a.Commit); e != nil {
		return a, errors.New("history-base")
	}
	commits, e := g("rev-list", p.HistoryBase+".."+a.Commit)
	if e != nil {
		return a, errors.New("history-read")
	}
	// Every introduced object uses the source allowlist, including accepted
	// maintainer commits before this release change. The anchor is unchanged.
	sourceCfg := append([]string{}, cfg...)
	sourceCfg[len(sourceCfg)-1] = "gpg.ssh.allowedSignersFile=" + filepath.Join(root, ".github/source-signers")
	for _, commit := range strings.Fields(commits) {
		if _, e = g(append(append([]string{}, sourceCfg...), "verify-commit", commit)...); e != nil {
			return a, errors.New("commit-signer")
		}
		f, e := g(append(append([]string{}, sourceCfg...), "log", "-1", "--format=%G? %GF", commit)...)
		fields := strings.Fields(f)
		if e != nil || len(fields) != 2 || fields[0] != "G" || !slices.Contains(p.SourceSignerFingerprints, fields[1]) {
			return a, errors.New("commit-fingerprint")
		}
	}
	return a, nil
}

func Admit(ctx context.Context, root, tag, expectedObject string) (Admission, error) {
	p, e := LoadPolicy(root)
	if e != nil {
		return Admission{}, e
	}
	version, e := os.ReadFile(filepath.Join(root, "release/VERSION"))
	if e != nil {
		return Admission{}, ErrPolicy
	}
	a, e := verifyGit(root, tag, string(version), expectedObject, p, localGit(root))
	if e != nil {
		return a, e
	}
	api := newGitHub(os.Getenv("GITHUB_TOKEN"))
	if e = api.validate(ctx, p, &a); e != nil {
		return a, e
	}
	return a, nil
}

func checkSignerFile(path, fingerprint string) error {
	return checkSignerEntries(path, "release-maintainer", []string{fingerprint})
}

// Strict generic principals and exact ordered fingerprints prohibit wildcard,
// extra-key and ambient-trust fallback for both independent authorities.
func checkSignerEntries(path, principal string, fingerprints []string) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return errors.New("signer-policy")
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(fingerprints) == 0 || len(lines) != len(fingerprints) {
		return errors.New("signer-policy")
	}
	seen := map[string]bool{}
	for i, line := range lines {
		f := strings.Fields(line)
		if len(f) != 4 || f[0] != principal || f[1] != `namespaces="git"` || f[2] != "ssh-ed25519" || seen[f[3]] {
			return errors.New("signer-policy")
		}
		blob, e := base64.StdEncoding.DecodeString(f[3])
		if e != nil {
			return errors.New("signer-policy")
		}
		h := sha256.Sum256(blob)
		if "SHA256:"+base64.RawStdEncoding.EncodeToString(h[:]) != fingerprints[i] {
			return errors.New("signer-fingerprint")
		}
		seen[f[3]] = true
	}
	return nil
}
