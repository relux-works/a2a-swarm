package darwinrelease

import (
	"reflect"
	"testing"
)

// Proves signArgs is exactly the codesign argv the release used before it was
// extracted: explicit identity and keychain, identifier, hardened runtime,
// secure timestamp and the full designated requirement, in that order. The
// native signing-keychain control relies on this being the release argv.
func TestSignArgsIsTheReleaseArgv(t *testing.T) {
	got := signArgs("SHA1", "/k/signing.keychain-db", "a2a", "anchor apple generic", "/w/a2a")
	want := []string{"--force", "--sign", "SHA1", "--keychain", "/k/signing.keychain-db", "--identifier", "works.relux.a2a",
		"--options", "runtime", "--timestamp", "--requirements", "=designated => anchor apple generic", "/w/a2a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("signArgs\n got %q\nwant %q", got, want)
	}
	// Negative: the companion gets its own identifier, never the CLI's.
	if c := signArgs("SHA1", "/k", "a2a-keychain-status", "x", "/w/c"); c[6] != "works.relux.a2a-keychain-status" {
		t.Fatalf("companion identifier = %q", c[6])
	}
}
