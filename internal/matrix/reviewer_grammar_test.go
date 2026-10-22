package matrix

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestReviewerUnicodeCodeUnits(t *testing.T) {
	for n := 0; n <= 65535; n++ {
		raw := []byte(fmt.Sprintf(`{"event_id":"$ack","x":"\u%04x"}`, n))
		compareStrict(t, raw)
		got, e := strictJSON(raw)
		valid := n < 0xd800 || n > 0xdfff
		if (e == nil) != valid {
			t.Fatalf("U+%04X valid=%v err=%v", n, valid, e)
		}
		if valid && got.(map[string]any)["x"] != string(rune(n)) {
			t.Fatalf("U+%04X wrong value", n)
		}
	}
}
func TestReviewerPublicGrammar(t *testing.T) {
	for _, raw := range []string{
		`{"event_id":"$ack","x":{"a":1,"\u0061":2}}`,
		`{"event_id":"$ack","x":[{"":1,"":2}]}`,
		`{"event_id":"$ack","x":"\uD800\uDBFF"}`,
		`{"event_id":"$ack"}\n`,
		`{"event_id":"$ack","x":01}`,
		`{"event_id":"$ack","x":1e}`,
		`{"event_id":"$ack","x":"\u000g"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			compareStrict(t, []byte(raw))
			if _, e := wireClient(raw, 200).PutStateReceipt(t.Context(), "!r:s", "s", "k", nil); e == nil {
				t.Fatal("invalid ACK accepted")
			}
		})
	}
	for _, raw := range []string{
		`{"event_id":"$ack","EVENT_ID":"$other","x":{"é":1,"e\u0301":2}}`,
		" \r\n{\"event_id\":\"$ack\",\"x\":\"�😀\",\"n\":-0.000e+100000}\t\r\n",
		`{"\u0065vent_id":"$ack","x":"\udbff\udfff"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			compareStrict(t, []byte(raw))
			ack, e := wireClient(raw, 200).PutStateReceipt(t.Context(), "!r:s", "s", "k", nil)
			if e != nil || ack.EventID() != "$ack" || string(ack.Raw()) != raw {
				t.Fatalf("valid ACK got=%+v err=%v", ack, e)
			}
		})
	}
	// Typed number lexemes must survive independently of binary floating point.
	for _, raw := range []string{"-0", "0e0", "1E-0009", "9007199254740993", "1e99999999999999999999999999999"} {
		v, e := strictJSON([]byte(raw))
		if e != nil || !reflect.DeepEqual(v, json.Number(raw)) {
			t.Fatalf("lexeme %s got=%v err=%v", raw, v, e)
		}
		compareStrict(t, []byte(raw))
	}
}
