package config

import (
	"encoding/json"
	"reflect"
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
	// Keep the existing typed decoder's validation and case-fold behavior. Raw
	// messages retain number precision and complete values without interpreting
	// unknown data or preserving unknown members inside known typed objects.
	if err := json.Unmarshal(data, s); err != nil {
		return err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return err
	}
	for name := range members {
		if knownStateJSONField(name) {
			delete(members, name)
		}
	}
	s.opaque = members
	return nil
}

func marshalState(s State) ([]byte, error) {
	if len(s.opaque) == 0 {
		return json.MarshalIndent(s, "", "  ")
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
		if !knownStateJSONField(name) {
			members[name] = value
		}
	}
	return json.MarshalIndent(members, "", "  ")
}
