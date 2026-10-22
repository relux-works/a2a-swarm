package matrix

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// Independent reference: the Go standard library owns RFC8259 grammar,
// framing and depth. A separate raw-string lexeme walk rejects non-scalar
// escapes BEFORE Decoder.Token can repair them. Token traversal, not the
// production byte-recursive parser, constructs the reference tree. No
// production parser, key, framing or escape validator is called here.
func referenceJSON(raw []byte) (any, error) {
	bad := errors.New("reference rejection")
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return nil, bad
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '"' {
			continue
		}
		for i++; i < len(raw) && raw[i] != '"'; i++ {
			if raw[i] != '\\' {
				continue
			}
			i++
			if raw[i] != 'u' {
				continue
			}
			code, _ := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
			i += 4
			if code >= 0xdc00 && code <= 0xdfff {
				return nil, bad
			}
			if code < 0xd800 || code > 0xdbff {
				continue
			}
			if len(raw)-i < 7 || string(raw[i+1:i+3]) != `\u` {
				return nil, bad
			}
			low, e := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if e != nil || low < 0xdc00 || low > 0xdfff {
				return nil, bad
			}
			i += 6
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var read func() (any, error)
	read = func() (any, error) {
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{':
				m := map[string]any{}
				for d.More() {
					key, e := d.Token()
					if e != nil {
						return nil, e
					}
					k := key.(string)
					if _, ok := m[k]; ok {
						return nil, bad
					}
					v, e := read()
					if e != nil {
						return nil, e
					}
					m[k] = v
				}
				_, e := d.Token()
				return m, e
			case '[':
				a := []any{}
				for d.More() {
					v, e := read()
					if e != nil {
						return nil, e
					}
					a = append(a, v)
				}
				_, e := d.Token()
				return a, e
			default:
				return nil, bad
			}
		}
		return tok, nil
	}
	v, e := read()
	if e != nil {
		return nil, e
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, bad
	}
	return v, nil
}

type wireCase struct {
	name, raw string
	valid     bool
	value     any
}

var strictCorpus = []wireCase{
	{"object", `{}`, true, map[string]any{}}, {"array", `[]`, true, []any{}},
	{"null", "null", true, nil}, {"true", "true", true, true}, {"false", "false", true, false},
	{"string", `"text"`, true, "text"}, {"replacement", `"�"`, true, "\ufffd"},
	{"replacement-escape", `"\ufffd"`, true, "\ufffd"},
	{"paired", `"\ud83d\ude00"`, true, "😀"}, {"raw-nonbmp", `"😀"`, true, "😀"},
	{"escapes", `"\"\\\/\b\f\n\r\t\u0000"`, true, "\"\\/\b\f\n\r\t\x00"},
	{"del", "\"\x7f\"", true, "\x7f"},
	{"line-values", `"a\r\nb\u2028c"`, true, "a\r\nb\u2028c"},
	{"whitespace", " \t\r\n[null,true,false]\r \n", true, []any{nil, true, false}},
	{"zero", "0", true, json.Number("0")}, {"negative-zero", "-0", true, json.Number("-0")},
	{"number", "-1.234E+99", true, json.Number("-1.234E+99")},
	{"huge-number", "90071992547409931234567890123456789", true, json.Number("90071992547409931234567890123456789")},
	{"huge-exponent", "1e999999999", true, json.Number("1e999999999")},
	{"case-distinct", `{"x":1,"X":2}`, true, map[string]any{"x": json.Number("1"), "X": json.Number("2")}},
	{"unicode-distinct", `{"é":1,"e\u0301":2}`, true, map[string]any{"é": json.Number("1"), "e\u0301": json.Number("2")}},
	{"decoded-key", `{"\u0078":"value"}`, true, map[string]any{"x": "value"}},
	{"backslash-u", `"\\ud800"`, true, `\ud800`},
	{"nested", `{"x":[{},[],{"v":1.00}]}`, true, map[string]any{"x": []any{map[string]any{}, []any{}, map[string]any{"v": json.Number("1.00")}}}},
	{"bom", "\xef\xbb\xbf{}", false, nil}, {"utf16", "\xff\xfe{\x00}\x00", false, nil},
	{"invalid-utf8", "\"\xff\"", false, nil}, {"invalid-key", "{\"\xff\":1}", false, nil},
	{"empty", "", false, nil}, {"space-only", " \r\t\n", false, nil},
	{"unicode-space", "\u00a0{}", false, nil}, {"unicode-trailing", "{}\u2028", false, nil},
	{"nul", "{}\x00", false, nil}, {"control", "\"a\tb\"", false, nil},
	{"comment", "{} // x", false, nil}, {"block-comment", "/*x*/{}", false, nil},
	{"trailing", "{}{}", false, nil}, {"prefix", "{\"x\":1", false, nil}, {"array-comma", "[1,]", false, nil},
	{"object-comma", `{"x":1,}`, false, nil},
	{"duplicate", `{"x":1,"x":2}`, false, nil},
	{"escaped-duplicate", `{"x":1,"\u0078":2}`, false, nil},
	{"nested-duplicate", `[{"outer":{"x":1,"x":2}}]`, false, nil},
	{"high-surrogate", `"\ud800"`, false, nil}, {"low-surrogate", `"\udfff"`, false, nil},
	{"reverse-surrogates", `"\udc00\ud800"`, false, nil},
	{"mismatched-surrogates", `"\ud800\u0000"`, false, nil},
	{"surrogate-key", `{"\ud800":1}`, false, nil},
	{"escaped-low", `"\\\udc00"`, false, nil},
	{"invalid-escape", `"\v"`, false, nil}, {"bad-hex", `"\u+123"`, false, nil},
	{"short-escape", `"\u123"`, false, nil},
	{"unclosed-string", `"abc`, false, nil},
	{"nan", "NaN", false, nil}, {"infinity", "Infinity", false, nil}, {"leading-zero", "01", false, nil},
	{"hex", "0x12", false, nil}, {"plus", "+1", false, nil}, {"dot", ".1", false, nil},
	{"no-fraction", "1.", false, nil}, {"no-exponent", "1e+", false, nil}, {"double-sign", "--1", false, nil},
	{"literal-suffix", "truex", false, nil}, {"wrong-case", "NULL", false, nil},
}

