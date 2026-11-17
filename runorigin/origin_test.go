package runorigin

import (
	"strings"
	"testing"
)

// TestValidGeneration proves only nonzero, exactly 32-byte lowercase hexadecimal strings pass the public validator.
func TestValidGeneration(t *testing.T) {
	for _, generation := range []string{"0123456789abcdef0123456789abcdef", strings.Repeat("f", 32), strings.Repeat("0", 31) + "1"} {
		t.Run("valid-"+generation, func(t *testing.T) {
			if !ValidGeneration(generation) {
				t.Fatal("valid journal spelling refused")
			}
		})
	}
	for name, generation := range map[string]string{
		"empty": "", "zero": strings.Repeat("0", 32),
		"uppercase":  "0123456789ABCDEF0123456789ABCDEF",
		"mixed-case": "0123456789abcdef0123456789abcdeF",
		"short":      strings.Repeat("a", 31), "long": strings.Repeat("a", 33),
		"non-hex": strings.Repeat("a", 31) + "g", "unicode": strings.Repeat("a", 30) + "é",
		"space": strings.Repeat("a", 31) + " ",
	} {
		t.Run(name, func(t *testing.T) {
			if ValidGeneration(generation) {
				t.Fatal("invalid journal spelling accepted")
			}
		})
	}
}
