package darwinrelease

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

var uuid = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var keyID = regexp.MustCompile(`^[A-Z0-9]{10}$`)

func cleanEnvironment() []string {
	var out []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		blocked := name == "GITHUB_TOKEN" || name == "GH_TOKEN" || strings.HasPrefix(name, "GIT_") || strings.HasPrefix(name, "ACTIONS_ID_TOKEN_") || name == "GOFLAGS" || name == "GOWORK" || name == "GOENV" || name == "GOTOOLCHAIN" || name == "GOOS" || name == "GOARCH" || strings.HasPrefix(name, "CGO_")
		for _, n := range SigningFields {
			blocked = blocked || name == n
		}
		if !blocked {
			out = append(out, entry)
		}
	}
	return out
}

// execute never includes argv, stderr or subprocess error text in a returned
// failure. Apple fields are stripped before every subprocess, including builds.
func execute(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = dir
	c.Env = cleanEnvironment()
	b, e := c.Output()
	if e != nil {
		return nil, errors.New("release subprocess refused")
	}
	return b, nil
}

type Evidence struct {
	Admission           Admission         `json:"admission"`
	Identities          []Identity        `json:"identities"`
	Notary              NotaryResult      `json:"notary"`
	NotaryZIP           string            `json:"notary_zip_sha256"`
	Toolchain           map[string]string `json:"toolchain"`
	NativeQualification string            `json:"native_qualification"`
}

