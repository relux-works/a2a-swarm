package signedpost

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Claim: the library signs preimages just under and at 24576 bytes, but refuses 24577 bytes with the exact local code and zero Sign calls.
func TestSignPreimageCap(t *testing.T) {
	f := &fakeSigner{identity: validIdentity(t)}
	now := testNow
	d := draft("hello", "@one:test.invalid")
	if _, err := newSender(f, &now).Sign(context.Background(), d); err != nil || len(f.preimages) != 1 {
		t.Fatalf("sizing control: %v", err)
	}
	base := len(f.preimages[0])
	for _, size := range []int{24575, 24576, 24577} {
		t.Run(fmt.Sprintf("bytes-%d", size), func(t *testing.T) {
			f := &fakeSigner{identity: validIdentity(t)}
			d := d
			// Keep each string within the module's independent 8192-byte
			// parse bound so the controls reach actual signing admission.
			growth := size - base
			growth -= 8192 - len(d.Topic)
			d.Topic = strings.Repeat("x", 8192)
			d.Re = strings.Repeat("x", 8192)
			growth -= len(d.Re)
			d.Kind += strings.Repeat("x", growth)
			signed, err := newSender(f, &now).Sign(context.Background(), d)
			if size > 24576 {
				wantRefusal(t, err, "recipients_too_many", "signing input over 24576 bytes", false)
				if f.signCalls != 0 || !reflect.DeepEqual(signed, Signed{}) {
					t.Fatal("oversize preimage reached Sign or returned an envelope")
				}
			} else if err != nil || f.signCalls != 1 || len(f.preimages[0]) != size || len(signed.Envelope) == 0 {
				t.Fatalf("valid preimage refused: %v, Sign calls %d", err, f.signCalls)
			}
		})
	}
}

// Claim: mutation of the signer's returned attestation during qualification cannot alter the checked identity or prepared bytes, beside an unchanged control.
func TestPrepareOwnsAttestationBeforeQualification(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		t.Run(fmt.Sprintf("mutate-%v", mutate), func(t *testing.T) {
			f := &fakeSigner{identity: validIdentity(t)}
			original := append([]byte(nil), f.identity.Attestation...)
			now := testNow
			s := newSender(f, &now)
			s.Qualified = func(id Identity) bool {
				// Identity has returned. Mutate its signer's retained slice at
				// this deterministic boundary, without racing a slice read.
				if mutate {
					i := bytes.Index(f.identity.Attestation, []byte("generation-0"))
					if i < 0 {
						t.Fatal("fixture lacks the byte to change")
					}
					f.identity.Attestation[i+len("generation-")] = '1'
				}
				return bytes.Equal(id.Attestation, original) && CheckAttestation(id) == nil
			}
			p, err := s.Prepare(context.Background(), testRoom, nil)
			if err != nil || !bytes.Equal(p.Identity.Attestation, original) || CheckAttestation(p.Identity) != nil || f.identityCalls != 1 || f.signCalls != 0 {
				t.Fatalf("prepared identity changed after signer mutation: %v", err)
			}
			if mutate && bytes.Equal(f.identity.Attestation, original) {
				t.Fatal("mutation control did not change the signer's retained slice")
			}
		})
	}
}
