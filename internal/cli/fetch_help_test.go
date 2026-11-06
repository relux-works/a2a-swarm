package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func assertFetchHelp(t *testing.T, r result) {
	t.Helper()
	if r.code != ExitUsage || r.stdout != "" {
		t.Fatalf("help exit=%d stdout=%q, want usage exit and empty stdout", r.code, r.stdout)
	}
	for _, text := range []string{
		"usage: a2a fetch <room> <$event_id> [-o path]",
		"a2a fetch <mxc://...> --sha256 <hex> [-o path]",
		"-o string", "output path (never overwritten)",
		"-sha256 string", "expected sha256 (bare mxc only)",
		"a2a: fetch: flag: help requested\n",
	} {
		if !strings.Contains(r.stderr, text) {
			t.Errorf("help missing %q: %q", text, r.stderr)
		}
	}
}

// Run is the command entry path. Help needs neither an identity nor any
// configuration lookup, including when the caller context has been cancelled.
func TestFetchHelpWithoutSession(t *testing.T) {
	for _, args := range [][]string{
		{"fetch", "--help"}, {"fetch", "-h"},
		{"fetch", "proj", "--help", "$file"},
		{"fetch", "proj", "$file", "-h"},
		{"fetch", "--profile", "unconfigured", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, errOut bytes.Buffer
			a := NewApp()
			a.Stdout, a.Stderr = &out, &errOut
			a.Getenv = func(k string) string {
				t.Fatalf("help consulted environment %s", k)
				return ""
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			code := a.Run(ctx, args)
			assertFetchHelp(t, result{code, out.String(), errOut.String()})
		})
	}
}

// An unusable connection registry must not suppress help or be changed by it.
// A syntactically valid fetch still reaches and refuses that same registry.
func TestFetchHelpWithInvalidConfiguration(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("A2A_CONFIG_DIR", dir)
	path := filepath.Join(dir, "connections.json")
	original := []byte("invalid synthetic connection registry\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"fetch", "--help"}, {"fetch", "proj", "$file", "-h"}} {
		var out, errOut bytes.Buffer
		a := NewApp()
		a.Stdout, a.Stderr = &out, &errOut
		code := a.Run(context.Background(), args)
		assertFetchHelp(t, result{code, out.String(), errOut.String()})
		if strings.Contains(errOut.String(), dir) || strings.Contains(errOut.String(), string(original)) {
			t.Fatal("help exposed configuration state")
		}
	}
	var out, errOut bytes.Buffer
	a := NewApp()
	a.Stdout, a.Stderr = &out, &errOut
	if code := a.Run(context.Background(), []string{"fetch", "proj", "$file"}); code != ExitError || out.Len() != 0 {
		t.Fatalf("valid fetch bypassed invalid configuration: exit=%d stdout=%q", code, out.String())
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) || !reflect.DeepEqual(dirEntries(t, dir), []string{"connections.json"}) {
		t.Fatal("help or refused fetch changed configuration")
	}
}

// Fake HTTP downloads are the neighboring positive controls. Help and parse
// refusals send no request and leave no output file, even with -o specified.
func TestFetchHelpAndRefusalsPreserveDownload(t *testing.T) {
	e, id, data, _ := attachWorld(t)
	dir := t.TempDir()
	t.Chdir(dir)
	before := len(e.hs.urls())
	for _, args := range [][]string{
		{"fetch", "--help"}, {"fetch", "-h"},
		{"fetch", "proj", "--help", id, "-o", "unused"},
		{"fetch", "proj", id, "-h"},
	} {
		assertFetchHelp(t, e.as("bob", args...))
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"fetch", "--unknown", "--help"}, "flag provided but not defined"},
		{[]string{"fetch", "proj", id, "-o"}, "flag needs an argument"},
		{[]string{"fetch"}, "usage: a2a fetch"},
		{[]string{"fetch", "proj"}, "usage: a2a fetch"},
		{[]string{"fetch", "proj", id, "extra"}, "usage: a2a fetch"},
		{[]string{"fetch", "--", "proj", id, "--help"}, "usage: a2a fetch"},
		{[]string{"fetch", "mxc://test.local/media", "--sha256", "invalid"}, "invalid --sha256"},
	} {
		r := e.as("bob", tc.args...).failsWith(t, ExitUsage, tc.want)
		if r.stdout != "" {
			t.Fatal("refused fetch wrote stdout")
		}
	}
	if len(e.hs.urls()) != before || len(dirEntries(t, dir)) != 0 {
		t.Fatal("help or parse refusal sent HTTP or wrote an artifact")
	}
	// A help spelling consumed as an output value is a filename, not help.
	e.as("bob", "fetch", "proj", id, "-o", "--help").ok(t)
	got, err := os.ReadFile("--help")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("flag-value download did not preserve bytes")
	}
	e.as("bob", "fetch", "--", "proj", id).ok(t)
	got, err = os.ReadFile("notes.txt")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("positional download after -- did not preserve bytes")
	}
	ev, _ := e.hs.eventByID(id)
	h, _ := decodeHeader(ev.Message().A2A)
	e.as("bob", "fetch", h.Attachment.MXC, "--sha256", h.Attachment.SHA256, "-o", "bare").ok(t)
	got, err = os.ReadFile("bare")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("bare mxc download did not preserve bytes")
	}
}
