package e2ee

import (
	"bytes"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"maunium.net/go/mautrix/crypto/olm"
	"maunium.net/go/mautrix/id"
)

func working(t testing.TB) (*Engine, *Working, []byte) {
	t.Helper()
	engine := NewEngine(new(Inventory))
	w, err := engine.New()
	if err != nil {
		t.Fatal(err)
	}
	return engine, w, bytes.Repeat([]byte{0x42}, 32)
}
func prekey(t testing.TB) (*Working, []byte, id.Curve25519, string, []byte) {
	t.Helper()
	_, bob, key := working(t)
	_, alice, _ := working(t)
	keys, err := bob.GenerateOneTimeKeys(1)
	if err != nil {
		t.Fatal(err)
	}
	var otk id.Curve25519
	for _, k := range keys {
		otk = k
	}
	_, bobCurve, err := bob.IdentityKeys()
	if err != nil {
		t.Fatal(err)
	}
	_, aliceCurve, err := alice.IdentityKeys()
	if err != nil {
		t.Fatal(err)
	}
	sid, err := alice.NewOutboundOlm(bobCurve, otk)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("{ \"type\" : \"test\", \"content\": {\"body\":\"exact\\nbytes\"} }")
	kind, ciphertext, err := alice.EncryptOlm(sid, plaintext)
	if err != nil || kind != id.OlmMsgTypePreKey {
		t.Fatal("prekey control", err)
	}
	return bob, key, aliceCurve, string(ciphertext), plaintext
}

// Real goolm prekey decryption consumes one OTK only on the isolated fork;
// discarding a wrong-source or rejected-payload fork preserves the prior state.
func TestEngineOlmSavepoints(t *testing.T) {
	bob, key, sender, ciphertext, plain := prekey(t)
	before, err := bob.Checkpoint(key)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := bob.Fork(key)
	if err != nil {
		t.Fatal(err)
	}
	badSource := id.Curve25519(base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)))
	if _, _, err = wrong.DecryptPrekey(badSource, ciphertext); err != ErrCodec {
		t.Fatal("wrong sender rejection", err)
	}
	after, _ := bob.Checkpoint(key)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("rejected attempt changed original")
	}
	accepted, err := bob.Fork(key)
	if err != nil {
		t.Fatal(err)
	}
	sid, got, err := accepted.DecryptPrekey(sender, ciphertext)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal("valid prekey", err)
	}
	if len(accepted.account.OTKeys) != 0 || len(bob.account.OTKeys) != 1 {
		t.Fatal("OTK isolation failed")
	}
	persisted, err := accepted.Checkpoint(key)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := bob.engine.Restore(persisted, key)
	if err != nil {
		t.Fatal(err)
	}
	if restored.olm[string(sid)] == nil || len(restored.account.OTKeys) != 0 {
		t.Fatal("ratchet/OTK persistence failed")
	}
	if _, _, err = restored.DecryptPrekey(sender, ciphertext); err != ErrCodec {
		t.Fatal("consumed prekey rejection", err)
	}
	fresh, err := bob.Fork(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = fresh.DecryptPrekey(sender, ciphertext); err != nil {
		t.Fatal("discarded fork consumed original prekey", err)
	}
}

// Exact Megolm plaintext and authenticated message index survive checkpoints;
// corrupt signatures, unknown sessions, wrong pickle keys and unknown versions
// refuse beside the real successful path. This is not live-client interop proof.
func TestEngineMegolmPersistenceAndIntegrity(t *testing.T) {
	engine, w, key := working(t)
	sid, sharing, err := w.NewOutboundMegolm()
	if err != nil {
		t.Fatal(err)
	}
	if accepted, err := w.AcceptDirectMegolm(sharing); err != nil || accepted != sid {
		t.Fatal("direct session", err)
	}
	if _, err = w.AcceptDirectMegolm(sharing); err != ErrExists {
		t.Fatalf("duplicate session: %v", err)
	}
	raw := []byte("{\n \"body\" : \"literal\\u0020text\", \"n\":1.00 }")
	ciphertext, err := w.EncryptMegolm(sid, raw)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := w.Checkpoint(key)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := engine.Restore(checkpoint, key)
	if err != nil {
		t.Fatal(err)
	}
	got, index, err := restored.DecryptMegolm(sid, ciphertext)
	if err != nil || index != 0 || !bytes.Equal(got, raw) {
		t.Fatal("lossless decrypt", err)
	}
	binary, err := base64.RawStdEncoding.DecodeString(string(ciphertext))
	if err != nil {
		t.Fatal(err)
	}
	binary[len(binary)-1] ^= 1
	corrupted := []byte(base64.RawStdEncoding.EncodeToString(binary))
	if _, _, err = restored.DecryptMegolm(sid, corrupted); err != ErrIntegrity {
		t.Fatalf("signature control missed: %v", err)
	}
	if _, _, err = restored.DecryptMegolm("unknown", ciphertext); err != ErrUnavailable {
		t.Fatalf("unknown session: %v", err)
	}
	if _, err = engine.Restore(checkpoint, bytes.Repeat([]byte{2}, 32)); err != ErrIntegrity {
		t.Fatal("wrong pickle key rejection", err)
	}
	checkpoint.Version = 2
	if _, err = engine.Restore(checkpoint, key); err != ErrCodec {
		t.Fatalf("unknown engine codec: %v", err)
	}
	if engineError(olm.ErrBadMAC) != ErrIntegrity || engineError(olm.ErrBadSignature) != ErrIntegrity {
		t.Fatal("typed integrity mapping")
	}
}

// OTK/fallback generation remains locally bounded and encoded secrets from the
// actual library account, ratchet and pickles are registered before diagnostics.
func TestEngineInventoryAndBounds(t *testing.T) {
	engine, w, key := working(t)
	if _, err := w.GenerateOneTimeKeys(1); err != nil {
		t.Fatal(err)
	}
	if _, err := w.GenerateOneTimeKeys(101); err != ErrInvalid {
		t.Fatal("OTK bound", err)
	}
	if _, err := w.GenerateFallbackKey(); err != nil {
		t.Fatal(err)
	}
	state, err := w.Checkpoint(key)
	if err != nil {
		t.Fatal(err)
	}
	sid, _, err := w.NewOutboundMegolm()
	if err != nil {
		t.Fatal(err)
	}
	megolmData := w.outbound[string(sid)].Ratchet.Data[:]
	if encoded := base64.RawURLEncoding.EncodeToString(megolmData); strings.Contains(engine.secrets.Scrub(encoded), encoded) {
		t.Fatal("fixed-array ratchet secret survived")
	}
	for _, secret := range [][]byte{w.account.IdKeys.Ed25519.PrivateKey, w.account.IdKeys.Curve25519.PrivateKey, w.account.OTKeys[0].Key.PrivateKey, w.account.CurrentFallbackKey.Key.PrivateKey, state.Account} {
		for _, encoded := range []string{string(secret), base64.RawStdEncoding.EncodeToString(secret), base64.RawURLEncoding.EncodeToString(secret)} {
			if strings.Contains(engine.secrets.Scrub(encoded), encoded) {
				t.Fatal("library secret encoding survived")
			}
		}
	}
}