// Prepare builds only after the caller's five-field preflight. There is one real
// native path; no unsigned fallback or GoReleaser stage can replace signatures.
func Prepare(ctx context.Context, root, tag, tagObject string) (out string, err error) {
	if SigningConfiguration(os.Getenv) != nil {
		return "", errors.New("signing-config")
	}
	values := map[string]string{}
	for _, n := range SigningFields {
		values[n] = os.Getenv(n)
		_ = os.Unsetenv(n)
	}
	if !uuid.MatchString(values["ASC_ISSUER_ID"]) || !keyID.MatchString(values["ASC_KEY_ID"]) {
		return "", errors.New("notary-identifiers")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return "", errors.New("native-darwin-arm64-runner-required")
	}
	admission, e := Admit(ctx, root, tag, tagObject)
	if e != nil {
		return "", e
	}
	policy, e := LoadPolicy(root)
	if e != nil {
		return "", e
	}
	if runtime.Version() != policy.GoVersion {
		return "", errors.New("go-toolchain-mismatch")
	}
	toolchain := map[string]string{"go": runtime.Version()}
	for _, name := range Binaries {
		expr, _ := Requirement(name)
		if _, e := CompileRequirement(expr); e != nil {
			return "", e
		}
	}
	cosignVersion, e := execute(ctx, root, "cosign", "version")
	if e != nil || !regexp.MustCompile(`(?m)^GitVersion:\s+v2\.6\.1\s*$`).Match(cosignVersion) {
		return "", errors.New("cosign-version-mismatch")
	}
	toolchain["cosign"] = string(cosignVersion)
	gitVersion, e := execute(ctx, root, "git", "--version")
	if e != nil {
		return "", errors.New("git-version-evidence")
	}
	toolchain["git"] = string(gitVersion)
	xcode, e := execute(ctx, root, "/usr/bin/xcodebuild", "-version")
	if e != nil || !strings.HasPrefix(string(xcode), "Xcode "+policy.XcodeVersion+"\n") {
		return "", errors.New("xcode-mismatch")
	}
	toolchain["xcode"] = string(xcode)
	swift, e := execute(ctx, root, "/usr/bin/xcrun", "swiftc", "--version")
	if e != nil || !strings.Contains(string(swift), "Apple Swift version "+policy.SwiftVersion+" ") {
		return "", errors.New("swift-mismatch")
	}
	toolchain["swift"] = string(swift)
	for name, args := range map[string][]string{"macOS": {"/usr/bin/sw_vers"}, "SDK": {"/usr/bin/xcrun", "--sdk", "macosx", "--show-sdk-version"}, "clang": {"/usr/bin/xcrun", "clang", "--version"}, "notarytool": {"/usr/bin/xcrun", "notarytool", "--version"}} {
		b, e := execute(ctx, root, args[0], args[1:]...)
		if e != nil {
			return "", errors.New("toolchain-evidence")
		}
		toolchain[name] = string(b)
	}
	temp := os.Getenv("RUNNER_TEMP")
	if !filepath.IsAbs(temp) {
		return "", errors.New("runner-temp-required")
	}
	private := filepath.Join(temp, "a2a-release-private")
	if e = os.Mkdir(private, 0700); e != nil {
		return "", errors.New("private-runner-directory")
	}
	var signer *signingKeychain
	var createdOutput string
	defer func() {
		if signer != nil {
			if e := signer.close(); e != nil {
				err = e
			}
		}
		if e := os.RemoveAll(private); e != nil {
			err = errors.New("private-cleanup")
		}
		if err != nil && createdOutput != "" {
			_ = os.RemoveAll(createdOutput)
			out = ""
		}
	}()
	work := filepath.Join(private, "payload")
	proof := filepath.Join(private, "evidence")
	for _, dir := range []string{work, proof} {
		if e = os.Mkdir(dir, 0700); e != nil {
			return "", e
		}
	}
	for _, name := range []string{"LICENSE", "NOTICE"} {
		if e = copyFile(filepath.Join(root, name), filepath.Join(work, name), 0644); e != nil {
			return "", e
		}
	}
	if e = copyFile(filepath.Join(root, "release/INSTALL.md"), filepath.Join(work, "INSTALL.md"), 0644); e != nil {
		return "", e
	}
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-buildvcs=true", "-o", filepath.Join(work, "a2a"), "./cmd/a2a")
	build.Dir = root
	build.Env = append(cleanEnvironment(), "CGO_ENABLED=1", "GOOS=darwin", "GOARCH=arm64", "GOFLAGS=-mod=readonly", "GOTOOLCHAIN=local", "GOWORK=off", "GOENV=off", "MACOSX_DEPLOYMENT_TARGET=13.0")
	if e = build.Run(); e != nil {
		return "", errors.New("native-cli-build")
	}
	if _, e = execute(ctx, root, "/usr/bin/xcrun", "swiftc", "-O", "-target", "arm64-apple-macos13.0", "-o", filepath.Join(work, "a2a-keychain-status"), "cmd/a2a-keychain-status/main.swift"); e != nil {
		return "", errors.New("swift-companion-build")
	}
	signer, e = openSigningKeychain(filepath.Join(private, "signing.keychain-db"), values)
	if e != nil {
		return "", e
	}
	ids := []Identity{}
	cdhashes := map[string]string{}
	for _, name := range Binaries {
		expr, _ := Requirement(name)
		onDisk, e := os.ReadFile(filepath.Join(root, "release/requirements", name+".txt"))
		if e != nil || strings.TrimSpace(string(onDisk)) != expr {
			return "", errors.New("requirement-policy-drift")
		}
		path := filepath.Join(work, name)
		if _, e = execute(ctx, root, "/usr/bin/codesign", "--force", "--sign", signer.selector, "--keychain", signer.path, "--identifier", "works.relux."+name, "--options", "runtime", "--timestamp", "--requirements", "=designated => "+expr, path); e != nil {
			return "", errors.New("binary-sign")
		}
		if _, e = execute(ctx, root, "/usr/bin/codesign", "--verify", "--strict", "--verbose=4", "--test-requirement", "="+expr, path); e != nil {
			return "", errors.New("binary-signature-check")
		}
		// Display writes to stderr. Capture only after signature+native full-DR gate;
		// commands contain no Apple secrets, and display cannot access key material.
		id, e := VerifyBinary(path, name, proof)
		if e != nil {
			return "", e
		}
		ids = append(ids, id)
		display := exec.CommandContext(ctx, "/usr/bin/codesign", "--display", "--verbose=4", "--requirements", "-", path)
		display.Env = cleanEnvironment()
		raw, e := display.CombinedOutput()
		if e != nil {
			return "", errors.New("signature-display")
		}
		hash, e := codeDirectoryHash(raw)
		if e != nil {
			return "", e
		}
		cdhashes[name] = hash
		if e = os.WriteFile(filepath.Join(proof, name+".codesign.txt"), raw, 0600); e != nil {
			return "", e
		}
	}
	if e = signer.close(); e != nil {
		return "", e
	}
	signer = nil
	submission := filepath.Join(private, "notarize.zip")
	if e = makeZIP(work, submission); e != nil {
		return "", e
	}
	key := filepath.Join(private, "notary.p8")
	if e = os.WriteFile(key, []byte(values["ASC_KEY_P8"]), 0600); e != nil {
		return "", errors.New("private-notary-file")
	}
	// Public issuer/key IDs and a private key PATH are the supported Apple API.
	// Never log argv or output on failure; never pass P8/P12/password bytes.
	response, e := execute(ctx, root, "/usr/bin/xcrun", "notarytool", "submit", submission, "--key", key, "--key-id", values["ASC_KEY_ID"], "--issuer", values["ASC_ISSUER_ID"], "--wait", "--timeout", "20m", "--output-format", "json")
	if e != nil {
		return "", errors.New("notary-submit")
	}
	notarized, e := acceptedNotary(response)
	if e != nil {
		return "", e
	}
	logPath := filepath.Join(private, "notary-log.json")
	_, e = execute(ctx, root, "/usr/bin/xcrun", "notarytool", "log", notarized.ID, logPath, "--key", key, "--key-id", values["ASC_KEY_ID"], "--issuer", values["ASC_ISSUER_ID"])
	if e != nil {
		return "", errors.New("notary-log")
	}
	log, e := os.ReadFile(logPath)
	if e != nil {
		return "", errors.New("notary-log-read")
	}
	zipBytes, e := os.ReadFile(submission)
	if e != nil {
		return "", e
	}
	record, e := validateNotaryLog(log, notarized, digest(zipBytes), cdhashes)
	if e != nil {
		return "", e
	}
	if e = writeJSON(filepath.Join(proof, "notary.json"), record); e != nil {
		return "", e
	}
	for i, name := range Binaries {
		id, e := VerifyBinary(filepath.Join(work, name), name, proof)
		if e != nil || id != ids[i] {
			return "", errors.New("signed-bytes-changed-after-notary")
		}
	}
	if e = os.Remove(key); e != nil {
		return "", errors.New("notary-key-cleanup")
	}
	out = filepath.Join(temp, "a2a-release-output")
	if e = os.Mkdir(out, 0700); e != nil {
		return "", errors.New("output-exists")
	}
	createdOutput = out
	archive := filepath.Join(out, "a2a_"+admission.Version+"_darwin_arm64.tar.gz")
	if e = makeArchive(work, archive); e != nil {
		return "", e
	}
	if e = verifyArchive(archive, work); e != nil {
		return "", e
	}
	evidence := Evidence{admission, ids, notarized, digest(zipBytes), toolchain, "source workflow is not native Keychain/no-UI/N-to-N+1/adoption qualification"}
	if e = writeJSON(filepath.Join(proof, "release-evidence.json"), evidence); e != nil {
		return "", e
	}
	if e = rejectCredentialBytes(work, values); e != nil {
		return "", e
	}
	if e = rejectCredentialBytes(proof, values); e != nil {
		return "", e
	}
	for n := range values {
		values[n] = ""
	}
	proofArchive := filepath.Join(out, "evidence.tar.gz")
	if e = archiveEvidence(proof, proofArchive); e != nil {
		return "", e
	}
	if e = copyFile(filepath.Join(proof, "release-evidence.json"), filepath.Join(out, "release-evidence.json"), 0600); e != nil {
		return "", e
	}
	if e = checksums(out); e != nil {
		return "", e
	}
	return out, nil
}
func copyFile(from, to string, mode os.FileMode) error {
	b, e := os.ReadFile(from)
	if e != nil {
		return errors.New("required-source-file")
	}
	return os.WriteFile(to, b, mode)
}
func checksums(dir string) error {
	es, e := os.ReadDir(dir)
	if e != nil {
		return e
	}
	sort.Slice(es, func(i, j int) bool { return es[i].Name() < es[j].Name() })
	var b strings.Builder
	for _, entry := range es {
		if entry.IsDir() || strings.ContainsAny(entry.Name(), "\n\r") {
			return errors.New("asset-name")
		}
		data, e := os.ReadFile(filepath.Join(dir, entry.Name()))
		if e != nil {
			return e
		}
		fmt.Fprintf(&b, "%s  %s\n", digest(data), entry.Name())
	}
	return os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(b.String()), 0600)
}

