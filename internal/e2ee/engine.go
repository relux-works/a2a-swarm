package e2ee

import (
	"encoding/json"
	"errors"

	"maunium.net/go/mautrix/crypto/goolm/account"
	"maunium.net/go/mautrix/crypto/goolm/session"
	"maunium.net/go/mautrix/crypto/olm"
	"maunium.net/go/mautrix/id"
)

// Engine uses concrete pure-Go primitives, never OlmMachine (which owns network
// side effects and mutates before payload admission). Working is always an
// isolated savepoint. U3/U4 must validate policy before invoking it and commit
// only accepted savepoints into a prepared snapshot. No library globals change.
type Engine struct{ secrets *Inventory }

func NewEngine(secrets *Inventory) *Engine {
	if secrets == nil {
		secrets = new(Inventory)
	}
	return &Engine{secrets: secrets}
}

type EngineState struct {
	Version  int               `json:"v"`
	Account  []byte            `json:"account"`
	Olm      map[string][]byte `json:"olm"`
	Inbound  map[string][]byte `json:"inbound"`
	Outbound map[string][]byte `json:"outbound"`
}

func (s EngineState) String() string   { return "<e2ee-engine-state>" }
func (s EngineState) GoString() string { return s.String() }

// Working deliberately has no String/JSON representation exposing crypto structs.
type Working struct {
	engine   *Engine
	account  *account.Account
	olm      map[string]*session.OlmSession
	inbound  map[string]*session.MegolmInboundSession
	outbound map[string]*session.MegolmOutboundSession
}

func (e *Engine) New() (*Working, error) {
	a, err := account.NewAccount()
	if err != nil {
		return nil, ErrUnavailable
	}
	w := &Working{e, a, map[string]*session.OlmSession{}, map[string]*session.MegolmInboundSession{}, map[string]*session.MegolmOutboundSession{}}
	e.observe(a)
	return w, nil
}
func (e *Engine) observe(v any) {
	b, err := json.Marshal(v)
	if err == nil {
		e.secrets.ObserveJSON(b)
	}
}

