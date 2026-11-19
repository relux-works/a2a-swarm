package codexwake

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func qualificationEntry(t *testing.T, versions ...string) Qualification {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"version": qualificationDocument, "qualified_versions": versions})
	if err != nil {
		t.Fatal(err)
	}
	q, err := ParseQualification(raw)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// Proves the protected entry grammar names only complete pinned
// current-protocol versions: the valid entry admits exactly the versions it
// lists, beside malformed, duplicate, unpinned, baseline and prefix cases
// that parse to no admission.
func TestQualificationParseExactVersions(t *testing.T) {
	all := qualificationEntry(t, "0.163.0", "0.165.0", "0.167.0")
	for _, version := range []string{"0.163.0", "0.165.0", "0.167.0"} {
		if !all.Admits(version) {
			t.Fatalf("valid entry refused its named version %s", version)
		}
	}
	one := qualificationEntry(t, "0.167.0")
	if !one.Admits("0.167.0") || one.Admits("0.165.0") {
		t.Fatal("single-version entry admitted an unnamed version or refused its own")
	}
	for _, version := range []string{"0.167", "0.167.0 ", " 0.167.0", "0.167.01", "0.1670", ""} {
		if one.Admits(version) {
			t.Fatalf("prefix or padded version %q admitted", version)
		}
	}
	if (Qualification{}).Admits("0.167.0") {
		t.Fatal("absent entry admitted a current version")
	}
	for _, raw := range []string{
		``,
		`{`,
		`[]`,
		`null`,
		`{"version":"a2a-native-qualification/1"}`,
		`{"qualified_versions":["0.167.0"]}`,
		`{"version":"a2a-native-qualification/2","qualified_versions":["0.167.0"]}`,
		`{"version":1,"qualified_versions":["0.167.0"]}`,
		`{"version":"a2a-native-qualification/1","qualified_versions":[]}`,
		`{"version":"a2a-native-qualification/1","qualified_versions":null}`,
		`{"version":"a2a-native-qualification/1","qualified_versions":"0.167.0"}`,
		`{"version":"a2a-native-qualification/1","qualified_versions":[0.167]}`,
		`{"version":"a2a-native-qualification/1","qualified_versions":[null]}`,
		`{"version":"a2a-native-qualification/1","qualified_versions":["0.167.0","0.167.0"]}`,
		`{"version":"a2a-native-qualification/1","qualified_versions":["0.999.0"]}`,
		`{"version":"a2a-native-qualification/1","qualified_versions":["0.167"]}`,
		`{"version":"a2a-native-qualification/1","qualified_versions":["0.161.0"]}`,
		`{"version":"a2a-native-qualification/1","qualified_versions":["0.167.0"],"extra":true}`,
		`{"version":"a2a-native-qualification/1","version":"a2a-native-qualification/1","qualified_versions":["0.167.0"]}`,
		`{"qualified_versions":["0.165.0"],"qualified_versions":["0.167.0"],"version":"a2a-native-qualification/1"}`,
		`{"version":"a2a-native-qualification/1","qualified_versions":["0.167.0"]}{}`,
		strings.Repeat(" ", MaxQualificationBytes+1),
	} {
		if q, err := ParseQualification([]byte(raw)); err == nil || q.Admits("0.167.0") || q.Admits("0.165.0") {
			t.Fatalf("malformed entry admitted: %.80q", raw)
		}
	}
	// The valid control still parses after the malformed cases.
	if !qualificationEntry(t, "0.167.0").Admits("0.167.0") {
		t.Fatal("valid entry lost after malformed cases")
	}
}

