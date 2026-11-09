//go:build linux || darwin

package courierenroll

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Proves the four-entry reader and rotation verifier reject incomplete,
// malformed and extended generations beside a valid generation. No live daemon.
func TestStationFourEntryContract(t *testing.T) {
	for _, mutation := range []string{"valid", "missing", "invalid", "extra", "mode", "alias"} {
		t.Run(mutation, func(t *testing.T) {
			dir := target(t)
			if err := Install(dir, strings.NewReader(fixture("identity", 17)), false); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "missing":
				if err := os.Remove(filepath.Join(dir, "station")); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				if err := os.WriteFile(filepath.Join(dir, "station"), []byte("Bad/Name\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "extra":
				if err := os.WriteFile(filepath.Join(dir, "extra"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(filepath.Join(dir, "station"), 0644); err != nil {
					t.Fatal(err)
				}
			case "alias":
				alias := filepath.Join(filepath.Dir(dir), "alias")
				if err := os.Symlink(filepath.Dir(dir), alias); err != nil {
					t.Fatal(err)
				}
				dir = filepath.Join(alias, ".a2a")
			}
			e, present, err := Read(dir)
			defer clear(e.Key)
			if !present {
				t.Fatal("existing enrollment treated as absent")
			}
			if mutation == "valid" {
				if err != nil || e.Station != "station-1" || e.Identity != "identity" {
					t.Fatal("valid neighbor refused")
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "re-run enroll") {
					t.Fatal("invalid generation accepted or missing recovery guidance")
				}
				if Install(dir, strings.NewReader(fixture("next", 18)), true) == nil {
					t.Fatal("rotation accepted invalid generation")
				}
			}
		})
	}
}

// Proves the station grammar matches legitimate registry names and rejects the
// previously admitted opaque reference syntax; does not test registry writes.
func TestStationRegistryGrammar(t *testing.T) {
	for _, s := range []string{"x", "a0._=-", strings.Repeat("s", 64)} {
		if !validStation(s) {
			t.Fatal("valid station refused")
		}
	}
	for _, s := range []string{"", "a/b", "A", "a b", "s\n", strings.Repeat("s", 65)} {
		if validStation(s) {
			t.Fatal("invalid station accepted")
		}
	}
}
