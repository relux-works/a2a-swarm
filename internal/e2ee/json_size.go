package e2ee

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"unicode/utf8"
)

// Size the canonical encoding of the one value without encoding or allocating a
// second snapshot. Field metadata is built once from the actual storage schema;
// this is not a second, hand-maintained list of persisted fields. The restricted
// schema has only integers, booleans, strings, byte slices/arrays and containers.
// RawMessage keeps its spelling but receives compact+HTML escaping, as Marshal.
type jsonSizeField struct {
	index, nameSize int
	omit            bool
}

var rawJSONType = reflect.TypeFor[json.RawMessage]()
var preparedType = reflect.TypeFor[Prepared]()
var snapshotFields = func() map[reflect.Type][]jsonSizeField {
	fields := map[reflect.Type][]jsonSizeField{}
	var visit func(reflect.Type)
	visit = func(t reflect.Type) {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			visit(t.Elem())
		case reflect.Struct:
			if _, ok := fields[t]; ok {
				return
			}
			fields[t] = nil
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
				fields[t] = append(fields[t], jsonSizeField{i, jsonStringSize(name), len(tag) > 1 && tag[1] == "omitempty"})
				visit(f.Type)
			}
		}
	}
	visit(reflect.TypeFor[Snapshot]())
	return fields
}()

type snapshotSizer struct {
	size, prepared int
	err            error
}

func (z *snapshotSizer) add(n int) {
	if n > MaxSnapshot-z.size {
		z.err = ErrCapacity
		return
	}
	z.size += n
}
func snapshotSizes(s Snapshot) (base, prepared, total int, err error) {
	z := snapshotSizer{}
	z.value(reflect.ValueOf(s))
	base = z.size
	if s.Prepared != nil {
		base -= len(`,"prepared":`) + z.prepared
	}
	return base, z.prepared, z.size, z.err
}
func (z *snapshotSizer) value(v reflect.Value) {
	if z.err != nil {
		return
	}
	start := z.size
	if v.Type() == rawJSONType {
		if v.IsNil() {
			z.add(4)
			return
		}
		if v.Len() > MaxSnapshot {
			z.err = ErrCapacity
			return
		}
		size, err := scanJSON(v.Bytes(), 2)
		if err != nil {
			z.err = err
			return
		}
		z.add(size)
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			z.add(4)
		} else {
			z.value(v.Elem())
		}
	case reflect.Struct:
		z.add(2)
		count := 0
		for _, field := range snapshotFields[v.Type()] {
			f := v.Field(field.index)
			if field.omit && f.IsZero() {
				continue
			}
			if count > 0 {
				z.add(1)
			}
			count++
			z.add(field.nameSize + 1)
			z.value(f)
		}
	case reflect.String:
		z.add(jsonStringSize(v.String()))
	case reflect.Bool:
		if v.Bool() {
			z.add(4)
		} else {
			z.add(5)
		}
	case reflect.Int, reflect.Int64:
		n := v.Int()
		if n < 0 {
			z.add(1)
			z.add(uintSize(uint64(-(n + 1)) + 1))
		} else {
			z.add(uintSize(uint64(n)))
		}
	case reflect.Uint8, reflect.Uint64:
		z.add(uintSize(v.Uint()))
	case reflect.Slice:
		if v.IsNil() {
			z.add(4)
			return
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			if v.Len() > MaxSnapshot {
				z.err = ErrCapacity
				return
			}
			z.add(2 + base64.StdEncoding.EncodedLen(v.Len()))
			return
		}
		fallthrough
	case reflect.Array:
		z.add(2)
		for i := 0; i < v.Len() && z.err == nil; i++ {
			if i > 0 {
				z.add(1)
			}
			z.value(v.Index(i))
		}
	case reflect.Map:
		if v.IsNil() {
			z.add(4)
			return
		}
		z.add(2)
		count := 0
		// Marshal replaces invalid UTF-8 in string keys. Preserve the previous
		// strict-parse refusal if two distinct keys would serialize identically.
		var normalized map[string]bool
		if v.Type().Key().Kind() == reflect.String {
			iter := v.MapRange()
			for iter.Next() {
				if !utf8.ValidString(iter.Key().String()) {
					normalized = map[string]bool{}
					break
				}
			}
		}
		iter := v.MapRange()
		for iter.Next() && z.err == nil {
			if count > 0 {
				z.add(1)
			}
			count++
			key := iter.Key()
			if key.Kind() == reflect.String {
				k := key.String()
				if normalized != nil {
					normalizedKey := string([]rune(k))
					if normalized[normalizedKey] {
						z.err = ErrCodec
						return
					}
					normalized[normalizedKey] = true
				}
				z.add(jsonStringSize(k))
			} else if key.Kind() == reflect.Uint64 {
				z.add(2 + uintSize(key.Uint()))
			} else {
				z.err = ErrCodec
				return
			}
			z.add(1)
			z.value(iter.Value())
		}
	default:
		z.err = ErrCodec
	}
	if v.Type() == preparedType {
		z.prepared = z.size - start
	}
}
func uintSize(n uint64) int {
	size := 1
	for n >= 10 {
		size++
		n /= 10
	}
	return size
}
func jsonStringSize(s string) int {
	size := 2
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"', '\\', '\b', '\f', '\n', '\r', '\t':
				size += 2
			case '<', '>', '&':
				size += 6
			default:
				if c < 0x20 {
					size += 6
				} else {
					size++
				}
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == '\u2028' || r == '\u2029' || (r == utf8.RuneError && n == 1) {
			size += 6
		} else {
			size += n
		}
		i += n
	}
	return size
}
