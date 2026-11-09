package courier

import "testing"

// Proves only the closed admission-count query is added; status mutation and path options remain forbidden.
func TestAdmissionStatusBoundary(t *testing.T) {
	if _, err := Authorize([]string{"status", "admission", "--json"}, "alice"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"status", "admission"}, {"status", "set", "active"}, {"status", "admission", "--json", "--dir", "/private"}, {"status", "admission", "--json=true"}} {
		if _, err := Authorize(args, "alice"); err == nil {
			t.Fatal("unexpected status authority")
		}
	}
}
