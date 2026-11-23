package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// TestBindPartDiagnosisPrintsNoFixturePathOrHost proves the failure
// diagnostic does what it promises: for a valid marked fixture it reports no
// failing check, it names a planted defect, and its text carries neither the
// fixture's root directory nor the host label nor the host name.
func TestBindPartDiagnosisPrintsNoFixturePathOrHost(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	if _, err := config.EnsureBindPart(e.paths(), "alice", config.BindPartOpenOptions{Holder: e.state("alice").Lease.Holder, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	text := bindPartDiagnosis(e.paths(), "alice", e.dir)
	if !strings.Contains(text, "none of the document-level checks") || !strings.Contains(text, "sidecar.revision=") || !strings.Contains(text, `sidecar.lease.host="<host>"`) {
		t.Fatalf("diagnosis of a valid fixture:\n%s", text)
	}
	t.Logf("sample for a valid fixture:\n%s", text)
	host, _ := os.Hostname()
	for _, forbidden := range []string{e.dir, machineLabel(), host, os.Getenv("HOME")} {
		if forbidden != "" && strings.Contains(text, forbidden) {
			t.Fatalf("the diagnosis carries %q", forbidden)
		}
	}
	// A planted defect is named: a notice observed before it began.
	path := e.paths().BindPartFile("alice")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	doc["own_notice"] = map[string]any{"possibly_orphaned": true, "reason": orphanReason, "since": "2026-01-02T00:00:00Z", "observed_at": "2026-01-01T00:00:00Z"}
	planted, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, planted, 0o600); err != nil {
		t.Fatal(err)
	}
	if text := bindPartDiagnosis(e.paths(), "alice", e.dir); !strings.Contains(text, "document: own notice: observed_at is before since") {
		t.Fatalf("the planted defect is not named:\n%s", text)
	}
}
