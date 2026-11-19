package config

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

// NativeQualificationPath selects a protected host native-qualification
// entry; no default entry or discovery. Bad/missing configuration selects
// nothing, which leaves native admission at its default verified versions.
func NativeQualificationPath(p Paths) string {
	base := p.Base
	if p.Root != "" {
		base = p.Root
	}
	path := filepath.Join(base, "native-qualification.json")
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 4096 {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(raw) > 4096 {
		return ""
	}
	var c struct {
		QualificationPath string `json:"qualification_path"`
	}
	if json.Unmarshal(raw, &c) != nil || !filepath.IsAbs(c.QualificationPath) || filepath.Clean(c.QualificationPath) != c.QualificationPath {
		return ""
	}
	return c.QualificationPath
}
