package e2ee

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

// caseFoldCheck refuses a member name that case-folds to a known field of the
// destination type but is not exactly that name, at every depth that the
// typed decoder would reach. encoding/json matches field names without case
// and lets the last member win, so without this check `"M.RELATES_TO"` would
// populate the `m.relates_to` field while a case-sensitive consumer of the
// same bytes sees a different member. It walks tokens and looks at member
// names only: values are never decoded, so it cannot change which values the
// surrounding decoder accepts (numbers stay json.Number, never float64).
// Opaque RawMessage members and subtrees of unknown type are consumed without
// checks. Grammar and depth were validated by the caller before this walk.
func caseFoldCheck(raw []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if !foldValue(d, reflect.TypeOf(dst)) {
		return ErrCodec
	}
	return nil
}

// foldValue consumes one JSON value from d, checking object member names
// against the fields of t. A nil or opaque t consumes without checking.
func foldValue(d *json.Decoder, t reflect.Type) bool {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == rawJSONType {
		t = nil
	}
	tok, err := d.Token()
	if err != nil {
		return false
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return true
	}
	switch delim {
	case '{':
		var fields map[string]reflect.Type
		var elem reflect.Type
		if t != nil {
			switch t.Kind() {
			case reflect.Struct:
				fields = foldFields(t)
			case reflect.Map:
				elem = t.Elem()
			}
		}
		for d.More() {
			k, err := d.Token()
			key, ok := k.(string)
			if err != nil || !ok {
				return false
			}
			child := elem
			if fields != nil {
				child = nil
				for name, ft := range fields {
					if name == key {
						child = ft
					} else if strings.EqualFold(name, key) {
						return false
					}
				}
			}
			if !foldValue(d, child) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim('}')
	case '[':
		var elem reflect.Type
		if t != nil && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
			elem = t.Elem()
		}
		for d.More() {
			if !foldValue(d, elem) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim(']')
	}
	return false
}

// foldFields lists the JSON member names a struct type decodes, as
// encoding/json derives them from tags and field names.
func foldFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")
		if f.PkgPath != "" || tag[0] == "-" {
			continue
		}
		name := tag[0]
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}
