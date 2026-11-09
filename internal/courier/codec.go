// Package courier implements the local authenticated Courier transport.
package courier

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

const MaxFrameBytes = 65536

var errRefused = errors.New("courier: refused")

// object accepts exactly one object with unique, case-sensitive keys. Nested
// values remain raw and are checked by their schema's own bounded decoder.
func object(data []byte, allowed map[string]bool) (map[string]json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, errRefused
	}
	d := json.NewDecoder(bytes.NewReader(data))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, errRefused
	}
	out := make(map[string]json.RawMessage)
	for d.More() {
		t, err = d.Token()
		if err != nil {
			return nil, errRefused
		}
		k, ok := t.(string)
		if !ok || (allowed != nil && !allowed[k]) {
			return nil, errRefused
		}
		if _, exists := out[k]; exists {
			return nil, errRefused
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil {
			return nil, errRefused
		}
		out[k] = raw
	}
	if t, err = d.Token(); err != nil || t != json.Delim('}') {
		return nil, errRefused
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, errRefused
	}
	return out, nil
}

func readFrame(r io.Reader) ([]byte, error) {
	var header [4]byte
	if n, err := io.ReadFull(r, header[:]); err != nil {
		if n != 0 {
			return nil, errRefused
		}
		return nil, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > MaxFrameBytes {
		return nil, errRefused
	}
	b := make([]byte, int(n))
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, errRefused
	}
	return b, nil
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) != 0 {
		n, err := w.Write(b)
		if n < 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func writeFrame(w io.Writer, body []byte) error {
	if len(body) == 0 || len(body) > MaxFrameBytes {
		return errRefused
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	return writeAll(w, body)
}
