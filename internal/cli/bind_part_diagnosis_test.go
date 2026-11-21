package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// bindPartDiagnosis describes a test fixture's own bind-state sidecar for a
// failure report: every field of the document, and the names of the
// document-level checks that do not hold. It is printed only when a hook of a
// fixture fails. It reads the one sidecar file of the fixture and nothing
// else: no environment and nothing outside the fixture's root. The root and
// the host label are replaced by placeholders before anything is printed.
func bindPartDiagnosis(p config.Paths, name, root string) string {
	clean := func(s string) string {
		for _, dir := range []string{root, "/private" + root} {
			if dir != "" && dir != "/private" {
				s = strings.ReplaceAll(s, dir, "<root>")
			}
		}
		if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != "" {
			s = strings.ReplaceAll(s, resolved, "<root>")
		}
		if host := machineLabel(); host != "" {
			s = strings.ReplaceAll(s, host, "<host>")
		}
		if host, err := os.Hostname(); err == nil && host != "" {
			s = strings.ReplaceAll(s, host, "<host>")
		}
		return s
	}
	data, err := os.ReadFile(p.BindPartFile(name))
	if err != nil {
		return "bind-state diagnosis: the sidecar document could not be read"
	}
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return fmt.Sprintf("bind-state diagnosis: %d bytes, not a JSON object", len(data))
	}
	var lines []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(prefix+"."+k, x[k])
			}
		case string:
			lines = append(lines, fmt.Sprintf("%s=%q", prefix, clean(x)))
		default:
			lines = append(lines, fmt.Sprintf("%s=%v", prefix, x))
		}
	}
	walk("sidecar", doc)

	str := func(v any) string { s, _ := v.(string); return s }
	obj := func(v any) map[string]any { m, _ := v.(map[string]any); return m }
	stamp := func(s string) (time.Time, bool) {
		t, err := time.Parse(time.RFC3339Nano, strings.ToUpper(s))
		return t, err == nil
	}
	publications := map[string]bool{"": true, "published": true, "unavailable: own notice changed": true,
		"unavailable: no liveness record (a2a status set)": true, "unavailable: invalid notice retry policy": true,
		"unavailable: own liveness publication retry pending": true, "unavailable: own liveness publication failed": true}
	var failing []string
	fail := func(where, what string) { failing = append(failing, where+": "+what) }
	projection := func(where string, m map[string]any) {
		if m == nil {
			return
		}
		if (str(m["generation"]) == "") != (str(m["session_id"]) == "") {
			fail(where, "generation and session are not both set or both empty")
		}
		if lease := obj(m["lease"]); lease != nil {
			if str(lease["holder"]) == "" {
				fail(where, "lease without a holder")
			}
			if expires, ok := stamp(str(lease["expires"])); !ok || expires.IsZero() {
				fail(where, "lease without an expiry")
			}
			if pid, _ := lease["pid"].(json.Number); strings.HasPrefix(pid.String(), "-") {
				fail(where, "lease with a negative pid")
			}
		}
		if !publications[str(m["notice_publication"])] {
			fail(where, "notice publication outside the allowed set")
		}
		notice := obj(m["own_notice"])
		observed, observedOK := stamp(str(notice["observed_at"]))
		if str(notice["observed_at"]) != "" && !observedOK {
			fail(where, "own notice: observed_at is not a timestamp")
		}
		suspected, _ := notice["possibly_orphaned"].(bool)
		if suspected || str(notice["reason"]) != "" || str(notice["since"]) != "" {
			since, sinceOK := stamp(str(notice["since"]))
			switch {
			case !sinceOK:
				fail(where, "own notice: suspicion without a valid since")
			case str(notice["reason"]) != orphanReason:
				fail(where, "own notice: suspicion with another reason")
			case !observedOK:
				fail(where, "own notice: suspicion without observed_at")
			case observed.Before(since):
				fail(where, "own notice: observed_at is before since")
			case notice["possibly_orphaned"] != nil && !suspected:
				fail(where, "own notice: since or reason on a cleared notice")
			}
		}
		if notice["possibly_orphaned"] != nil && !observedOK {
			fail(where, "own notice: a verdict without observed_at")
		}
	}
	if fmt.Sprint(doc["format"]) != "1" {
		fail("document", "format is not 1")
	}
	if str(doc["identity"]) != name {
		fail("document", "identity is not the fixture's")
	}
	if len(data) > 16<<10 {
		fail("document", "larger than 16 KiB")
	}
	projection("document", doc)
	projection("legacy_shadow", obj(doc["legacy_shadow"]))
	revision := fmt.Sprint(doc["revision"])
	if op := obj(doc["status_op"]); op != nil {
		failing = append(failing, "pending: a status operation")
		if fmt.Sprint(op["expected_revision"]) != revision {
			fail("status_op", "expected_revision differs from the document's revision")
		}
		a, _ := json.Marshal(op["notice"])
		b, _ := json.Marshal(doc["own_notice"])
		if !bytes.Equal(a, b) {
			fail("status_op", "its notice differs from the document's own notice")
		}
		if fmt.Sprint(op["pid"]) == "0" || fmt.Sprint(op["pid"]) == "1" || str(op["birth"]) == "" || str(op["holder"]) == "" {
			fail("status_op", "owner without holder, pid above 1 or start")
		}
	}
	if op := obj(doc["cursor_op"]); op != nil {
		failing = append(failing, "pending: a cursor operation of kind "+str(op["kind"]))
		if fmt.Sprint(op["pid"]) == "0" || fmt.Sprint(op["pid"]) == "1" || str(op["birth"]) == "" || str(op["holder"]) == "" {
			fail("cursor_op", "owner without holder, pid above 1 or start")
		}
	}
	if txn := obj(doc["control_txn"]); txn != nil {
		failing = append(failing, "pending: a control transaction of kind "+str(txn["kind"]))
		projection("control_txn", txn)
	}
	if len(failing) == 0 {
		failing = []string{"none of the document-level checks this helper knows fails: the refusal came from an operation's arguments or from a rule not mirrored here"}
	}
	return "bind-state diagnosis\n  failing or pending:\n    " + strings.Join(failing, "\n    ") + "\n  document:\n    " + strings.Join(lines, "\n    ")
}
