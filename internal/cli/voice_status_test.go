package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/voice"
)

type voiceStatusNoHTTP struct{}

func (voiceStatusNoHTTP) RoundTrip(*http.Request) (*http.Response, error) {
	panic("voice status attempted HTTP")
}

func voiceStatusSnapshot(t *testing.T, base string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		st, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		result[rel] = st.Mode().String() + st.ModTime().String()
		if st.Mode().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			result[rel] += string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func runVoiceStatus(t *testing.T, base, name string, args ...string) result {
	t.Helper()
	var out, errout bytes.Buffer
	a := &App{Stdout: &out, Stderr: &errout, HTTP: &http.Client{Transport: voiceStatusNoHTTP{}}, Getenv: func(k string) string {
		switch k {
		case config.EnvConfigDir:
			return base
		case config.EnvAgent:
			return name
		case config.EnvRegistrationToken:
			t.Fatal("status consulted registration secret")
		}
		return ""
	}}
	code := a.Run(context.Background(), args)
	return result{code: code, stdout: out.String(), stderr: errout.String()}
}

func TestVoiceStatusReadOnly(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(bin, "executed")
	if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	for _, tc := range []struct{ name, data, want string }{
		{"missing", "", "missing"},
		{"enabled", `{"enabled":true,"max_seconds":180}`, "enabled"},
		{"disabled", `{"enabled":false,"max_seconds":180}`, "invalid"},
		{"invalid", `{"SYNTHETIC_PRIVATE_VALUE":"never print this"}`, "invalid"},
		{"oversize", strings.Repeat("x", 4097), "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			// Invalid unrelated state must not be opened by local voice inspection.
			for _, path := range []string{"matrix.env", "agents/alice.json", "owner-dm/alice.json"} {
				path = filepath.Join(base, path)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("SYNTHETIC_PRIVATE_VALUE"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.data != "" {
				if err := os.MkdirAll(voice.Dir(base), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(voice.ConfigPath(base), []byte(tc.data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(voice.ModelDir(base), 0700); err != nil {
				t.Fatal(err)
			}
			before := voiceStatusSnapshot(t, base)
			for _, args := range [][]string{{"voice", "status"}, {"voice", "status", "--json"}} {
				r := runVoiceStatus(t, base, "alice", args...).ok(t)
				if r.stderr != "" || strings.Contains(r.stdout, "SYNTHETIC_PRIVATE_VALUE") || strings.Contains(r.stdout, base) {
					t.Fatalf("unsafe diagnostic: %+v", r)
				}
				if args[len(args)-1] == "--json" {
					var status voice.Status
					if err := json.Unmarshal([]byte(r.stdout), &status); err != nil {
						t.Fatal(err)
					}
					if status.Config != tc.want || status.Model != "present_unverified" || status.Encoder != "found_unverified" || status.Synthesis != "not_checked" || status.LastAttempt != "not_recorded" || status.MaxSeconds != 180 {
						t.Fatalf("status=%+v", status)
					}
				} else if !strings.Contains(r.stdout, "config="+tc.want+" ") || !strings.Contains(r.stdout, "synthesis=not_checked last_attempt=not_recorded") {
					t.Fatalf("status=%q", r.stdout)
				}
			}
			if !reflect.DeepEqual(before, voiceStatusSnapshot(t, base)) {
				t.Fatal("status changed local state")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("status executed ffmpeg")
			}
		})
	}
	base := filepath.Join(t.TempDir(), "absent")
	t.Setenv("PATH", t.TempDir())
	r := runVoiceStatus(t, base, "", "voice", "status", "--json").ok(t)
	var s voice.Status
	if err := json.Unmarshal([]byte(r.stdout), &s); err != nil {
		t.Fatal(err)
	}
	if s.Config != "missing" || s.Model != "missing" || s.Encoder != "unavailable" {
		t.Fatalf("status=%+v", s)
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatal("status created config root")
	}
}

func TestVoiceStatusNamedWithoutCredentials(t *testing.T) {
	f := newNamedFixture(t, false)
	p := f.paths("beta", false)
	if err := voice.Save(p.Base, voice.Config{Enabled: true, MaxSeconds: 180}); err != nil {
		t.Fatal(err)
	}
	before := voiceStatusSnapshot(t, f.root)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"voice", "status", "--json"}, "missing"},
		{[]string{"--profile", "beta", "voice", "status", "--json"}, "enabled"},
	} {
		r := runVoiceStatus(t, f.root, "alice", tc.args...).ok(t)
		var s voice.Status
		if err := json.Unmarshal([]byte(r.stdout), &s); err != nil {
			t.Fatal(err)
		}
		if s.Config != tc.want || r.stderr != "" {
			t.Fatalf("status=%+v stderr=%q", s, r.stderr)
		}
	}
	if !reflect.DeepEqual(before, voiceStatusSnapshot(t, f.root)) || f.httpCalls.Load() != 0 || len(f.store.reads) != 0 || len(f.store.creates) != 0 {
		t.Fatal("status had side effects")
	}
}

func TestVoiceStatusRefusals(t *testing.T) {
	base := t.TempDir()
	for _, args := range [][]string{{"voice", "status", "--enable"}, {"voice", "status", "extra"}, {"voice", "status", "--json=bad"}, {"--profile", "missing", "voice", "status"}} {
		runVoiceStatus(t, base, "", args...).failsWith(t, ExitUsage, "")
	}
	if err := os.MkdirAll(voice.Dir(base), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "synthetic-private")
	if err := os.WriteFile(target, []byte("SYNTHETIC_PRIVATE_VALUE"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, voice.ConfigPath(base)); err != nil {
		t.Fatal(err)
	}
	r := runVoiceStatus(t, base, "", "voice", "status").ok(t)
	if !strings.Contains(r.stdout, "config=invalid") || strings.Contains(r.stdout, "SYNTHETIC_PRIVATE_VALUE") {
		t.Fatalf("status=%+v", r)
	}
	if err := os.WriteFile(filepath.Join(base, "connections.json"), []byte("SYNTHETIC_PRIVATE_VALUE"), 0600); err != nil {
		t.Fatal(err)
	}
	r = runVoiceStatus(t, base, "", "voice", "status")
	if r.code != ExitError || strings.Contains(r.stderr, "SYNTHETIC_PRIVATE_VALUE") {
		t.Fatalf("status=%+v", r)
	}
}
