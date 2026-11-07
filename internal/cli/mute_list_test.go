package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Listing is offline and read-only, scoped to the selected identity. Empty and
// populated lists have deterministic text/JSON forms; invalid arguments and
// corrupt state refuse instead of reporting an empty list. No credentials exist.
func TestMuteListReadOnly(t *testing.T) {
	p := config.Paths{Base: t.TempDir()}
	var out, errs bytes.Buffer
	calls := 0
	run := func(name string, args ...string) int {
		t.Helper()
		out.Reset()
		errs.Reset()
		a := NewApp()
		a.Stdout, a.Stderr = &out, &errs
		a.HTTP = &http.Client{Transport: muteRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("mute list must not contact a server")
		})}
		a.Getenv = func(k string) string {
			if k == config.EnvConfigDir {
				return p.Base
			}
			return ""
		}
		return a.Run(context.Background(), append([]string{"--as", name, "mute", "list"}, args...))
	}
	if code := run("alice", "--json"); code != 0 || out.String() != "{\"identity\":\"alice\",\"muted\":[]}\n" {
		t.Fatalf("empty JSON: %d %q %q", code, out.String(), errs.String())
	}
	if _, err := os.Stat(p.StateFile("alice")); !os.IsNotExist(err) {
		t.Fatalf("listing created state: %v", err)
	}
	if code := run("alice"); code != 0 || out.String() != "no muted topics\n" {
		t.Fatalf("empty text: %d %q", code, out.String())
	}
	if _, err := config.UpdateState(p, "alice", func(st *config.State) error {
		st.NextBatch = "preserved-cursor"
		st.Mute("$z")
		st.Mute("$a")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p.StateFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p.StateFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if code := run("alice"); code != 0 || out.String() != "$a\n$z\n" {
		t.Fatalf("populated text: %d %q %q", code, out.String(), errs.String())
	}
	if code := run("alice", "--json"); code != 0 {
		t.Fatalf("populated JSON: %d %q", code, errs.String())
	}
	var got struct {
		Identity string   `json:"identity"`
		Muted    []string `json:"muted"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Identity != "alice" || !reflect.DeepEqual(got.Muted, []string{"$a", "$z"}) {
		t.Fatalf("JSON: %+v %v", got, err)
	}
	for _, args := range [][]string{{"unexpected"}, {"--topic", "$a"}, {"--json=invalid"}} {
		if code := run("alice", args...); code != ExitUsage || out.Len() != 0 {
			t.Fatalf("invalid args %v: %d %q %q", args, code, out.String(), errs.String())
		}
	}
	after, err := os.ReadFile(p.StateFile("alice"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("listing changed state: %v", err)
	}
	newInfo, err := os.Stat(p.StateFile("alice"))
	if err != nil || !os.SameFile(info, newInfo) || !info.ModTime().Equal(newInfo.ModTime()) {
		t.Fatalf("listing rewrote state: %v", err)
	}
	if _, err := os.Stat(p.MailboxFile("alice")); !os.IsNotExist(err) {
		t.Fatalf("listing created mailbox: %v", err)
	}
	if code := run("bob", "--json"); code != 0 || out.String() != "{\"identity\":\"bob\",\"muted\":[]}\n" {
		t.Fatalf("identity scope: %d %q", code, out.String())
	}
	if err := os.WriteFile(p.StateFile("alice"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := run("alice", "--json"); code != ExitError || out.Len() != 0 {
		t.Fatalf("corrupt state treated as empty: %d %q %q", code, out.String(), errs.String())
	}
	if calls != 0 {
		t.Fatalf("listing made %d network calls", calls)
	}
}

// Named-profile inspection needs no credentials/secret store and cannot leak
// another profile's mutes. Invalid identity and profile selection still refuse;
// the valid selected-profile reads are their neighboring controls.
func TestMuteListNamedNamespace(t *testing.T) {
	f := newNamedFixture(t, false)
	for _, profile := range []string{"alpha", "beta"} {
		p := f.paths(profile, false)
		if err := config.SaveState(p, "alice", config.State{Muted: []string{"$" + profile}}); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(p.StateFile("alice"))
		if err != nil {
			t.Fatal(err)
		}
		f.out.Reset()
		f.err.Reset()
		if code := f.app().Run(context.Background(), []string{"--profile", profile, "mute", "list", "--json"}); code != 0 || f.out.String() != "{\"identity\":\"alice\",\"muted\":[\"$"+profile+"\"]}\n" {
			t.Fatalf("profile=%s: %d %q %q", profile, code, f.out.String(), f.err.String())
		}
		after, err := os.ReadFile(p.StateFile("alice"))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("profile state changed: %v", err)
		}
	}
	for _, args := range [][]string{
		{"--profile", "missing", "mute", "list"},
		{"--as", "bob", "mute", "list"},
	} {
		f.out.Reset()
		if code := f.app().Run(context.Background(), args); code == 0 || f.out.Len() != 0 {
			t.Fatalf("invalid selection: %d %q", code, f.out.String())
		}
	}
	if f.effects() != 0 {
		t.Fatalf("local inspection produced %d HTTP/secret-store effects", f.effects())
	}
}
