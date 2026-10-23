package voice

import (
	"archive/tar"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const ModelURL = "https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/vits-piper-ru_RU-irina-medium.tar.bz2"
const ModelSHA256 = "1fc0f54e5e084fe287c07909f2f6e0ba6d857864cf800e3ab80286a4e8233008"
const manifestSHA256 = "e7c154c19f233349ff4c3578316b43cb144f594af9411aab4814dbfd69a63392"
const modelName = "vits-piper-ru_RU-irina-medium"
const maxArchiveBytes = 80 << 20
const maxExtractBytes = 160 << 20

func ModelDir(base string) string { return filepath.Join(Dir(base), "models", ModelSHA256) }
func digest(b []byte) string      { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// Install is the only model network path. Post/Render never call it. Extraction
// starts only after checking the entire pinned archive; publication is atomic.
func Install(ctx context.Context, base string, client *http.Client) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if _, err := os.Lstat(ModelDir(base)); err == nil {
		return ValidateModel(ctx, base)
	} else if !os.IsNotExist(err) {
		return err
	}
	root := filepath.Dir(ModelDir(base))
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(root, ".install-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelURL, nil)
	if err != nil {
		return err
	}
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("voice model download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("voice model download refused")
	}
	archive := filepath.Join(dir, "model.tar.bz2")
	f, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxArchiveBytes+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || n > maxArchiveBytes {
		return errors.New("voice model download incomplete or oversized")
	}
	if hex.EncodeToString(h.Sum(nil)) != ModelSHA256 {
		return errors.New("voice model sha256 mismatch; nothing installed")
	}
	archiveFile, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer archiveFile.Close()
	extracted := filepath.Join(dir, "extracted")
	if err = os.Mkdir(extracted, 0700); err != nil {
		return err
	}
	if err = extract(ctx, bzip2.NewReader(archiveFile), extracted, manifestSHA256); err != nil {
		return err
	}
	return os.Rename(extracted, ModelDir(base))
}

func extract(ctx context.Context, reader io.Reader, dest, expectedManifest string) error {
	tr := tar.NewReader(io.LimitReader(reader, maxExtractBytes+1))
	files := map[string]string{}
	seen := map[string]bool{}
	var total int64
	entries := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("invalid voice model archive")
		}
		entries++
		if entries > 1000 {
			return errors.New("voice model archive has too many entries")
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		if name == modelName && hdr.Typeflag == tar.TypeDir {
			continue
		}
		rel, ok := strings.CutPrefix(name, modelName+"/")
		if !ok || rel == "" || rel == ".." || rel == "." || path.Clean(rel) != rel || strings.HasPrefix(rel, "../") || strings.Contains(rel, "\\") || path.IsAbs(rel) || seen[rel] {
			return errors.New("unsafe voice model archive path")
		}
		seen[rel] = true
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0700); err != nil {
				return err
			}
			continue
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Size < 0 || hdr.Size > maxExtractBytes-total {
			return errors.New("unsupported or oversized voice model archive entry")
		}
		total += hdr.Size
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, h), tr)
		cerr := f.Close()
		if err != nil || cerr != nil || n != hdr.Size {
			return errors.New("incomplete voice model entry")
		}
		files[rel] = hex.EncodeToString(h.Sum(nil))
	}
	manifest, err := json.Marshal(files)
	if err != nil {
		return err
	}
	if digest(manifest) != expectedManifest {
		return errors.New("voice model file manifest mismatch")
	}
	return os.WriteFile(filepath.Join(dest, "manifest.json"), manifest, 0600)
}

// ValidateModel authenticates every installed asset against a manifest whose
// digest is pinned in source, rejecting symlinks as well as modified files.
func ValidateModel(ctx context.Context, base string) error {
	return validateModel(ctx, ModelDir(base), manifestSHA256)
}

func validateModel(ctx context.Context, root, manifestPin string) error {
	st, err := os.Lstat(root)
	if err != nil || !st.IsDir() {
		return errors.New("missing voice model; run a2a voice install")
	}
	b, err := readBounded(filepath.Join(root, "manifest.json"), 128<<10)
	if err != nil || digest(b) != manifestPin {
		return errors.New("voice model manifest is missing or changed")
	}
	var files map[string]string
	if json.Unmarshal(b, &files) != nil {
		return errors.New("invalid voice model manifest")
	}
	for name, want := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := filepath.Join(root, filepath.FromSlash(name))
		// No parent may redirect an authenticated file out of this model tree.
		current := root
		for _, part := range strings.Split(name, "/") {
			current = filepath.Join(current, part)
			s, e := os.Lstat(current)
			if e != nil || s.Mode()&os.ModeSymlink != 0 {
				return errors.New("voice model asset is missing or redirected")
			}
		}
		f, err := os.Open(p)
		if err != nil {
			return errors.New("voice model asset unavailable")
		}
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(f, maxExtractBytes+1))
		f.Close()
		if err != nil || n > maxExtractBytes || hex.EncodeToString(h.Sum(nil)) != want {
			return fmt.Errorf("voice model asset integrity check failed")
		}
	}
	return nil
}
