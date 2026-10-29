package codexwake

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Retained R1 reviewer probe and original test names. The following historical
// comment describes its original overlay execution, preserved under task evidence.
// ISSUE-122 review-r1 probe (reviewer-owned overlay; candidate source unchanged).
// Claim: the PRODUCTION history matcher (matchesOutput, used by Accepted and
// Client.Reconcile) must refuse duplicate-key current content exactly as the
// strict currentTextPart decoder does. Positive controls: the strict valid
// input_text array and the unchanged legacy inputText array are accepted.
// Limit: offline fixture server; exact-text items only, no native server.
func reviewR1Items(notice string) (valid, legacy, dupStrict, dupFallthrough, dupMixedFallthrough string) {
	text, _ := json.Marshal(notice)
	t := string(text)
	valid = `[{"type":"input_text","text":` + t + `}]`
	legacy = `[{"type":"inputText","text":` + t + `}]`
	// Refused by both decoders: last key stays input_text.
	dupStrict = `[{"type":"input_text","text":"peer","text":` + t + `}]`
	// Same ambiguity, last-key "type" switches to the legacy tag.
	dupFallthrough = `[{"type":"input_text","text":"peer","text":` + t + `,"type":"inputText"}]`
	// Mixed media object whose last duplicate tag is the legacy text tag.
	dupMixedFallthrough = `[{"type":"input_image","image_url":"peer","type":"inputText","text":` + t + `}]`
	return
}

func TestReviewR1ProductionMatcherDuplicateFallthrough(t *testing.T) {
	valid, legacy, dupStrict, dupFall, dupMixed := reviewR1Items(currentNotice)
	item := Item{Type: "functionCallOutput", Name: ToolName, Namespace: ToolNamespace}
	p := fixtureProtocol(t, "0.165.0")
	for _, ok := range []string{valid, legacy} {
		item.Output = json.RawMessage(ok)
		if !matchesOutput(item, currentNotice) {
			t.Fatalf("positive control refused: %s", ok)
		}
	}
	item.Output = json.RawMessage(dupStrict)
	if matchesOutput(item, currentNotice) {
		t.Fatal("neighbor duplicate control unexpectedly accepted")
	}
	for name, raw := range map[string]string{"dup-text-then-legacy-type": dupFall, "mixed-image-then-legacy-type": dupMixed} {
		item.Output = json.RawMessage(raw)
		helper := p.matchesOutput(item, currentNotice)
		production := matchesOutput(item, currentNotice)
		_, accepted := Accepted(Snapshot{Turns: []Turn{{ID: "t", Items: []Item{item}}}}, currentNotice)
		t.Logf("%s: strict-helper=%v production-matchesOutput=%v Accepted=%v", name, helper, production, accepted)
		if helper {
			t.Errorf("%s: strict helper accepted duplicate content", name)
		}
		if production || accepted {
			t.Errorf("%s: production matcher accepted duplicate-key content the strict decoder refuses", name)
		}
	}
}

// Same mechanism through the actual paged Client.Reconcile after a lost reply.
// Claim: Reconcile must not report applied acceptance from ambiguous content.
func TestReviewR1ReconcileDuplicateFallthrough(t *testing.T) {
	for _, version := range []string{"0.163.0", "0.165.0"} {
		for _, mode := range []string{"valid", "dup-strict", "dup-fallthrough"} {
			t.Run(version+"/"+mode, func(t *testing.T) {
				b, _, attempt := currentAttempt(t, version)
				valid, _, dupStrict, dupFall, _ := reviewR1Items(attempt.Output)
				body := map[string]string{"valid": valid, "dup-strict": dupStrict, "dup-fallthrough": dupFall}[mode]
				endpoint := testServer(t, func(method string, raw json.RawMessage) any {
					switch method {
					case "thread/turns/list":
						return map[string]any{"data": []Turn{{ID: "applied-turn", Status: "completed"}}}
					case "thread/items/list":
						return map[string]any{"data": []any{map[string]any{"turnId": "applied-turn", "item": Item{Type: "functionCallOutput", Name: ToolName, Namespace: ToolNamespace, Output: json.RawMessage(body)}}}}
					}
					t.Error("unexpected method", method)
					return nil
				})
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				c := fixtureCurrentClient(t, ctx, endpoint, version)
				turn, err := c.Reconcile(ctx, b.ThreadID, attempt.Output, attempt.Started)
				t.Logf("mode=%s turn=%q err=%v", mode, turn, err)
				if mode == "valid" && turn != "applied-turn" {
					t.Fatal("positive control lost", err)
				}
				if mode != "valid" && turn != "" {
					t.Errorf("Reconcile accepted ambiguous duplicate-key content (%s)", strings.TrimPrefix(mode, "dup-"))
				}
			})
		}
	}
}

