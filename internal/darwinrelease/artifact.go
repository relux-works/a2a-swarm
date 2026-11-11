package darwinrelease

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"debug/macho"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Identity struct {
	Name       string `json:"name"`
	SHA256     string `json:"sha256"`
	DR         string `json:"designated_requirement"`
	CompiledDR string `json:"compiled_requirement_sha256"`
}

func VerifyBinary(path, name, evidence string) (Identity, error) {
	id := Identity{Name: name}
	expr, e := Requirement(name)
	if e != nil {
		return id, e
	}
	st, e := os.Lstat(path)
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
		return id, errors.New("companion-or-cli-missing")
	}
	f, e := macho.Open(path)
	if e != nil {
		return id, errors.New("mach-o-required")
	}
	defer f.Close()
	if f.Cpu != macho.CpuArm64 || f.Type != macho.TypeExec {
		return id, errors.New("darwin-arm64-executable-required")
	}
	raw, compiled, e := nativeVerify(path, expr)
	if e != nil {
		return id, e
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return id, errors.New("binary-read")
	}
	id.SHA256 = digest(b)
	id.DR = raw
	id.CompiledDR = digest(compiled)
	if e = os.WriteFile(filepath.Join(evidence, name+".requirement.bin"), compiled, 0600); e != nil {
		return id, e
	}
	if e = os.WriteFile(filepath.Join(evidence, name+".requirement.txt"), []byte(raw+"\n"), 0600); e != nil {
		return id, e
	}
	return id, nil
}

// CompareDR compares complete compiled requirements, not TeamIdentifier fields.
// Its input evidence alone is not signature validation or N-to-N+1 item access.
func CompareDR(old, new []Identity) error {
	if len(old) != 2 || len(new) != 2 {
		return errors.New("dr-pair-missing")
	}
	for i, name := range Binaries {
		if old[i].Name != name || new[i].Name != name || old[i].CompiledDR == "" || old[i].CompiledDR != new[i].CompiledDR || old[i].SHA256 == new[i].SHA256 {
			return errors.New("dr-transition-refused")
		}
	}
	return nil
}

var payloadNames = []string{"a2a", "a2a-keychain-status", "LICENSE", "NOTICE", "INSTALL.md"}

func payload(dir string) error {
	entries, e := os.ReadDir(dir)
	if e != nil || len(entries) != len(payloadNames) {
		return errors.New("package-members")
	}
	for _, name := range payloadNames {
		st, e := os.Lstat(filepath.Join(dir, name))
		if e != nil || !st.Mode().IsRegular() || st.Size() == 0 {
			return errors.New("package-companion-or-member")
		}
		if (name == "a2a" || name == "a2a-keychain-status") && st.Mode().Perm()&0111 == 0 {
			return errors.New("package-executable-mode")
		}
	}
	return nil
}
func makeZIP(dir, out string) error {
	if e := payload(dir); e != nil {
		return e
	}
	f, e := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	w := zip.NewWriter(f)
	for _, name := range payloadNames {
		b, e := os.ReadFile(filepath.Join(dir, name))
		if e != nil {
			w.Close()
			f.Close()
			return e
		}
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		mode := os.FileMode(0644)
		if name == "a2a" || name == "a2a-keychain-status" {
			mode = 0755
		}
		h.SetMode(mode)
		entry, e := w.CreateHeader(h)
		if e == nil {
			_, e = entry.Write(b)
		}
		if e != nil {
			w.Close()
			f.Close()
			return e
		}
	}
	if e = w.Close(); e != nil {
		f.Close()
		return e
	}
	return f.Close()
}
func makeArchive(dir, out string) error {
	if e := payload(dir); e != nil {
		return e
	}
	f, e := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, name := range payloadNames {
		b, e := os.ReadFile(filepath.Join(dir, name))
		if e != nil {
			tw.Close()
			gz.Close()
			f.Close()
			return e
		}
		mode := int64(0644)
		if name == "a2a" || name == "a2a-keychain-status" {
			mode = 0755
		}
		if e = tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(b)), Typeflag: tar.TypeReg}); e == nil {
			_, e = tw.Write(b)
		}
		if e != nil {
			tw.Close()
			gz.Close()
			f.Close()
			return e
		}
	}
	if e = tw.Close(); e != nil {
		gz.Close()
		f.Close()
		return e
	}
	if e = gz.Close(); e != nil {
		f.Close()
		return e
	}
	return f.Close()
}
func verifyArchive(path, dir string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	compressed := bufio.NewReader(f)
	gz, e := gzip.NewReader(compressed)
	if e != nil {
		return e
	}
	defer gz.Close()
	gz.Multistream(false)
	r := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		h, e := r.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		expectedMode := int64(0644)
		if h.Name == "a2a" || h.Name == "a2a-keychain-status" {
			expectedMode = 0755
		}
		if seen[h.Name] || h.Typeflag != tar.TypeReg || strings.Contains(h.Name, "/") || h.Mode != expectedMode {
			return errors.New("package-entry")
		}
		b, e := io.ReadAll(io.LimitReader(r, 256<<20))
		if e != nil {
			return e
		}
		original, e := os.ReadFile(filepath.Join(dir, h.Name))
		if e != nil || digest(b) != digest(original) {
			return errors.New("package-bytes")
		}
		seen[h.Name] = true
	}
	if len(seen) != len(payloadNames) {
		return errors.New("package-members")
	}
	for _, n := range payloadNames {
		if !seen[n] {
			return errors.New("package-companion")
		}
	}
	return finishArchiveStream(gz, compressed)
}

type NotaryResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func acceptedNotary(b []byte) (NotaryResult, error) {
	var n NotaryResult
	if json.Unmarshal(b, &n) != nil || n.Status != "Accepted" || !uuid.MatchString(n.ID) {
		return n, errors.New("notary-not-accepted")
	}
	return n, nil
}

func codeDirectoryHash(raw []byte) (string, error) {
	matches := regexp.MustCompile(`(?m)^CDHash=([0-9a-f]{40})$`).FindAllSubmatch(raw, -1)
	if len(matches) != 1 {
		return "", errors.New("code-directory-hash")
	}
	return string(matches[0][1]), nil
}

type NotaryLog struct {
	JobID      string `json:"jobId"`
	Status     string `json:"status"`
	StatusCode *int   `json:"statusCode"`
	SHA256     string `json:"sha256"`
	Tickets    []struct {
		Path   string `json:"path"`
		Arch   string `json:"arch"`
		CDHash string `json:"cdhash"`
	} `json:"ticketContents"`
}

// UUID identity tolerates alphabetic case without normalizing retained evidence.
func validateNotaryLog(data []byte, n NotaryResult, zipHash string, hashes map[string]string) (NotaryLog, error) {
	var record NotaryLog
	if n.Status != "Accepted" || !uuid.MatchString(n.ID) || json.Unmarshal(data, &record) != nil || !uuid.MatchString(record.JobID) || !strings.EqualFold(record.JobID, n.ID) || record.Status != "Accepted" || record.StatusCode == nil || *record.StatusCode != 0 || record.SHA256 != zipHash {
		return record, errors.New("notary-log-not-accepted")
	}
	seen := map[string]bool{}
	for _, ticket := range record.Tickets {
		name := filepath.Base(ticket.Path)
		if (name != "a2a" && name != "a2a-keychain-status") || ticket.Arch != "arm64" || len(hashes[name]) != 40 || ticket.CDHash != hashes[name] || seen[name] {
			return record, errors.New("notary-companion-ticket-missing")
		}
		seen[name] = true
	}
	if len(seen) != 2 {
		return record, errors.New("notary-companion-ticket-missing")
	}
	return record, nil
}

func matchCompiledRequirement(expected, actual []byte) error {
	if len(expected) == 0 || !bytes.Equal(expected, actual) {
		return errors.New("full-designated-requirement-mismatch")
	}
	return nil
}

// tar EOF is not gzip EOF. A ByteReader plus Multistream(false) preserves the
// first stream boundary so a valid second member is also unexpected input.
func finishArchiveStream(gz *gzip.Reader, compressed *bufio.Reader) error {
	n, e := io.Copy(io.Discard, gz)
	if e != nil {
		return errors.New("archive-gzip-integrity")
	}
	if e := gz.Close(); e != nil {
		return errors.New("archive-gzip-integrity")
	}
	if n != 0 {
		return errors.New("archive-gzip-trailing-data")
	}
	if _, e := compressed.ReadByte(); e != io.EOF {
		if e != nil {
			return errors.New("archive-gzip-integrity")
		}
		return errors.New("archive-gzip-trailing-data")
	}
	return nil
}