// PublishAfter is the single publication boundary. Every prerequisite and the
// last ref/check revalidation runs before calling the irreversible publisher.
func PublishAfter(gates []func() error, publish func() error) error {
	for _, gate := range gates {
		if e := gate(); e != nil {
			return e
		}
	}
	return publish()
}

func RunRelease(ctx context.Context, root, tag, tagObject string) error {
	var out string
	gates := []func() error{
		func() error { var e error; out, e = Prepare(ctx, root, tag, tagObject); return e },
		func() error {
			sum := filepath.Join(out, "checksums.txt")
			signature := filepath.Join(out, "checksums.txt.sig")
			cert := filepath.Join(out, "checksums.txt.pem")
			c := exec.CommandContext(ctx, "cosign", "sign-blob", "--yes", "--output-signature", signature, "--output-certificate", cert, sum)
			c.Env = oidcEnvironment()
			if e := c.Run(); e != nil {
				return errors.New("checksum-sign")
			}
			return nil
		},
		func() error { _, e := Admit(ctx, root, tag, tagObject); return e },
		func() error { return verifySignedAssets(ctx, root, tag, out) },
	}
	return PublishAfter(gates, func() error { return publishAssets(ctx, root, tag, tagObject, out) })
}
func verifySignedAssets(ctx context.Context, root, tag, out string) error {
	if e := verifyAssetChecksums(out); e != nil {
		return e
	}
	identity := "https://github.com/relux-works/a2a-swarm/.github/workflows/darwin-release.yml@refs/tags/" + tag
	if _, e := execute(ctx, root, "cosign", "verify-blob", "--signature", filepath.Join(out, "checksums.txt.sig"), "--certificate", filepath.Join(out, "checksums.txt.pem"), "--certificate-identity", identity, "--certificate-oidc-issuer", "https://token.actions.githubusercontent.com", filepath.Join(out, "checksums.txt")); e != nil {
		return errors.New("checksum-signature-check")
	}
	return nil
}

func rejectCredentialBytes(dir string, values map[string]string) error {
	patterns := [][]byte{}
	for _, value := range values {
		if value != "" {
			patterns = append(patterns, []byte(value))
		}
	}
	if decoded, e := base64.StdEncoding.DecodeString(values["MACOS_SIGN_P12"]); e == nil && len(decoded) > 0 {
		patterns = append(patterns, decoded)
	}
	return filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return errors.New("output-inspection")
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("output-symlink")
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return errors.New("output-inspection")
		}
		for _, secret := range patterns {
			if bytes.Contains(data, secret) {
				return errors.New("private-material-in-output")
			}
		}
		return nil
	})
}

func oidcEnvironment() []string {
	env := cleanEnvironment()
	for _, n := range []string{"ACTIONS_ID_TOKEN_REQUEST_URL", "ACTIONS_ID_TOKEN_REQUEST_TOKEN"} {
		if value := os.Getenv(n); value != "" {
			env = append(env, n+"="+value)
		}
	}
	return env
}