// Observation (non-blocking): production matching is version-unscoped, so a
// legacy 0.161.0 client now also accepts current input_text, and current
// clients accept legacy inputText. Logged only; asserts nothing.
func TestReviewR1VersionUnscopedMatcherObservation(t *testing.T) {
	valid, legacy, _, _, _ := reviewR1Items(currentNotice)
	item := Item{Type: "functionCallOutput", Name: ToolName, Namespace: ToolNamespace}
	item.Output = json.RawMessage(valid)
	t.Logf("version-free matcher accepts input_text: %v", matchesOutput(item, currentNotice))
	item.Output = json.RawMessage(legacy)
	t.Logf("version-free matcher accepts inputText: %v", matchesOutput(item, currentNotice))
}

// The production matcher accepts only exact authoritative strings or homogeneous
// strict text arrays. These cases pair the R1 bypasses with valid current/legacy
// neighbors; they exercise matchesOutput, Accepted and real paged Reconcile,
// rather than inferring production strictness from the helper alone.
func productionMatcherCases(output string) []struct {
	name string
	raw  string
	want bool
} {
	quote := func(text string) string { raw, _ := json.Marshal(text); return string(raw) }
	x := quote(output)
	part := func(kind, text string) string { return `{"type":` + quote(kind) + `,"text":` + quote(text) + `}` }
	mid := len(output) / 2
	valid, legacy, dupStrict, dupFall, dupMixed := reviewR1Items(output)
	return []struct {
		name, raw string
		want      bool
	}{
		{"string", x, true},
		{"current", valid, true},
		{"legacy", legacy, true},
		{"current-split", `[` + part("input_text", output[:mid]) + `,` + part("input_text", output[mid:]) + `]`, true},
		{"legacy-split", `[` + part("inputText", output[:mid]) + `,` + part("inputText", output[mid:]) + `]`, true},
		{"legacy-reordered-fields", `[{"text":` + x + `,"type":"inputText"}]`, true},
		{"dup-strict", dupStrict, false},
		{"dup-text-then-legacy-type", dupFall, false},
		{"mixed-image-then-legacy-type", dupMixed, false},
		{"duplicate-type-current-last", `[{"type":"inputText","type":"input_text","text":` + x + `}]`, false},
		{"duplicate-type-legacy-last", `[{"type":"input_text","type":"inputText","text":` + x + `}]`, false},
		{"duplicate-type-same-legacy", `[{"type":"inputText","type":"inputText","text":` + x + `}]`, false},
		{"duplicate-type-same-current", `[{"type":"input_text","type":"input_text","text":` + x + `}]`, false},
		{"duplicate-text-legacy-exact-last", `[{"type":"inputText","text":"peer","text":` + x + `}]`, false},
		{"duplicate-text-legacy-exact-first", `[{"type":"inputText","text":` + x + `,"text":"peer"}]`, false},
		{"duplicate-text-current-exact-first", `[{"type":"input_text","text":` + x + `,"text":"peer"}]`, false},
		{"duplicate-text-same-legacy", `[{"type":"inputText","text":` + x + `,"text":` + x + `}]`, false},
		{"legacy-unknown-media-key", `[{"type":"inputText","text":` + x + `,"image_url":"peer"}]`, false},
		{"current-unknown-media-key", `[{"type":"input_text","text":` + x + `,"image_url":"peer"}]`, false},
		{"mixed-tags-current-first", `[` + part("input_text", output[:mid]) + `,` + part("inputText", output[mid:]) + `]`, false},
		{"mixed-tags-legacy-first", `[` + part("inputText", output[:mid]) + `,` + part("input_text", output[mid:]) + `]`, false},
		{"mixed-media", `[` + part("inputText", output) + `,{"type":"input_image","image_url":"peer"}]`, false},
		{"stale-string", quote(output + "-stale"), false},
		{"stale-current", `[` + part("input_text", output+"-stale") + `]`, false},
		{"stale-legacy", `[` + part("inputText", output+"-stale") + `]`, false},
		{"missing-text", `[{"type":"inputText"}]`, false},
		{"null-text", `[{"type":"inputText","text":null}]`, false},
		{"nonstring-text", `[{"type":"inputText","text":1}]`, false},
		{"case-folded-field", `[{"Type":"inputText","text":` + x + `}]`, false},
		{"null-part", `[null,` + part("inputText", output) + `]`, false},
		{"empty-array", `[]`, false},
	}
}

