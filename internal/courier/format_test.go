package courier

import (
	"strings"
	"testing"
)

func TestV1SharedFileFormats(t *testing.T) {
	for _, path := range []string{"/", "/a/../b ", "/" + strings.Repeat("a", 102), "/" + strings.Repeat("é", 51)} {
		data, err := PointerFile(path)
		if err != nil || string(data) != path+"\n" {
			t.Fatal("valid pointer encoding refused")
		}
		got, err := ParsePointerFile(data)
		if err != nil || got != path {
			t.Fatal("pointer did not round trip byte-exactly")
		}
	}
	for _, path := range []string{"", "relative", "/" + strings.Repeat("a", 103), "/" + strings.Repeat("é", 52), "/x\x00", "/x\r", "/x\n", "/x\uFEFF", "/\xff"} {
		if _, err := PointerFile(path); err == nil {
			t.Fatal("invalid pointer encoded")
		}
		if _, err := ParsePointerFile([]byte(path + "\n")); err == nil {
			t.Fatal("invalid pointer file accepted")
		}
	}
	for _, identity := range []string{"a", "local.name_=-", strings.Repeat("z", 64)} {
		data, err := IdentityFile(identity)
		if err != nil || string(data) != identity+"\n" {
			t.Fatal("valid identity encoding refused")
		}
		got, err := ParseIdentityFile(data)
		if err != nil || got != identity {
			t.Fatal("identity did not round trip")
		}
	}
	for _, identity := range []string{"", "A", "a b", "a\n", "a\r", "é", "a/", strings.Repeat("z", 65)} {
		if _, err := IdentityFile(identity); err == nil {
			t.Fatal("invalid identity encoded")
		}
		if _, err := ParseIdentityFile([]byte(identity + "\n")); err == nil {
			t.Fatal("invalid identity file accepted")
		}
	}
	for _, data := range []string{"", "/x", "/x\n\n", "/x\r\n"} {
		if _, err := ParsePointerFile([]byte(data)); err == nil {
			t.Fatal("noncanonical pointer accepted")
		}
	}
	for _, data := range []string{"", "name", "name\n\n", "name\r\n"} {
		if _, err := ParseIdentityFile([]byte(data)); err == nil {
			t.Fatal("noncanonical identity accepted")
		}
	}
}