// Checkpoint uses explicit library pickles, not gob/global pickle keys. The
// caller supplies the per-generation wrapping key; the outer AEAD binds identity.
func (w *Working) Checkpoint(key []byte) (EngineState, error) {
	if len(key) != 32 {
		return EngineState{}, ErrInvalid
	}
	w.engine.secrets.Register(key)
	out := EngineState{Version: 1, Olm: map[string][]byte{}, Inbound: map[string][]byte{}, Outbound: map[string][]byte{}}
	var err error
	out.Account, err = w.account.Pickle(key)
	if err != nil {
		return EngineState{}, engineError(err)
	}
	for k, v := range w.olm {
		out.Olm[k], err = v.Pickle(key)
		if err != nil {
			return EngineState{}, engineError(err)
		}
	}
	for k, v := range w.inbound {
		out.Inbound[k], err = v.Pickle(key)
		if err != nil {
			return EngineState{}, engineError(err)
		}
	}
	for k, v := range w.outbound {
		out.Outbound[k], err = v.Pickle(key)
		if err != nil {
			return EngineState{}, engineError(err)
		}
	}
	w.engine.observe(out)
	return out, nil
}
func (e *Engine) Restore(state EngineState, key []byte) (*Working, error) {
	if state.Version != 1 || len(key) != 32 || len(state.Account) == 0 || len(state.Account) > 1<<20 || len(state.Olm) > 1024 || len(state.Inbound) > 128 || len(state.Outbound) > 32 {
		return nil, ErrCodec
	}
	e.secrets.Register(key)
	e.observe(state)
	raw, err := checkedPickle("account", state.Account, key, e.secrets)
	if err != nil {
		return nil, err
	}
	a := &account.Account{}
	if err = a.UnpickleLibOlm(raw); err != nil {
		return nil, engineError(err)
	}
	w := &Working{e, a, map[string]*session.OlmSession{}, map[string]*session.MegolmInboundSession{}, map[string]*session.MegolmOutboundSession{}}
	for k, v := range state.Olm {
		if len(v) > 1<<20 {
			return nil, ErrCodec
		}
		raw, err := checkedPickle("olm", v, key, e.secrets)
		if err != nil {
			return nil, err
		}
		s := &session.OlmSession{}
		if err = s.UnpickleLibOlm(raw); err != nil {
			return nil, engineError(err)
		}
		if string(s.ID()) != k {
			return nil, ErrIntegrity
		}
		w.olm[k] = s
		e.observe(s)
	}
	for k, v := range state.Inbound {
		if len(v) > 1<<20 {
			return nil, ErrCodec
		}
		raw, err := checkedPickle("inbound", v, key, e.secrets)
		if err != nil {
			return nil, err
		}
		s := &session.MegolmInboundSession{}
		if err = s.UnpickleLibOlm(raw); err != nil {
			return nil, engineError(err)
		}
		if string(s.ID()) != k {
			return nil, ErrIntegrity
		}
		w.inbound[k] = s
		e.observe(s)
	}
	for k, v := range state.Outbound {
		if len(v) > 1<<20 {
			return nil, ErrCodec
		}
		raw, err := checkedPickle("outbound", v, key, e.secrets)
		if err != nil {
			return nil, err
		}
		s := &session.MegolmOutboundSession{}
		if err = s.UnpickleLibOlm(raw); err != nil {
			return nil, engineError(err)
		}
		if string(s.ID()) != k {
			return nil, ErrIntegrity
		}
		w.outbound[k] = s
		e.observe(s)
	}
	e.observe(a)
	return w, nil
}
func (w *Working) Fork(key []byte) (*Working, error) {
	state, err := w.Checkpoint(key)
	if err != nil {
		return nil, err
	}
	return w.engine.Restore(state, key)
}
func (w *Working) IdentityKeys() (id.Ed25519, id.Curve25519, error) {
	ed, curve, err := w.account.IdentityKeys()
	if err != nil {
		return "", "", engineError(err)
	}
	return ed, curve, nil
}
func (w *Working) GenerateOneTimeKeys(n uint) (map[string]id.Curve25519, error) {
	if n == 0 || n > 100 || uint(len(w.account.OTKeys))+n > 100 {
		return nil, ErrInvalid
	}
	if err := w.account.GenOneTimeKeys(n); err != nil {
		return nil, engineError(err)
	}
	w.engine.observe(w.account)
	keys, err := w.account.OneTimeKeys()
	if err != nil {
		return nil, engineError(err)
	}
	return keys, nil
}
func (w *Working) GenerateFallbackKey() (map[string]id.Curve25519, error) {
	if err := w.account.GenFallbackKey(); err != nil {
		return nil, engineError(err)
	}
	w.engine.observe(w.account)
	return w.account.FallbackKeyUnpublished(), nil
}
func (w *Working) NewOutboundOlm(identity, otk id.Curve25519) (id.SessionID, error) {
	if len(w.olm) >= 1024 {
		return "", ErrCapacity
	}
	s, err := w.account.NewOutboundSession(identity, otk)
	if err != nil {
		return "", engineError(err)
	}
	concrete, ok := s.(*session.OlmSession)
	if !ok {
		return "", ErrCodec
	}
	w.engine.observe(concrete)
	w.olm[string(s.ID())] = concrete
	return s.ID(), nil
}

