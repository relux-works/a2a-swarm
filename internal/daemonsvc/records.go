//go:build darwin || linux

package daemonsvc

import "bytes"

func decodeRecords(b []byte) ([]RescueRecord, error) {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return nil, ErrRefused
	}
	var out []RescueRecord
	for _, line := range bytes.Split(b[:len(b)-1], []byte{'\n'}) {
		var r RescueRecord
		if strictJSON(line, &r) != nil {
			return nil, ErrRefused
		}
		out = append(out, r)
	}
	return out, nil
}
