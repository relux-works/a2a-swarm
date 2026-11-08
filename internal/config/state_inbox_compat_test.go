package config

import (
	"encoding/json"
	"testing"
)

func TestInboxPathAndOpaqueStateCompatibility(t *testing.T) {
	const input = `{"next_batch":"before","last_inbox":"/private/saved-frame","future_member":{"n":9007199254740993}}`
	// The pre-preservation writer's struct knew only its own members. This
	// counterexample models its JSON rewrite without running a historical CLI.
	var legacy struct {
		NextBatch string `json:"next_batch"`
	}
	if err := json.Unmarshal([]byte(input), &legacy); err != nil {
		t.Fatal(err)
	}
	lost, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var rewritten map[string]json.RawMessage
	if err := json.Unmarshal(lost, &rewritten); err != nil {
		t.Fatal(err)
	}
	if rewritten["last_inbox"] != nil || rewritten["future_member"] != nil {
		t.Fatal("legacy fixture did not model unknown-member loss")
	}
	p := testPaths(t)
	seedStateJSON(t, p, input)
	if _, err := UpdateState(p, "a", func(st *State) error { st.NextBatch = "after"; return nil }); err != nil {
		t.Fatal(err)
	}
	got := readStateJSON(t, p)
	if string(got["last_inbox"]) != `"/private/saved-frame"` || !sameStateJSON(got["future_member"], []byte(`{"n":9007199254740993}`)) {
		t.Fatal("current writer lost last_inbox or opaque state")
	}
}