// DecryptPrekey must run only on a fork after the anchored sender/device/DM
// prefilter. The caller must discard that fork on ANY post-decrypt refusal.
// The source key is bound by library NewInboundSessionFrom, not an outer device ID.
func (w *Working) DecryptPrekey(sender id.Curve25519, ciphertext string) (id.SessionID, []byte, error) {
	if len(ciphertext) == 0 || len(ciphertext) > 1<<20 || len(w.olm) >= 1024 {
		return "", nil, ErrCapacity
	}
	s, err := w.account.NewInboundSessionFrom(&sender, ciphertext)
	if err != nil {
		return "", nil, engineError(err)
	}
	concrete, ok := s.(*session.OlmSession)
	if !ok {
		return "", nil, ErrCodec
	}
	raw, err := s.Decrypt(ciphertext, id.OlmMsgTypePreKey)
	w.engine.observe(concrete)
	if err != nil {
		return "", nil, engineError(err)
	}
	w.engine.secrets.Register(raw)
	if err = w.account.RemoveOneTimeKeys(s); err != nil {
		return "", nil, engineError(err)
	}
	w.engine.observe(w.account)
	w.olm[string(s.ID())] = concrete
	return s.ID(), raw, nil
}
func (w *Working) EncryptOlm(sid id.SessionID, plaintext []byte) (id.OlmMsgType, []byte, error) {
	s := w.olm[string(sid)]
	if s == nil {
		return 0, nil, ErrUnavailable
	}
	if len(plaintext) == 0 || len(plaintext) > 1<<20 {
		return 0, nil, ErrCapacity
	}
	w.engine.secrets.Register(plaintext)
	kind, raw, err := s.Encrypt(plaintext)
	w.engine.observe(s)
	if err != nil {
		return 0, nil, engineError(err)
	}
	return kind, raw, nil
}
func (w *Working) DecryptOlm(sid id.SessionID, kind id.OlmMsgType, ciphertext string) ([]byte, error) {
	s := w.olm[string(sid)]
	if s == nil {
		return nil, ErrUnavailable
	}
	if len(ciphertext) == 0 || len(ciphertext) > 1<<20 {
		return nil, ErrCapacity
	}
	if kind != id.OlmMsgTypeMsg && kind != id.OlmMsgTypePreKey {
		return nil, ErrInvalid
	}
	raw, err := s.Decrypt(ciphertext, kind)
	w.engine.observe(s)
	if err != nil {
		return nil, engineError(err)
	}
	w.engine.secrets.Register(raw)
	return raw, nil
}

// Megolm primitives carry no room policy. U3 owns authenticated distribution
// metadata, source/tombstone checks and exact room/content validation on the fork.
func (w *Working) NewOutboundMegolm() (id.SessionID, []byte, error) {
	if len(w.outbound) >= 32 {
		return "", nil, ErrCapacity
	}
	s, err := session.NewMegolmOutboundSession()
	if err != nil {
		return "", nil, engineError(err)
	}
	w.engine.observe(s)
	key, err := s.SessionSharingMessage()
	if err != nil {
		return "", nil, engineError(err)
	}
	w.engine.secrets.Register(key)
	w.outbound[string(s.ID())] = s
	return s.ID(), key, nil
}
func (w *Working) AcceptDirectMegolm(key []byte) (id.SessionID, error) {
	if len(key) == 0 || len(key) > 1024 {
		return "", ErrInvalid
	}
	if len(w.inbound) >= 128 {
		return "", ErrCapacity
	}
	w.engine.secrets.Register(key)
	s, err := session.NewMegolmInboundSession(key)
	if err != nil {
		return "", engineError(err)
	}
	if w.inbound[string(s.ID())] != nil {
		return "", ErrExists
	}
	w.engine.observe(s)
	w.inbound[string(s.ID())] = s
	return s.ID(), nil
}
func (w *Working) EncryptMegolm(sid id.SessionID, raw []byte) ([]byte, error) {
	s := w.outbound[string(sid)]
	if s == nil {
		return nil, ErrUnavailable
	}
	if len(raw) == 0 || len(raw) > 1<<20 {
		return nil, ErrCapacity
	}
	w.engine.secrets.Register(raw)
	out, err := s.Encrypt(raw)
	w.engine.observe(s)
	if err != nil {
		return nil, engineError(err)
	}
	return out, nil
}
func (w *Working) DecryptMegolm(sid id.SessionID, ciphertext []byte) ([]byte, uint, error) {
	s := w.inbound[string(sid)]
	if s == nil {
		return nil, 0, ErrUnavailable
	}
	if len(ciphertext) == 0 || len(ciphertext) > 1<<20 {
		return nil, 0, ErrCapacity
	}
	raw, index, err := s.Decrypt(ciphertext)
	w.engine.observe(s)
	if err != nil {
		return nil, 0, engineError(err)
	}
	w.engine.secrets.Register(raw)
	return raw, index, nil
}
func engineError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, olm.ErrBadMAC) || errors.Is(err, olm.ErrBadSignature) {
		return ErrIntegrity
	}
	if errors.Is(err, olm.ErrUnknownMessageIndex) {
		return ErrExpired
	}
	return ErrCodec
}
