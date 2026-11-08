//go:build linux || darwin

package courierenroll

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func socketFixture(t *testing.T, socket string) string {
	t.Helper()
	var fields map[string]string
	if json.Unmarshal([]byte(fixture("valid", 7)), &fields) != nil {
		t.Fatal("fixture setup")
	}
	fields["socket"] = socket
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal("fixture encoding")
	}
	return string(b) + "\n"
}

func TestSocketV1ValidNeighbors(t *testing.T) {
	for name, socket := range map[string]string{
		"root": "/", "ascii-103": "/" + strings.Repeat("x", 102),
		"utf8-103":   "/" + strings.Repeat("é", 51),
		"utf8-short": "/é/駅", "space": "/a b", "tab": "/a\tb",
		"dot-components": "/run/../socket", "repeated-slashes": "/run//socket",
	} {
		t.Run(name, func(t *testing.T) {
			dir := target(t)
			input := socketFixture(t, socket)
			if Install(dir, strings.NewReader(input), false) != nil {
				t.Fatal("valid pointer refused")
			}
			got, err := os.ReadFile(filepath.Join(dir, "courier.sock"))
			if err != nil || !bytes.Equal(got, []byte(socket+"\n")) {
				t.Fatal("pointer bytes or terminal LF differ")
			}
			if Install(dir, strings.NewReader(input), false) != errFiles {
				t.Fatal("replacement without explicit rotate accepted")
			}
			if Install(dir, strings.NewReader(fixture("next", 8)), true) != nil {
				t.Fatal("valid existing pointer refused on rotation")
			}
			checkGeneration(t, dir, "next", 8)
		})
	}
}

func TestSocketV1MalformedExistingGenerations(t *testing.T) {
	for name, data := range map[string][]byte{
		"ascii-104":  []byte("/" + strings.Repeat("x", 103) + "\n"),
		"utf8-105":   []byte("/" + strings.Repeat("é", 52) + "\n"),
		"missing-lf": []byte("/socket"), "additional-lf": []byte("/socket\n\n"),
		"cr": []byte("/socket\r\n"), "embedded-lf": []byte("/so\ncket\n"),
		"nul": []byte("/so\x00cket\n"), "bom-prefix": []byte("\uFEFF/socket\n"),
		"bom-in-path": []byte("/so\uFEFFcket\n"), "invalid-utf8": {'/', 0xff, '\n'},
		"empty": {}, "lf-only": {'\n'}, "relative": []byte("socket\n"),
	} {
		t.Run(name, func(t *testing.T) {
			dir := target(t)
			if Install(dir, strings.NewReader(fixture("before", 9)), false) != nil {
				t.Fatal("generation setup")
			}
			path := filepath.Join(dir, "courier.sock")
			if os.WriteFile(path, data, 0600) != nil {
				t.Fatal("mutation setup")
			}
			if Install(dir, strings.NewReader(fixture("after", 10)), true) != errFiles {
				t.Fatal("malformed existing pointer accepted")
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatal("refused rotation changed pointer")
			}
			entries, err := os.ReadDir(filepath.Dir(dir))
			if err != nil || len(entries) != 1 {
				t.Fatal("refused rotation changed parent namespace")
			}
		})
	}
}

func TestIdentityAndKeyExistingGenerationFormats(t *testing.T) {
	for name, tc := range map[string]struct {
		file string
		data []byte
	}{
		"identity-missing-lf": {"identity", []byte("valid")},
		"identity-extra-lf":   {"identity", []byte("valid\n\n")},
		"identity-crlf":       {"identity", []byte("valid\r\n")},
		"identity-upper":      {"identity", []byte("Valid\n")},
		"identity-65":         {"identity", []byte(strings.Repeat("x", 65) + "\n")},
		"key-short":           {"station.key", bytes.Repeat([]byte{7}, 31)},
		"key-appended-lf":     {"station.key", append(bytes.Repeat([]byte{7}, 32), '\n')},
	} {
		t.Run(name, func(t *testing.T) {
			dir := target(t)
			if Install(dir, strings.NewReader(fixture("before", 7)), false) != nil || os.WriteFile(filepath.Join(dir, tc.file), tc.data, 0600) != nil {
				t.Fatal("generation setup")
			}
			if Install(dir, strings.NewReader(fixture("after", 8)), true) != errFiles {
				t.Fatal("malformed generation accepted")
			}
		})
	}
	for _, identity := range []string{"x", strings.Repeat("x", 64), "a0._=-"} {
		dir := target(t)
		if Install(dir, strings.NewReader(fixture(identity, 7)), false) != nil || Install(dir, strings.NewReader(fixture("next", 8)), true) != nil {
			t.Fatal("valid identity neighbor refused")
		}
	}
}
