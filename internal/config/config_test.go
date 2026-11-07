package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testPaths(t *testing.T) Paths {
	t.Helper()
	return Paths{Base: t.TempDir()}
}

func envOf(m map[string]string) Getenv { return func(k string) string { return m[k] } }

// Proves that valid agent names pass and that names which could escape the
// agents directory or break Matrix localparts are rejected.
func TestValidateName(t *testing.T) {
	for _, ok := range []string{"local-models-e9", "it-1a2b", "a", "x.y_z=1"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "../etc", "Upper", "a/b", "-lead", "has space", strings.Repeat("a", 65)} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", bad)
		}
	}
}

// Proves that A2A_CONFIG_DIR overrides the base directory.
func TestResolvePathsOverride(t *testing.T) {
	p, err := ResolvePaths(envOf(map[string]string{EnvConfigDir: "/x/y"}))
	if err != nil || p.Base != "/x/y" {
		t.Fatalf("ResolvePaths = %v, %v", p, err)
	}
	if got := p.AgentFile("a"); got != "/x/y/agents/a.json" {
		t.Fatalf("AgentFile = %q", got)
	}
}

// Proves that matrix.env is parsed (comments, export, quotes) and that process
// environment variables take precedence over the file.
func TestLoadMatrix(t *testing.T) {
	p := testPaths(t)
	content := "# comment\nexport A2A_MATRIX_URL=\"http://h:8008/\"\nA2A_MATRIX_SERVER_NAME=a2a.local\nA2A_MATRIX_REGISTRATION_TOKEN='tok'\ngarbage\n"
	if err := os.WriteFile(p.EnvFile(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadMatrix(p, envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	if m.URL != "http://h:8008" || m.ServerName != "a2a.local" || m.RegistrationToken != "tok" {
		t.Fatalf("LoadMatrix = %+v", m)
	}
	m, _ = LoadMatrix(p, envOf(map[string]string{EnvServerName: "other"}))
	if m.ServerName != "other" {
		t.Fatalf("env override ignored: %+v", m)
	}
	// A missing file is fine; values then come only from the environment.
	m, err = LoadMatrix(testPaths(t), envOf(map[string]string{EnvMatrixURL: "http://e"}))
	if err != nil || m.URL != "http://e" {
		t.Fatalf("missing file: %+v %v", m, err)
	}
}

func writeCred(t *testing.T, p Paths, name string, mode os.FileMode, body string) {
	t.Helper()
	path := p.AgentFile(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	os.Remove(path)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// Proves that a 0600 credential file loads (positive control) and that group
// or world readable files, symlinks, malformed JSON, and incomplete files are
// refused without echoing the file content.
func TestLoadCredentials(t *testing.T) {
	p := testPaths(t)
	good := `{"user_id":"@a:s","access_token":"secret-token-123","device_id":"D","password":"pw-secret","homeserver":"http://h"}`
	writeCred(t, p, "a", 0o600, good)
	c, err := LoadCredentials(p, "a")
	if err != nil || c.UserID != "@a:s" || c.ServerName() != "s" {
		t.Fatalf("LoadCredentials = %+v, %v", c, err)
	}

	for _, mode := range []os.FileMode{0o644, 0o640, 0o400, 0o700} {
		writeCred(t, p, "b", mode, good)
		_, err := LoadCredentials(p, "b")
		if err == nil || !strings.Contains(err.Error(), "want 0600") {
			t.Errorf("mode %o: err = %v, want mode refusal", mode, err)
		}
	}

	writeCred(t, p, "c", 0o600, `{"user_id":"@c:s","access_token":"secret-token-123"`)
	if _, err := LoadCredentials(p, "c"); err == nil || strings.Contains(err.Error(), "secret-token-123") {
		t.Errorf("malformed: err = %v", err)
	}

	writeCred(t, p, "d", 0o600, `{"user_id":"@d:s"}`)
	if _, err := LoadCredentials(p, "d"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("incomplete: err = %v", err)
	}

	if err := os.Symlink(p.AgentFile("a"), p.AgentFile("e")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials(p, "e"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("symlink: err = %v", err)
	}

	if _, err := LoadCredentials(p, "missing"); err == nil || !strings.Contains(err.Error(), "a2a identity ensure --role <your-role>") {
		t.Errorf("missing: err = %v", err)
	}
	if _, err := LoadCredentials(p, "../x"); err == nil {
		t.Error("path traversal name accepted")
	}
}

// Proves that the credential file is created with mode 0600 and that a second
// create for the same name is refused with ErrExists and leaves the first intact.
func TestCreateCredentialFileRefusesOverwrite(t *testing.T) {
	p := testPaths(t)
	f, err := CreateCredentialFile(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCredentials(f, Credentials{UserID: "@a:s", AccessToken: "t0k3n-abc"}); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p.AgentFile("a"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", st.Mode().Perm())
	}
	_, err = CreateCredentialFile(p, "a")
	if !errors.Is(err, ErrExists) {
		t.Fatalf("second create err = %v, want ErrExists", err)
	}
	c, err := LoadCredentials(p, "a")
	if err != nil || c.AccessToken != "t0k3n-abc" {
		t.Fatalf("original damaged: %+v %v", c, err)
	}
}

func writeTestCreds(t *testing.T, p Paths, name string, c Credentials) {
	t.Helper()
	f, err := CreateCredentialFile(p, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCredentials(f, c); err != nil {
		t.Fatal(err)
	}
}

// Proves UpdateCredentials rewrites the file at the same path as a regular
// 0600 file, keeps the other fields, and leaves no temp file.
func TestUpdateCredentials(t *testing.T) {
	p := testPaths(t)
	writeTestCreds(t, p, "a", Credentials{UserID: "@a:s", AccessToken: "old-token-1", DeviceID: "D", Password: "pw-123456"})
	got, err := UpdateCredentials(p, "a", func(c *Credentials) error { c.AccessToken = "new-token-2"; return nil })
	if err != nil || got.AccessToken != "new-token-2" {
		t.Fatalf("UpdateCredentials = %+v, %v", got, err)
	}
	c, err := LoadCredentials(p, "a")
	if err != nil || c.AccessToken != "new-token-2" || c.DeviceID != "D" || c.Password != "" {
		t.Fatalf("on disk = %+v, %v", c, err)
	}
	if st, _ := os.Lstat(p.AgentFile("a")); st.Mode().Perm() != 0o600 || !st.Mode().IsRegular() {
		t.Fatalf("mode = %v", st.Mode())
	}
	entries, _ := os.ReadDir(filepath.Dir(p.AgentFile("a")))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
}

// Proves UpdateCredentials never writes when the callback fails or reports
// ErrUnchanged, and refuses a file whose mode is not 0600 without touching it.
func TestUpdateCredentialsNoWrite(t *testing.T) {
	p := testPaths(t)
	writeTestCreds(t, p, "a", Credentials{UserID: "@a:s", AccessToken: "old-token-1"})
	before, _ := os.ReadFile(p.AgentFile("a"))
	boom := errors.New("boom")
	if _, err := UpdateCredentials(p, "a", func(c *Credentials) error { c.AccessToken = "x-token-9"; return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if _, err := UpdateCredentials(p, "a", func(c *Credentials) error { c.AccessToken = "x-token-9"; return ErrUnchanged }); err != nil {
		t.Fatalf("ErrUnchanged: err = %v", err)
	}
	if after, _ := os.ReadFile(p.AgentFile("a")); string(after) != string(before) {
		t.Fatal("file written despite a failed or unchanged callback")
	}
	os.Chmod(p.AgentFile("a"), 0o644)
	called := false
	if _, err := UpdateCredentials(p, "a", func(*Credentials) error { called = true; return nil }); err == nil || !strings.Contains(err.Error(), "want 0600") || called {
		t.Fatalf("loose mode: err = %v called = %v", err, called)
	}
	if st, _ := os.Stat(p.AgentFile("a")); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode changed to %o", st.Mode().Perm())
	}
}

// Proves that state round-trips with mode 0600, that a missing state is empty,
// and that a state file with a loose mode is refused.
func TestState(t *testing.T) {
	p := testPaths(t)
	s, err := LoadState(p, "a")
	if err != nil || s.NextBatch != "" {
		t.Fatalf("empty state = %+v %v", s, err)
	}
	s.NextBatch = "s42"
	if !s.AddThread("$root") || s.AddThread("$root") || s.AddThread("") {
		t.Fatal("AddThread dedupe broken")
	}
	if err := SaveState(p, "a", s); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p.StateFile("a"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o", st.Mode().Perm())
	}
	got, err := LoadState(p, "a")
	if err != nil || got.NextBatch != "s42" || !got.HasThread("$root") {
		t.Fatalf("round trip = %+v %v", got, err)
	}
	os.Chmod(p.StateFile("a"), 0o644)
	if _, err := LoadState(p, "a"); err == nil {
		t.Fatal("loose state mode accepted")
	}
}

// Proves that the thread list is bounded so the state file cannot grow without limit.
func TestStateThreadBound(t *testing.T) {
	var s State
	for i := 0; i < 600; i++ {
		s.AddThread(strings.Repeat("x", i+1))
	}
	if len(s.Threads) != 500 || s.Threads[0] != strings.Repeat("x", 101) {
		t.Fatalf("len = %d", len(s.Threads))
	}
}

// Proves that concurrent UpdateState calls serialize: 20 writers each adding a
// distinct thread all survive (a lost update would drop some).
func TestUpdateStateConcurrent(t *testing.T) {
	p := testPaths(t)
	done := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func(i int) {
			_, err := UpdateState(p, "a", func(s *State) error {
				s.AddThread(strings.Repeat("t", i+1))
				return nil
			})
			done <- err
		}(i)
	}
	for i := 0; i < 20; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	s, err := LoadState(p, "a")
	if err != nil || len(s.Threads) != 20 {
		t.Fatalf("threads = %d, err = %v, want 20", len(s.Threads), err)
	}
}

// Proves that an error from the update function aborts without saving.
func TestUpdateStateAbort(t *testing.T) {
	p := testPaths(t)
	_, err := UpdateState(p, "a", func(s *State) error {
		s.NextBatch = "x"
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("want error")
	}
	if s, _ := LoadState(p, "a"); s.NextBatch != "" {
		t.Fatalf("aborted update was saved: %+v", s)
	}
}

// Proves a human's credentials live outside agents/: LoadCredentials refuses a
// name that exists only in humans/ with a human-identity error, while
// LoadHumanCredentials reads it; MoveToHumans moves an agents/ file there with
// dir 0700 / file 0600 and never overwrites an existing human file.
func TestHumanCredentials(t *testing.T) {
	p := testPaths(t)
	os.MkdirAll(filepath.Dir(p.AgentFile("x")), 0o700)
	write := func(path string) {
		os.WriteFile(path, []byte(`{"user_id":"@ivan:a2a.local","access_token":"t"}`), 0o600)
		os.Chmod(path, 0o600)
	}
	write(p.AgentFile("ivan"))
	if _, err := LoadCredentials(p, "ivan"); err != nil {
		t.Fatalf("control: agents/ivan.json loads before the move: %v", err)
	}
	dst, err := MoveToHumans(p, "ivan")
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Dir(dst)); st.Mode().Perm() != 0o700 {
		t.Fatalf("humans dir mode %o", st.Mode().Perm())
	}
	if _, err := LoadCredentials(p, "ivan"); err == nil || !strings.Contains(err.Error(), "human's identity") {
		t.Fatalf("agent load of a human: %v", err)
	}
	if c, err := LoadHumanCredentials(p, "ivan"); err != nil || c.UserID != "@ivan:a2a.local" {
		t.Fatalf("human load: %+v %v", c, err)
	}
	write(p.AgentFile("ivan"))
	if _, err := MoveToHumans(p, "ivan"); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("overwrite: %v", err)
	}
}

// Learned membership is room-scoped and bounded, cannot overwrite an existing
// observation or accept empty components, and grants no follow/unmute status.
// Valid same-ID observations in two rooms are the independent controls.
func TestThreadEventMembershipScopeAndRetention(t *testing.T) {
	var st State
	for _, e := range []ThreadEvent{{EventID: "$child", Root: "$root"}, {Room: "!room", Root: "$root"}, {Room: "!room", EventID: "$child"}} {
		st.RememberThreadEvent(e)
	}
	if len(st.ThreadEvents) != 0 {
		t.Fatal("invalid membership retained")
	}
	st.Mute("$root")
	st.RememberThreadEvent(ThreadEvent{Room: "!room", EventID: "$child", Root: "$root"})
	st.RememberThreadEvent(ThreadEvent{Room: "!room", EventID: "$child", Root: "$replacement"})
	st.RememberThreadEvent(ThreadEvent{Room: "!other", EventID: "$child", Root: "$other-root"})
	if len(st.ThreadEvents) != 2 || st.ThreadEventRoot("!room", "$child") != "$root" || st.ThreadEventRoot("!other", "$child") != "$other-root" || st.ThreadEventRoot("!unknown", "$child") != "" || st.HasThread("$root") || !st.IsMuted("$root") {
		t.Fatalf("membership altered scope or follow/mute policy: %+v", st)
	}
	for i := 0; i < 2000; i++ {
		st.RememberThreadEvent(ThreadEvent{Room: "!room", EventID: fmt.Sprintf("$event-%d", i), Root: "$root"})
	}
	if len(st.ThreadEvents) != 2000 || st.ThreadEventRoot("!room", "$child") != "" || st.ThreadEventRoot("!other", "$child") != "" || st.ThreadEventRoot("!room", "$event-0") != "$root" || st.ThreadEventRoot("!room", "$event-1999") != "$root" {
		t.Fatal("membership retention did not evict only the oldest entries")
	}
}
