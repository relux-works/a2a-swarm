package config

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// State has flat exported JSON fields. Derive their names from that schema so
// a newly added typed field (including an omitted zero value) cannot be retained
// as opaque data. EqualFold matches encoding/json's accepted case variants.
var stateJSONFields = func() []string {
	typ := reflect.TypeFor[State]()
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		names = append(names, name)
	}
	return names
}()

func knownStateJSONField(name string) bool {
	for _, field := range stateJSONFields {
		if strings.EqualFold(name, field) {
			return true
		}
	}
	return false
}

func unmarshalState(data []byte, s *State) error {
	globalStateStats.unmarshalPasses.Add(1)
	globalStateStats.unmarshalBytes.Add(int64(len(data)))
	// Keep the existing typed decoder's validation and case-fold behavior. Raw
	// messages retain number precision and complete values without interpreting
	// unknown data or preserving unknown members inside known typed objects.
	// Writer is compatibility metadata: decode it separately so malformed
	// metadata cannot brick otherwise valid State. All other typed members
	// retain encoding/json's strict type validation and original order.
	envelope := struct {
		*State
		Writer json.RawMessage `json:"writer"`
	}{State: s}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if err := rejectAuthorityAliases(data); err != nil {
		return err
	}
	s.Writer = nil
	var writer StateWriter
	if len(envelope.Writer) != 0 && json.Unmarshal(envelope.Writer, &writer) == nil {
		s.Writer = &writer
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return err
	}
	// An old-format file still carries the retained thread window in a
	// thread_events member. Decode it (with the typed decoder's former
	// validation and case-fold acceptance) into the legacy slot for
	// migration and union reads, and keep it out of the opaque members so
	// no save ever writes it back.
	s.legacyThreadEvents = nil
	var legacyKeys []string
	for name := range members {
		if strings.EqualFold(name, "thread_events") {
			legacyKeys = append(legacyKeys, name)
		}
	}
	if len(legacyKeys) > 0 {
		sort.Strings(legacyKeys)
		key := legacyKeys[0]
		for _, k := range legacyKeys {
			if k == "thread_events" {
				key = k
			}
		}
		raw := members[key]
		for _, k := range legacyKeys {
			delete(members, k)
		}
		if len(bytes.TrimSpace(raw)) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			var legacy []ThreadEvent
			if err := json.Unmarshal(raw, &legacy); err != nil {
				return err
			}
			s.legacyThreadEvents = legacy
		}
	}
	for name := range members {
		if knownStateJSONField(name) {
			delete(members, name)
		}
	}
	if len(members) == 0 {
		members = nil
	}
	s.opaque = members
	s.rawState = append([]byte(nil), data...)
	return nil
}

func marshalState(s State) ([]byte, error) {
	globalStateStats.stateEncodes.Add(1)
	compact := s.HostEnrollment != nil && s.HostEnrollment.Version == 2
	if len(s.opaque) == 0 {
		return encodeStateJSON(s, compact)
	}
	known, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(known, &members); err != nil {
		return nil, err
	}
	// Merge into a fresh map, never into the map shared by State value copies.
	// Check against the entire schema, not only the non-omitted emitted fields.
	for name, value := range s.opaque {
		// The retired thread_events member is never written back, however
		// it reached opaque storage; its entries live in the thread file.
		if !knownStateJSONField(name) && !strings.EqualFold(name, "thread_events") {
			members[name] = value
		}
	}
	return encodeStateJSON(members, compact)
}

// Host v2 counts the compact retained ledger; legacy and v1 keep their formatting.
func encodeStateJSON(value any, compact bool) ([]byte, error) {
	if compact {
		return json.Marshal(value)
	}
	return json.MarshalIndent(value, "", "  ")
}
