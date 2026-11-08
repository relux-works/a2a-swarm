//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/courier"
)

// Exercise the public command dispatcher with credential/environment traps and
// the registration payload as enroll's actual input, never a copied schema.
func TestCourierComposedDispatchAndFormats(t *testing.T) {
	home := courierHome(t)
	var out, diag bytes.Buffer
	app := localCourierApp(&out, &diag)
	run := func(want int, args ...string) {
		t.Helper()
		out.Reset()
		diag.Reset()
		if got := app.Run(context.Background(), append([]string{"courier"}, args...)); got != want {
			t.Fatalf("%s dispatch exit = %d, want %d", args[0], got, want)
		}
	}
	// A real serve refusal must reach the endpoint, not the unknown-command path.
	run(ExitError, "serve", "--home", home)
	if diag.String() != "a2a: courier: refused\n" {
		t.Fatal("serve did not reach the closed endpoint refusal")
	}
	run(ExitOK, "register-station", "--home", home, "--station", "station", "--identity", "local.name_=-", "--uid", strconv.Itoa(os.Geteuid()), "--emit", "-")
	payload := append([]byte(nil), out.Bytes()...)
	defer clear(payload)
	fields := map[string]string{}
	if json.Unmarshal(payload, &fields) != nil || len(fields) != 4 || fields["socket"] != courier.DefaultSocket() {
		t.Fatal("registration contract differs")
	}
	key, err := base64.StdEncoding.Strict().DecodeString(fields["secret_b64"])
	if err != nil || len(key) != courier.StationKeyLen || courier.ProtocolVersion != 1 {
		t.Fatal("v1 key/version differs")
	}
	defer clear(key)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, ".a2a")
	app.Stdin = bytes.NewReader(payload)
	run(ExitOK, "enroll", "--dir", dir, "--from", "-")
	if out.String() != "Enrollment installed.\n" || diag.Len() != 0 {
		t.Fatal("enroll output differs")
	}
	pointer, err := os.ReadFile(filepath.Join(dir, "courier.sock"))
	if err != nil {
		t.Fatal(err)
	}
	socket, err := courier.ParsePointerFile(pointer)
	if err != nil || socket != fields["socket"] {
		t.Fatal("enrolled pointer differs from registration")
	}
	identityBytes, err := os.ReadFile(filepath.Join(dir, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := courier.ParseIdentityFile(identityBytes)
	if err != nil || identity != fields["identity"] {
		t.Fatal("enrolled identity differs from registration")
	}
	installed, err := os.ReadFile(filepath.Join(dir, "station.key"))
	defer clear(installed)
	if err != nil || !bytes.Equal(installed, key) {
		t.Fatal("enrollment changed the raw station key")
	}
	// Composition must retain the explicit-rotation contract.
	app.Stdin = bytes.NewReader(payload)
	run(ExitError, "enroll", "--dir", dir, "--from", "-")
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	app.Stdin = bytes.NewReader(payload)
	run(ExitError, "enroll", "--dir", filepath.Join(alias, ".a2a"), "--from", "-", "--rotate")
	app.Stdin = bytes.NewReader(payload)
	run(ExitOK, "enroll", "--dir", dir, "--from", "-", "--rotate")
	run(ExitOK, "unregister-station", "--home", home, "--station", "station")
	run(ExitError, "unregister-station", "--home", home, "--station", "station")
}
