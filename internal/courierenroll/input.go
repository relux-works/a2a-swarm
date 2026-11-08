// Package courierenroll installs a station user's local Courier enrollment.
// It neither registers a station nor opens a connection to Courier.
package courierenroll

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/relux-works/a2a-swarm/internal/courier"
)

const maxInput = 4096

var (
	errInput    = errors.New("courier enroll: invalid enrollment input")
	errFiles    = errors.New("courier enroll: unsafe or incomplete enrollment path")
	errWrite    = errors.New("courier enroll: enrollment not installed")
	errRecovery = errors.New("courier enroll: transaction outcome uncertain; inspect enrollment and sibling staging directories before retrying")
)

type enrollment struct {
	socket, identity, station string
	key                       []byte
}

// readInput accepts one LF-terminated closed JSON object and EOF. Errors never
// contain input bytes, including JSON decoder errors and field names.
func readInput(r io.Reader) (enrollment, error) {
	var e enrollment
	if r == nil {
		return e, errInput
	}
	b, err := io.ReadAll(io.LimitReader(r, maxInput+1))
	if err != nil || len(b) == 0 || len(b) > maxInput || b[len(b)-1] != '\n' || bytes.Count(b, []byte{'\n'}) != 1 || !utf8.Valid(b) {
		return e, errInput
	}
	d := json.NewDecoder(bytes.NewReader(b[:len(b)-1]))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return e, errInput
	}
	fields := make(map[string]string, 4)
	for d.More() {
		t, err = d.Token()
		if err != nil {
			return e, errInput
		}
		k, ok := t.(string)
		if !ok {
			return e, errInput
		}
		switch k {
		case "socket", "identity", "station", "secret_b64":
		default:
			return e, errInput
		}
		if _, exists := fields[k]; exists {
			return e, errInput
		}
		// Decode through Token: null cannot silently become an empty string.
		v, err := d.Token()
		if err != nil {
			return e, errInput
		}
		s, ok := v.(string)
		if !ok {
			return e, errInput
		}
		fields[k] = s
	}
	if t, err = d.Token(); err != nil || t != json.Delim('}') || len(fields) != 4 {
		return e, errInput
	}
	if _, err = d.Token(); err != io.EOF {
		return e, errInput
	}
	e.socket, e.identity, e.station = fields["socket"], fields["identity"], fields["station"]
	if courier.ValidateIdentity(e.identity) != nil || !validSocket(e.socket) || !validStation(e.station) {
		return enrollment{}, errInput
	}
	s := fields["secret_b64"]
	e.key, err = base64.StdEncoding.Strict().DecodeString(s)
	if err != nil || len(e.key) != courier.StationKeyLen || base64.StdEncoding.EncodeToString(e.key) != s {
		return enrollment{}, errInput
	}
	return e, nil
}

func validSocket(s string) bool {
	return courier.ValidateSocketPath(s) == nil
}

// Stationlink carries a station name as an opaque string, including reference
// names such as "id/swarma-station-1". It is not a local path or an identity.
// Bound it to 255 printable ASCII bytes; never substitute it for identity.
func validStation(s string) bool {
	if len(s) == 0 || len(s) > 255 || strings.TrimSpace(s) != s {
		return false
	}
	for _, c := range s {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}
