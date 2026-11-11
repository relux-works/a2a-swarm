package darwinrelease

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

func archiveEvidence(dir, out string) error {
	es, e := os.ReadDir(dir)
	if e != nil {
		return e
	}
	f, e := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, entry := range es {
		if entry.IsDir() {
			return errors.New("evidence-entry")
		}
		b, e := os.ReadFile(filepath.Join(dir, entry.Name()))
		if e != nil {
			return e
		}
		if e = tw.WriteHeader(&tar.Header{Name: entry.Name(), Mode: 0600, Size: int64(len(b)), Typeflag: tar.TypeReg}); e != nil {
			return e
		}
		if _, e = tw.Write(b); e != nil {
			return e
		}
	}
	if e = tw.Close(); e != nil {
		return e
	}
	if e = gz.Close(); e != nil {
		return e
	}
	return f.Close()
}
func verifyAssetChecksums(dir string) error {
	data, e := os.ReadFile(filepath.Join(dir, "checksums.txt"))
	if e != nil {
		return errors.New("checksums-missing")
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		f := strings.Split(line, "  ")
		if len(f) != 2 || len(f[0]) != 64 || filepath.Base(f[1]) != f[1] || seen[f[1]] {
			return errors.New("checksums-malformed")
		}
		seen[f[1]] = true
		b, e := os.ReadFile(filepath.Join(dir, f[1]))
		if e != nil || digest(b) != f[0] {
			return errors.New("distributed-bytes-changed")
		}
	}
	archives := 0
	for name := range seen {
		if regexp.MustCompile(`^a2a_(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)_darwin_arm64\.tar\.gz$`).MatchString(name) {
			archives++
		} else if name != "evidence.tar.gz" && name != "release-evidence.json" {
			return errors.New("asset-unqualified")
		}
	}
	if archives != 1 || !seen["evidence.tar.gz"] || !seen["release-evidence.json"] {
		return errors.New("asset-members")
	}
	es, e := os.ReadDir(dir)
	if e != nil || len(seen) != 3 || len(es) != 6 {
		return errors.New("asset-members")
	}
	for _, entry := range es {
		if entry.IsDir() {
			return errors.New("asset-directory")
		}
		n := entry.Name()
		if !seen[n] && n != "checksums.txt" && n != "checksums.txt.sig" && n != "checksums.txt.pem" {
			return errors.New("asset-unqualified")
		}
		st, e := os.Lstat(filepath.Join(dir, n))
		if e != nil || !st.Mode().IsRegular() || st.Size() == 0 {
			return errors.New("asset-type")
		}
	}
	return nil
}

func publishAssets(ctx context.Context, root, tag, tagObject, dir string) (err error) {
	admitted, e := Admit(ctx, root, tag, tagObject)
	if e != nil {
		return e
	}
	if e = verifySignedAssets(ctx, root, tag, dir); e != nil {
		return e
	}
	api := newGitHub(os.Getenv("GITHUB_TOKEN"))
	prefix := "/repos/relux-works/a2a-swarm/releases"
	var existing struct{ ID int64 }
	status, e := api.request(ctx, "GET", prefix+"/tags/"+url.PathEscape(tag), nil, &existing)
	if e != nil {
		return e
	}
	if status != 404 {
		return errors.New("release-already-exists")
	}
	var draft struct {
		ID    int64
		Draft bool
	}
	if _, e = api.request(ctx, "POST", prefix, map[string]any{"tag_name": tag, "target_commitish": admitted.Commit, "name": "a2a " + tag, "draft": true, "prerelease": false, "body": "Darwin arm64 CLI and sibling status companion. Verify checksums and release evidence before separately authorized adoption. Native Keychain/no-UI and cross-version qualification remain separate."}, &draft); e != nil {
		return e
	}
	if draft.ID <= 0 || !draft.Draft {
		return errors.New("release-draft-response")
	}
	path := prefix + "/" + strconv.FormatInt(draft.ID, 10)
	// A private draft may remain on failure. GitHub has no conditional draft-only
	// delete: never risk deleting a release after an ambiguous public transition.
	es, e := os.ReadDir(dir)
	if e != nil {
		return e
	}
	for _, entry := range es {
		file, e := os.Open(filepath.Join(dir, entry.Name()))
		if e != nil {
			return e
		}
		st, e := file.Stat()
		if e != nil {
			file.Close()
			return e
		}
		endpoint := fmt.Sprintf("https://uploads.github.com/repos/relux-works/a2a-swarm/releases/%d/assets?name=%s", draft.ID, url.QueryEscape(entry.Name()))
		req, e := http.NewRequestWithContext(ctx, "POST", endpoint, file)
		if e != nil {
			file.Close()
			return errors.New("asset-request")
		}
		req.ContentLength = st.Size()
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Authorization", "Bearer "+api.token)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		resp, e := api.client.Do(req)
		file.Close()
		if e != nil {
			return errors.New("asset-upload")
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != 201 || readErr != nil {
			return errors.New("asset-upload-status")
		}
		var uploaded struct {
			ID                  int64
			Name, Digest, State string
			Size                int64
		}
		if json.Unmarshal(body, &uploaded) != nil || uploaded.ID <= 0 || uploaded.Name != entry.Name() || uploaded.State != "uploaded" || uploaded.Size != st.Size() {
			return errors.New("asset-upload-malformed")
		}
		bytes, e := os.ReadFile(filepath.Join(dir, entry.Name()))
		if e != nil || uploaded.Digest != "sha256:"+digest(bytes) {
			return errors.New("uploaded-bytes-mismatch")
		}
	}
	if _, e = Admit(ctx, root, tag, tagObject); e != nil {
		return e
	}
	if e = verifySignedAssets(ctx, root, tag, dir); e != nil {
		return e
	}
	var published struct {
		ID    int64
		Draft *bool
		Tag   string `json:"tag_name"`
	}
	if _, e = api.request(ctx, "PATCH", path, map[string]bool{"draft": false}, &published); e != nil {
		return errors.New("publication-outcome-unknown; inspect without retry or deletion")
	}
	if published.ID != draft.ID || published.Draft == nil || *published.Draft || published.Tag != tag {
		return errors.New("publication-readback-unknown")
	}
	return nil
}
