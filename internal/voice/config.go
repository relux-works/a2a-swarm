// Package voice implements optional, local-only spoken copies of text posts.
package voice

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const MaxSeconds = 180
const TruncationNotice = "Аудио сокращено. Полный текст сохранён в сообщении."

type Config struct {
	Enabled        bool   `json:"enabled"`
	MaxSeconds     int    `json:"max_seconds"`
	Worker         string `json:"worker,omitempty"`
	WorkerSHA256   string `json:"worker_sha256,omitempty"`
	ManifestSHA256 string `json:"manifest_sha256,omitempty"`
}

func (c Config) Validate() error {
	if !c.Enabled {
		return errors.New("voice config requires explicit --enable")
	}
	if c.MaxSeconds != MaxSeconds {
		return fmt.Errorf("voice max_seconds must be %d", MaxSeconds)
	}
	// Old explicit enablement remains valid, but cannot render until pinned.
	if c.Worker != "" || c.WorkerSHA256 != "" || c.ManifestSHA256 != "" {
		if !filepath.IsAbs(c.Worker) || filepath.Clean(c.Worker) != c.Worker || filepath.Base(c.Worker) != WorkerName || !validSHA(c.WorkerSHA256) || !validSHA(c.ManifestSHA256) {
			return errors.New("voice helper requires an absolute a2a-voice-worker path and lowercase SHA256 pins for worker and manifest")
		}
	}
	return nil
}

func Dir(base string) string        { return filepath.Join(base, "voice") }
func ConfigPath(base string) string { return filepath.Join(Dir(base), "config.json") }
func Load(base string) (Config, error) {
	var c Config
	b, err := readBounded(ConfigPath(base), 4096)
	if err != nil {
		return c, errors.New("missing voice config; run a2a voice config --enable")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, errors.New("invalid voice config")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return c, errors.New("invalid voice config")
	}
	return c, c.Validate()
}

func Save(base string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(Dir(base), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(Dir(base), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), ConfigPath(base))
}
