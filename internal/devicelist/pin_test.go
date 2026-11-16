package devicelist

import (
	"strings"
	"testing"
)

// Claim: the pin is read from the name value by exact member name and in its
// one spelling (O3.4, O3.5). Controls: the member alone, beside other
// members, and with white space between the members of the top level.
// Negatives, each DEVICE_PIN_INVALID: no member, the member twice, a
// case-fold alias beside or instead of it, a member with an extra, missing or
// reordered member or inner whitespace, a seq of 0 or above the bound, a hash
// that is not 64 lowercase hex, a value that is not one object, and trailing
// input.
// Claim: only an exact, unique swarma-devices member yields a valid pin beside the listed malformed controls.
func TestPinFromNameValue(t *testing.T) {
	member := `"swarma-devices":{"seq":5,"sha256":"` + exampleHash + `"}`
	want := Pin{Seq: 5, SHA256: exampleHash}
	for _, value := range []string{
		`{` + member + `}`,
		`{"swarma":{"v":1},` + member + `,"nostr":"x"}`,
		`{ "swarma" : {} , "swarma-devices" : {"seq":5,"sha256":"` + exampleHash + `"} }`,
	} {
		if pin, err := PinFromNameValue([]byte(value)); err != nil || pin != want {
			t.Fatalf("control %s: %v", value, err)
		}
	}
	upper := "B" + exampleHash[1:]
	for name, value := range map[string]string{
		"empty":            ``,
		"no member":        `{"swarma":{}}`,
		"member twice":     `{` + member + `,` + member + `}`,
		"alias beside":     `{` + member + `,"Swarma-Devices":1}`,
		"alias instead":    `{"SWARMA-DEVICES":{"seq":5,"sha256":"` + exampleHash + `"}}`,
		"extra member":     `{"swarma-devices":{"seq":5,"sha256":"` + exampleHash + `","x":1}}`,
		"missing member":   `{"swarma-devices":{"seq":5}}`,
		"reordered":        `{"swarma-devices":{"sha256":"` + exampleHash + `","seq":5}}`,
		"inner whitespace": `{"swarma-devices":{"seq": 5,"sha256":"` + exampleHash + `"}}`,
		"seq zero":         `{"swarma-devices":{"seq":0,"sha256":"` + exampleHash + `"}}`,
		"seq over bound":   `{"swarma-devices":{"seq":9007199254740992,"sha256":"` + exampleHash + `"}}`,
		"seq string":       `{"swarma-devices":{"seq":"5","sha256":"` + exampleHash + `"}}`,
		"uppercase hash":   `{"swarma-devices":{"seq":5,"sha256":"` + upper + `"}}`,
		"short hash":       `{"swarma-devices":{"seq":5,"sha256":"` + exampleHash[2:] + `"}}`,
		"member null":      `{"swarma-devices":null}`,
		"array":            `[` + `{` + member + `}` + `]`,
		"trailing input":   `{` + member + `}{}`,
		"trailing brace":   `{` + member + `}}`,
		"unterminated":     `{` + member,
	} {
		if pin, err := PinFromNameValue([]byte(value)); err != DevicePinInvalid || pin != (Pin{}) {
			t.Fatalf("%s: want DEVICE_PIN_INVALID, got %v %v", name, pin, err)
		}
	}
}

// Claim: the pin reader enforces exact keys, boundaries and framing (O3.5, LIP-0010 SH6).
// The pin reader is as
// strict as the name value rules. Each negative stands next to the control it
// differs from in one thing, and refuses DEVICE_PIN_INVALID:
//
//   - a member name that decodes to swarma-devices but is spelled with a JSON
//     escape, alone or beside the plain spelling; control: the plain
//     spelling, and an escaped name of a member that is no alias;
//   - a member name that folds to swarma-devices: U+017F for s, written as
//     the character and as an escape; control: the plain spelling;
//   - any octet after the object, a newline and a space included, and white
//     space before it; control: the object alone;
//   - a value of 521 bytes; control: the same value at exactly 520 bytes.
func TestPinReaderStrictness(t *testing.T) {
	pin := `{"seq":5,"sha256":"` + exampleHash + `"}`
	plain := `{"swarma-devices":` + pin + `}`
	want := Pin{Seq: 5, SHA256: exampleHash}
	padded := func(size int) string {
		head, tail := `{"pad":"`, `","swarma-devices":`+pin+`}`
		return head + strings.Repeat("x", size-len(head)-len(tail)) + tail
	}
	if len(padded(520)) != 520 || len(padded(521)) != 521 {
		t.Fatal("fixture: the padded values do not have the stated sizes")
	}
	for name, value := range map[string]string{
		"plain":                     plain,
		"escaped name of no alias":  `{"caf\u00e9":1,"swarma-devices":` + pin + `}`,
		"escaped name of a sibling": `{"swarma-device\u0074":1,"swarma-devices":` + pin + `}`,
		"exactly 520 bytes":         padded(520),
	} {
		if got, err := PinFromNameValue([]byte(value)); err != nil || got != want {
			t.Fatalf("control %s: %v %v", name, got, err)
		}
	}
	for name, value := range map[string]string{
		"escaped name alone":        `{"swarma-device\u0073":` + pin + `}`,
		"escaped name, first octet": `{"\u0073warma-devices":` + pin + `}`,
		"escaped hyphen":            `{"swarma\u002ddevices":` + pin + `}`,
		"escaped beside plain":      `{"swarma-devices":` + pin + `,"swarma-device\u0073":1}`,
		"plain beside escaped":      `{"swarma-device\u0073":1,"swarma-devices":` + pin + `}`,
		"long s, literal":           "{\"\u017fwarma-devices\":" + pin + `}`,
		"long s, escaped":           `{"\u017fwarma-devices":` + pin + `}`,
		"long s beside plain":       "{\"swarma-devices\":" + pin + ",\"swarma-device\u017f\":1}",
		"trailing newline":          plain + "\n",
		"trailing space":            plain + " ",
		"trailing tab":              plain + "\t",
		"leading space":             " " + plain,
		"leading newline":           "\n" + plain,
		"521 bytes":                 padded(521),
	} {
		if got, err := PinFromNameValue([]byte(value)); err != DevicePinInvalid || got != (Pin{}) {
			t.Fatalf("%s: want DEVICE_PIN_INVALID, got %v %v", name, got, err)
		}
	}
	if MaxNameValue != 520 {
		t.Fatal("the bound of a name value is not the 520 bytes of LIP-0010")
	}
}
