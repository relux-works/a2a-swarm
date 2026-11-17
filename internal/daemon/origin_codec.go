package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"unicode/utf8"

	"github.com/relux-works/a2a-swarm/runorigin"
)

const originPrepare = "source.origin.prepare/1"
const originValidate = "source.origin.validate/1"

type originRequest struct {
	op, id, nonce, prepared, incarnation string
	event                                runorigin.EventRef
	input                                runorigin.Principal
}

// originObject is separate from the station parser: exact mandatory members,
// case-sensitive names, no duplicates (including escaped aliases), no trailers.
func originObject(b []byte, names ...string) (map[string]json.RawMessage, error) {
	if !utf8.Valid(b) {
		return nil, runorigin.ErrRefused
	}
	d := json.NewDecoder(bytes.NewReader(b))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, runorigin.ErrRefused
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		tok, err = d.Token()
		if err != nil {
			return nil, runorigin.ErrRefused
		}
		name, ok := tok.(string)
		allowed := false
		for _, n := range names {
			if name == n {
				allowed = true
			}
		}
		if !ok || !allowed || fields[name] != nil {
			return nil, runorigin.ErrRefused
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil {
			return nil, runorigin.ErrRefused
		}
		fields[name] = raw
	}
	if tok, err = d.Token(); err != nil || tok != json.Delim('}') || len(fields) != len(names) {
		return nil, runorigin.ErrRefused
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, runorigin.ErrRefused
	}
	return fields, nil
}

func originString(b []byte) (string, bool) {
	var s string
	err := json.Unmarshal(b, &s)
	return s, err == nil && len(s) > 0 && len(s) <= 4096 && !bytes.ContainsAny([]byte(s), "\x00\r\n")
}

func decodeOriginRequest(b []byte) (originRequest, error) {
	var r originRequest
	env, err := originObject(b, "op", "params")
	if err != nil {
		return r, err
	}
	r.op, _ = originString(env["op"])
	var f map[string]json.RawMessage
	switch r.op {
	case originPrepare:
		f, err = originObject(env["params"], "request_id", "launch_nonce", "event_ref", "input_principal")
	case originValidate:
		f, err = originObject(env["params"], "request_id", "launch_nonce", "prepared_id", "source_incarnation")
	default:
		return r, runorigin.ErrRefused
	}
	if err != nil {
		return r, err
	}
	var ok bool
	if r.id, ok = originString(f["request_id"]); !ok || !runorigin.LowerHex(r.id, 32) {
		return r, runorigin.ErrRefused
	}
	if r.nonce, ok = originString(f["launch_nonce"]); !ok || !runorigin.LowerHex(r.nonce, 32) {
		return r, runorigin.ErrRefused
	}
	if r.op == originValidate {
		r.prepared, _ = originString(f["prepared_id"])
		r.incarnation, _ = originString(f["source_incarnation"])
		if !runorigin.LowerHex(r.prepared, 32) || !runorigin.LowerHex(r.incarnation, 32) {
			return r, runorigin.ErrRefused
		}
		return r, nil
	}
	e, err := originObject(f["event_ref"], "room_id", "event_id")
	if err != nil {
		return r, err
	}
	if r.event.RoomID, ok = originString(e["room_id"]); !ok {
		return r, runorigin.ErrRefused
	}
	if r.event.EventID, ok = originString(e["event_id"]); !ok {
		return r, runorigin.ErrRefused
	}
	p, err := originObject(f["input_principal"], "uid", "generation")
	if err != nil {
		return r, err
	}
	// UID remains an integer; generation accepts only the journal's string spelling.
	// encoding/json accepts null for scalar destinations, so reject it explicitly
	// for UID and require a valid nonempty generation string.
	if bytes.Equal(bytes.TrimSpace(p["uid"]), []byte("null")) ||
		json.Unmarshal(p["uid"], &r.input.UID) != nil || r.input.UID == 0 {
		return r, runorigin.ErrRefused
	}
	if r.input.Generation, ok = originString(p["generation"]); !ok || !runorigin.ValidGeneration(r.input.Generation) {
		return r, runorigin.ErrRefused
	}
	return r, nil
}