func compareStrict(t testing.TB, raw []byte) {
	t.Helper()
	got, ge := strictJSON(raw)
	want, we := referenceJSON(raw)
	if (ge == nil) != (we == nil) {
		t.Fatalf("differential verdict raw=%q production=%v reference=%v", raw, ge, we)
	}
	if ge == nil && !reflect.DeepEqual(got, want) {
		t.Fatalf("differential value raw=%q production=%#v reference=%#v", raw, got, want)
	}
}
func TestStrictExpectedCorpus(t *testing.T) {
	for _, tc := range strictCorpus {
		t.Run(tc.name, func(t *testing.T) {
			for _, parser := range []struct {
				name  string
				parse func([]byte) (any, error)
			}{{"production", strictJSON}, {"reference", referenceJSON}} {
				v, e := parser.parse([]byte(tc.raw))
				if (e == nil) != tc.valid {
					t.Fatalf("%s expected verdict valid=%v value=%#v err=%v", parser.name, tc.valid, v, e)
				}
				if tc.valid && !reflect.DeepEqual(v, tc.value) {
					t.Fatalf("%s expected value got=%#v want=%#v", parser.name, v, tc.value)
				}
			}
			compareStrict(t, []byte(tc.raw))
		})
	}
}
func TestStrictResourceNeighbors(t *testing.T) {
	for _, depth := range []int{9999, 10000, 10001} {
		t.Run(strconv.Itoa(depth), func(t *testing.T) {
			b := []byte(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth))
			_, e := strictJSON(b)
			if (e == nil) != (depth <= 10000) {
				t.Fatalf("depth=%d err=%v", depth, e)
			}
			compareStrict(t, b)
		})
	}
	for _, size := range []int{4095, 4096, 4097, 65535, 65536, 65537, 131072} {
		t.Run("bytes-"+strconv.Itoa(size), func(t *testing.T) {
			b := []byte(`{"x":"` + strings.Repeat("a", size-8) + `"}`)
			compareStrict(t, b)
			if _, e := strictJSON(b); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func FuzzStrictJSON(f *testing.F) {
	for _, tc := range strictCorpus {
		f.Add([]byte(tc.raw))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 4096 {
			t.Skip("outside bounded fuzz domain")
		}
		compareStrict(t, raw)
	})
}
