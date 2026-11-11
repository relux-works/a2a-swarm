package config

import (
	"encoding/json"
	"testing"
)

// Absent legacy context and malformed feature-local values preserve ordinary
// state updates, the writer seal and unrelated future fields. These values
// remain recovery data; this layer grants no pair or delivery authority.
func TestAgentDMContextStateRoundTrip(t *testing.T) {
	for _, raw := range []string{"", `null`, `false`, `"damaged"`, `{"unexpected":[1,2]}`, `{"contract":"a2a-agent-dm/v1","caller_mxid":"@a:one.invalid","connection_id":"fixture","rooms":["!retained"]}`} {
		t.Run(raw, func(t *testing.T) {
			p := testPaths(t)
			body := `{"next_batch":"before","future_field":{"number":9007199254740993}}`
			if raw != "" {
				body = `{"next_batch":"before","agent_dm_contexts":` + raw + `,"future_field":{"number":9007199254740993}}`
			}
			seedStateJSON(t, p, body)
			updated, err := UpdateState(p, "a", func(st *State) error { st.NextBatch = "after"; return nil })
			if err != nil {
				t.Fatal("feature-only context fault bricked ordinary state", err)
			}
			if updated.NextBatch != "after" || !validWriterSeal(updated) {
				t.Fatal("ordinary state writer was bypassed")
			}
			read := readStateJSON(t, p)
			if !sameStateJSON(read["future_field"], []byte(`{"number":9007199254740993}`)) {
				t.Fatal("unrelated opaque state changed")
			}
			if raw == "" {
				if _, ok := read["agent_dm_contexts"]; ok {
					t.Fatal("legacy state gained context")
				}
			} else if !sameStateJSON(read["agent_dm_contexts"], json.RawMessage(raw)) {
				t.Fatal("original feature-local recovery value erased")
			}
		})
	}
}