// Proves a stored current-protocol descriptor for another release does not
// satisfy the fingerprint check of the version it is presented for, beside
// the matching 0.167.0 fixtures.
func TestQualificationSwappedSchemaFixtureRefused(t *testing.T) {
	fixtureProtocol(t, "0.167.0")
	turn167 := currentFixture(t, "0.167.0", "TurnStartParams")
	history167 := currentFixture(t, "0.167.0", "ThreadItemsListResponse")
	if _, err := inspectCurrentProtocol("0.167.0", currentFixture(t, "0.165.0", "TurnStartParams"), history167); err == nil {
		t.Fatal("previous release turn schema admitted for 0.167.0")
	}
	if _, err := inspectCurrentProtocol("0.167.0", turn167, currentFixture(t, "0.165.0", "ThreadItemsListResponse")); err == nil {
		t.Fatal("previous release history schema admitted for 0.167.0")
	}
	if _, err := inspectCurrentProtocol("0.165.0", turn167, currentFixture(t, "0.165.0", "ThreadItemsListResponse")); err == nil {
		t.Fatal("0.167.0 turn schema admitted for 0.165.0")
	}
}

// Proves DialQualified admits a current-protocol server only when the entry
// names its exact version, records that the connection runs under
// qualification, and still refuses with the default message beside an
// admitted control. Default versions keep working with no entry.
func TestQualifiedDialAdmission(t *testing.T) {
	const refusal = "runtime native toolOutput capability unverified for this server version (supported: 0.160.0, 0.160.1, 0.161.0)"
	entry167 := qualificationEntry(t, "0.167.0")
	entry165 := qualificationEntry(t, "0.165.0")
	entryAll := qualificationEntry(t, "0.163.0", "0.165.0", "0.167.0")
	for _, tc := range []struct {
		name      string
		version   string
		entry     Qualification
		useDial   bool
		admitted  bool
		qualified bool
	}{
		{"0.167.0 named by entry", "0.167.0", entry167, false, true, true},
		{"0.167.0 without entry via qualified dial", "0.167.0", Qualification{}, false, false, false},
		{"0.167.0 without entry via default dial", "0.167.0", Qualification{}, true, false, false},
		{"0.167.0 with entry naming only 0.165.0", "0.167.0", entry165, false, false, false},
		{"0.165.0 named by entry", "0.165.0", entry165, false, true, true},
		{"0.163.0 named by shared entry", "0.163.0", entryAll, false, true, true},
		{"unknown version with full entry", "0.999.0", entryAll, false, false, false},
		{"prefix lookalike with entry", "0.167.01", entry167, false, false, false},
		{"0.161.0 with no entry", "0.161.0", Qualification{}, false, true, false},
		{"0.160.0 with no entry via default dial", "0.160.0", Qualification{}, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			endpoint := testServer(t, func(method string, params json.RawMessage) any {
				calls.Add(1)
				switch method {
				case "initialize":
					return map[string]string{"userAgent": "fixture/" + tc.version}
				case "turn/start":
					var p map[string]json.RawMessage
					if json.Unmarshal(params, &p) != nil || string(p["input"]) != "[]" {
						t.Error("qualified wake sent user input")
					}
					var tool map[string]any
					if json.Unmarshal(p["toolOutput"], &tool) != nil || tool["retain"] != true {
						t.Error("qualified wake omitted retained tool output")
					}
					return map[string]any{"turn": map[string]string{"id": "qualified-turn"}}
				default:
					t.Errorf("unexpected method %s", method)
					return nil
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var client *Client
			var err error
			if tc.useDial {
				client, err = Dial(ctx, endpoint)
			} else {
				client, err = DialQualified(ctx, endpoint, tc.entry)
			}
			if !tc.admitted {
				if err == nil {
					client.Close()
					t.Fatal("unnamed current version admitted")
				}
				if err.Error() != refusal {
					t.Fatalf("refusal message changed: %v", err)
				}
				if calls.Load() != 1 {
					t.Fatal("refusal reached a thread/turn call")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			if client.Qualified() != tc.qualified {
				t.Fatalf("qualified=%v want %v", client.Qualified(), tc.qualified)
			}
			if tc.qualified {
				turn, err := client.Wake(ctx, "thread", currentNotice)
				if err != nil || turn != "qualified-turn" {
					t.Fatalf("qualified wake refused: %s %v", turn, err)
				}
			}
		})
	}
}