func TestCurrentProductionMatcherStrictFallback(t *testing.T) {
	for _, tc := range productionMatcherCases(currentNotice) {
		t.Run(tc.name, func(t *testing.T) {
			item := Item{Type: "functionCallOutput", Name: ToolName, Namespace: ToolNamespace, Output: json.RawMessage(tc.raw)}
			if got := matchesOutput(item, currentNotice); got != tc.want {
				t.Errorf("production matchesOutput=%v want=%v", got, tc.want)
			}
			turn, accepted := Accepted(Snapshot{Turns: []Turn{{ID: "applied-turn", Items: []Item{item}}}}, currentNotice)
			if accepted != tc.want || (tc.want && turn != "applied-turn") || (!tc.want && turn != "") {
				t.Errorf("production Accepted=(%q,%v) want accepted=%v", turn, accepted, tc.want)
			}
		})
	}
	// Even exact valid text has no acceptance authority in peer/user/foreign tools.
	for _, field := range []string{"type", "name", "namespace"} {
		t.Run("foreign-"+field, func(t *testing.T) {
			_, raw, _, _, _ := reviewR1Items(currentNotice)
			item := Item{Type: "functionCallOutput", Name: ToolName, Namespace: ToolNamespace, Output: json.RawMessage(raw)}
			switch field {
			case "type":
				item.Type = "userMessage"
			case "name":
				item.Name = "peer"
			case "namespace":
				item.Namespace = "peer"
			}
			_, accepted := Accepted(Snapshot{Turns: []Turn{{ID: "applied-turn", Items: []Item{item}}}}, currentNotice)
			if matchesOutput(item, currentNotice) || accepted {
				t.Fatal("foreign item acquired acceptance authority")
			}
		})
	}
}

// The real Client.Reconcile must page past stale output, then accept only the
// strict exact witness; absence stays inconclusive with the ledger fenced and
// no turn/start or other mutating RPC. This is an offline Unix fixture only:
// constructing the private test client does not admit either native version.
func TestCurrentProductionReconcileStrictFallback(t *testing.T) {
	for _, version := range []string{"0.163.0", "0.165.0"} {
		for _, tc := range productionMatcherCases(currentNotice) {
			t.Run(version+"/"+tc.name, func(t *testing.T) {
				b, ledger, attempt := currentAttempt(t, version)
				var raw string
				for _, c := range productionMatcherCases(attempt.Output) {
					if c.name == tc.name {
						raw = c.raw
						break
					}
				}
				before, _ := json.Marshal(ledger)
				calls := 0
				endpoint := testServer(t, func(method string, body json.RawMessage) any {
					calls++
					var p struct {
						ThreadID      string `json:"threadId"`
						TurnID        string `json:"turnId"`
						Cursor        string `json:"cursor"`
						Limit         int    `json:"limit"`
						ItemsView     string `json:"itemsView"`
						SortDirection string `json:"sortDirection"`
					}
					if json.Unmarshal(body, &p) != nil || p.ThreadID != b.ThreadID {
						t.Error("wrong reconciliation thread")
					}
					switch method {
					case "thread/turns/list":
						if p.Limit != 20 || p.ItemsView != "notLoaded" || p.SortDirection != "desc" {
							t.Error("unbounded or loaded metadata")
						}
						if calls == 1 && p.Cursor == "" {
							return map[string]any{"data": []Turn{}, "nextCursor": "turn-page-2"}
						}
						if calls != 2 || p.Cursor != "turn-page-2" {
							t.Error("metadata cursor not followed")
						}
						return map[string]any{"data": []Turn{{ID: "applied-turn", Status: "completed"}}}
					case "thread/items/list":
						if p.TurnID != "applied-turn" || p.Limit != 1 || p.SortDirection != "asc" {
							t.Error("wrong item scope/bound")
						}
						item := Item{Type: "functionCallOutput", Name: ToolName, Namespace: ToolNamespace, Output: json.RawMessage(raw)}
						response := map[string]any{}
						if calls == 3 && p.Cursor == "" {
							stale, _ := json.Marshal(attempt.Output + "-stale")
							item.Output = stale
							response["nextCursor"] = "item-page-2"
						} else if calls != 4 || p.Cursor != "item-page-2" {
							t.Error("item cursor not followed")
						}
						response["data"] = []any{map[string]any{"turnId": "applied-turn", "item": item}}
						return response
					default:
						t.Error("unexpected mutating RPC", method)
						return nil
					}
				})
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				c := fixtureCurrentClient(t, ctx, endpoint, version)
				turn, err := c.Reconcile(ctx, b.ThreadID, attempt.Output, attempt.Started)
				if err != nil {
					t.Fatal("fixture reconciliation failed", err)
				}
				want := ""
				if tc.want {
					want = "applied-turn"
				}
				if turn != want {
					t.Errorf("paged Reconcile turn=%q want=%q", turn, want)
				}
				if calls != 4 {
					t.Errorf("RPC count=%d want=4", calls)
				}
				if _, err := ledger.Begin(b, []string{"$next"}, currentNotice, attempt.Started); err == nil {
					t.Error("uncertain attempt admitted retry")
				}
				after, _ := json.Marshal(ledger)
				if string(before) != string(after) {
					t.Error("read-only reconciliation mutated ledger")
				}
			})
		}
	}
}
