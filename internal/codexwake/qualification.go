package codexwake

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// MaxQualificationBytes bounds the protected host qualification entry.
const MaxQualificationBytes = 4096

const qualificationDocument = "a2a-native-qualification/1"

var errQualification = errors.New("native qualification unavailable")

// Qualification is the parsed content of a protected host entry naming exact
// current-protocol server versions an operator has put under qualification
// for native delivery. The zero value names none. Storage provenance (a
// root-owned, non-writable file behind an absolute path selector) is the
// caller's obligation; production loads it through the shared protected
// host-file reader and treats every failure as the zero value.
type Qualification struct {
	versions map[string]bool
}

// Admits reports whether the entry names this exact version. Comparison is
// by complete string: a prefix, a later patch or an unpinned version the
// wire schema does not cover is never admitted.
func (q Qualification) Admits(version string) bool {
	return q.versions[version] && (currentProtocol{version: version}).valid()
}

// ParseQualification strictly decodes one qualification document:
//
//	{"version":"a2a-native-qualification/1","qualified_versions":["0.167.0"]}
//
// Unknown, duplicate or missing members, duplicate or unpinned versions and
// trailing data are all unavailable; no malformed entry narrows to a subset.
func ParseQualification(raw []byte) (Qualification, error) {
	if len(raw) == 0 || len(raw) > MaxQualificationBytes {
		return Qualification{}, errQualification
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	first, err := dec.Token()
	if err != nil || first != json.Delim('{') {
		return Qualification{}, errQualification
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return Qualification{}, errQualification
		}
		name, ok := key.(string)
		if !ok || (name != "version" && name != "qualified_versions") {
			return Qualification{}, errQualification
		}
		if _, seen := fields[name]; seen {
			return Qualification{}, errQualification
		}
		var value json.RawMessage
		if dec.Decode(&value) != nil {
			return Qualification{}, errQualification
		}
		fields[name] = value
	}
	if _, err := dec.Token(); err != nil || len(fields) != 2 {
		return Qualification{}, errQualification
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return Qualification{}, errQualification
	}
	var document string
	if len(fields["version"]) == 0 || fields["version"][0] != '"' || json.Unmarshal(fields["version"], &document) != nil || document != qualificationDocument {
		return Qualification{}, errQualification
	}
	var versions []string
	if json.Unmarshal(fields["qualified_versions"], &versions) != nil || len(versions) == 0 || len(versions) > 8 {
		return Qualification{}, errQualification
	}
	q := Qualification{versions: map[string]bool{}}
	for _, version := range versions {
		if !(currentProtocol{version: version}).valid() || q.versions[version] {
			return Qualification{}, errQualification
		}
		q.versions[version] = true
	}
	return q, nil
}
