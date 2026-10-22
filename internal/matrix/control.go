package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
)

// ACKReceipt owns the exact complete response entity and its interpreted ID.
// Accessors cannot mutate the binding. It is transport evidence, not a proof
// that an honest-looking server actually applied a mutation. Never log Raw.
type ACKReceipt struct {
	eventID       string
	raw           []byte
	status        int
	contentLength int64
}

func (a ACKReceipt) EventID() string      { return a.eventID }
func (a ACKReceipt) Raw() []byte          { return append([]byte(nil), a.raw...) }
func (a ACKReceipt) Status() int          { return a.status }
func (a ACKReceipt) ContentLength() int64 { return a.contentLength }

type controlValue struct {
	raw      []byte
	value    map[string]any
	status   int
	length   int64
	complete bool
}

// readControl collects the whole entity before interpreting any field. A clean
// EOF and a successful Close are required even with a declared length. This
// function owns Close: its observable effects must precede the final context
// check and interpretation, never run in a caller's deferred cleanup afterward.
// net/http may hide bytes outside entity framing; this cannot attest them.
func readControl(ctx context.Context, resp *http.Response, limit int64) (*controlValue, error) {
	if resp.StatusCode < http.StatusOK {
		resp.Body.Close()
		return nil, errors.New("matrix: control response is not final")
	}
	if err := ctx.Err(); err != nil {
		resp.Body.Close()
		return nil, err
	}
	var reader io.Reader = resp.Body
	if limit > 0 {
		reader = io.LimitReader(reader, limit)
	}
	raw, err := io.ReadAll(reader)
	closeErr := resp.Body.Close()
	v := &controlValue{raw: raw, status: resp.StatusCode, length: resp.ContentLength}
	if err != nil {
		return v, errors.New("matrix: control response read failed")
	}
	if err := ctx.Err(); err != nil {
		return v, err
	}
	if closeErr != nil {
		return v, errors.New("matrix: control response close failed")
	}
	if limit > 0 && int64(len(raw)) >= limit {
		return v, errors.New("matrix: control response exceeds evidence limit")
	}
	if resp.ContentLength >= 0 && int64(len(raw)) != resp.ContentLength {
		return v, errors.New("matrix: incomplete control response")
	}
	v.complete = true
	tree, err := strictJSON(raw)
	if err != nil {
		return v, err
	}
	object, ok := tree.(map[string]any)
	if !ok {
		return v, errors.New("matrix: control response must be an object")
	}
	if err := ctx.Err(); err != nil {
		v.complete = false
		return v, err
	}
	v.value = object
	return v, nil
}

// mapControl maps the already interpreted tree, matching only exact JSON tags.
// Unknown fields remain syntactically validated and are ignored for forwards
// compatibility. Null never coerces a known typed field into a zero value.
func mapControl(value any, out any) error {
	if out == nil {
		return nil
	}
	dst := reflect.ValueOf(out)
	if dst.Kind() != reflect.Pointer || dst.IsNil() {
		return errors.New("matrix: invalid control destination")
	}
	next := reflect.New(dst.Elem().Type()).Elem()
	if err := assignControl(next, value); err != nil {
		return err
	}
	dst.Elem().Set(next)
	return nil
}
func assignControl(dst reflect.Value, value any) error {
	bad := errors.New("matrix: invalid control field type")
	switch dst.Kind() {
	case reflect.Struct:
		obj, ok := value.(map[string]any)
		if !ok {
			return bad
		}
		for i := 0; i < dst.NumField(); i++ {
			f := dst.Type().Field(i)
			if !dst.Field(i).CanSet() {
				continue
			}
			key := strings.Split(f.Tag.Get("json"), ",")[0]
			if key == "-" {
				continue
			}
			if key == "" {
				key = f.Name
			}
			if v, exists := obj[key]; exists {
				if err := assignControl(dst.Field(i), v); err != nil {
					return err
				}
			}
		}
	case reflect.String:
		s, ok := value.(string)
		if !ok {
			return bad
		}
		dst.SetString(s)
	case reflect.Bool:
		b, ok := value.(bool)
		if !ok {
			return bad
		}
		dst.SetBool(b)
	case reflect.Int, reflect.Int64:
		n, ok := value.(json.Number)
		if !ok {
			return bad
		}
		v, err := strconv.ParseInt(string(n), 10, dst.Type().Bits())
		if err != nil {
			return bad
		}
		dst.SetInt(v)
	case reflect.Slice:
		a, ok := value.([]any)
		if !ok {
			return bad
		}
		dst.Set(reflect.MakeSlice(dst.Type(), len(a), len(a)))
		for i, v := range a {
			if err := assignControl(dst.Index(i), v); err != nil {
				return err
			}
		}
	default:
		return bad
	}
	return nil
}

func (v *controlValue) consume(ctx context.Context, out any, required ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, key := range required {
		x, exists := v.value[key]
		if !exists {
			return errors.New("matrix: missing required control field")
		}
		if key == "joined_rooms" {
			if _, ok := x.([]any); !ok {
				return errors.New("matrix: invalid joined_rooms")
			}
			continue
		}
		s, ok := x.(string)
		if !ok || s == "" {
			return errors.New("matrix: invalid required control field")
		}
		// Matrix specifies event IDs as fully opaque, case-sensitive strings with
		// a '$' sigil. Do not require a server suffix or constrain opaque characters.
		// The public identifier contract bounds both event and room IDs to 255 bytes.
		if (key == "event_id" || key == "room_id") && len(s) > 255 {
			return errors.New("matrix: control identifier exceeds 255 bytes")
		}
		if key == "event_id" && (len(s) < 2 || s[0] != '$') {
			return errors.New("matrix: invalid event identifier")
		}
		if key == "room_id" && (len(s) < 2 || s[0] != '!') {
			return errors.New("matrix: invalid room identifier")
		}
		if key == "user_id" {
			// Preserve historical localparts and server spellings. This is the
			// pinned structural contract, not registration's creation policy.
			local, server, separated := strings.Cut(strings.TrimPrefix(s, "@"), ":")
			if s[0] != '@' || !separated || local == "" || server == "" {
				return errors.New("matrix: invalid user identifier")
			}
		}
	}
	if err := mapControl(v.value, out); err != nil {
		return err
	}
	return ctx.Err()
}

func (e *Error) uia(out *uiaResponse) bool {
	if e.control == nil {
		return false
	}
	// Required exact members must exist in the interpreted tree; mapping an
	// absent field to a Go zero value is not authentication-flow evidence.
	flows, ok := e.control.value["flows"].([]any)
	if !ok {
		return false
	}
	for _, flow := range flows {
		object, ok := flow.(map[string]any)
		if !ok {
			return false
		}
		if _, ok := object["stages"].([]any); !ok {
			return false
		}
	}
	return mapControl(e.control.value, out) == nil && out.Session != ""
}

// callBulk is deliberately selected by endpoint code, never by peer content.
func (c *Client) callBulk(ctx context.Context, method, path string, query url.Values, in, out any) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return c.doBulk(ctx, method, path, query, in, out)
}
