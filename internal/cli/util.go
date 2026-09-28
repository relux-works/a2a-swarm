package cli

import (
	"encoding/json"
	"sort"
)

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
