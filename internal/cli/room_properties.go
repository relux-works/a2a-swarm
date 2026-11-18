package cli

import (
	"fmt"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Shared typed-property flag and result types. Only the room name and Space
// child commands use them on this line.

type propertyScalar struct {
	value string
	seen  bool
}

func (v *propertyScalar) String() string { return v.value }
func (v *propertyScalar) Set(s string) error {
	if v.seen {
		return fmt.Errorf("scalar flag repeated")
	}
	v.value, v.seen = s, true
	return nil
}

type propertyBool struct{ propertyScalar }

func (v *propertyBool) IsBoolFlag() bool { return true }
func (v *propertyBool) Set(s string) error {
	if s != "true" && s != "false" {
		return fmt.Errorf("boolean must be true or false")
	}
	return v.propertyScalar.Set(s)
}

// Version 1 projects only the selected property, never raw room state or ACK
// entities. For sets, a nonzero exit may accompany an acknowledged UNKNOWN.
type roomPropertyResult struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	Operation string `json:"operation"`
	Caller    string `json:"caller"`
	RoomID    string `json:"room_id"`
	StateType string `json:"state_type"`
	StateKey  string `json:"state_key"`
	matrix.StateWriteResult
	Content any `json:"content,omitempty"`
}
