package e2ee

import (
	"encoding/json"
	"reflect"
	"strings"
)

// caseFoldCheck refuses a member name that case-folds to a known field of the
// destination type but is not exactly that name, at every depth that the
// typed decoder would reach. encoding/json matches field names without case
// and lets the last member win, so without this check `"M.RELATES_TO"` would
// populate the `m.relates_to` field while a case-sensitive consumer of the
// same bytes sees a different member. Opaque RawMessage members are left to
// their own later decode; unknown members that fold to nothing are untouched.
func caseFoldCheck(raw []byte, dst any) error {
	var generic any
	if json.Unmarshal(raw, &generic) != nil {
		return ErrCodec
	}
	if !foldWalk(generic, reflect.TypeOf(dst)) {
		return ErrCodec
	}
	return nil
}

func foldWalk(v any, t reflect.Type) bool {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t == rawJSONType {
		return true
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := v.(map[string]any)
		if !ok {
			return true
		}
		fields := foldFields(t)
		for key, val := range obj {
			for name, ft := range fields {
				if name == key {
					if !foldWalk(val, ft) {
						return false
					}
				} else if strings.EqualFold(name, key) {
					return false
				}
			}
		}
	case reflect.Map:
		obj, ok := v.(map[string]any)
		if !ok {
			return true
		}
		for _, val := range obj {
			if !foldWalk(val, t.Elem()) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		arr, ok := v.([]any)
		if !ok {
			return true
		}
		for _, el := range arr {
			if !foldWalk(el, t.Elem()) {
				return false
			}
		}
	}
	return true
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
