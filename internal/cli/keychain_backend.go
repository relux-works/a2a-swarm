package cli

import (
	"context"
	"errors"
	"math"
)

// errKeychainInteraction means the Keychain refused an operation because it
// would need user interaction: an item ACL that does not trust this signed
// binary, a locked keychain or a canceled prompt. Reads and writes never show
// UI, so this is the expected result for every untrusted caller.
var errKeychainInteraction = errors.New("keychain interaction not allowed")

// errHumanKeychain refuses every Keychain operation for a human identity
// (owner decision 74). An item bound to the a2a binary's designated
// requirement is readable by every agent that runs a2a, so a human token is
// never kept in one; an owner-only tool holds it instead.
var errHumanKeychain = errors.New("human tokens are never kept in a2a Keychain items; an owner-only tool holds them (owner decision 74)")

// keychainBackend performs one generic-password operation in this process on
// an explicit keychain file, with user interaction disallowed (phase 2,
// a2a-swarm#138). It never runs a helper process such as /usr/bin/security,
// and its errors never carry secret material. A new item's ACL is the
// Keychain default for the creating binary: only its designated requirement
// may read the item without a prompt.
type keychainBackend interface {
	find(ctx context.Context, keychain, service, account string) ([]byte, error)
	add(ctx context.Context, keychain, service, account string, secret []byte) error
	// update replaces an existing item's data, keeping its ACL, and adds the
	// item when it no longer exists.
	update(ctx context.Context, keychain, service, account string, secret []byte) error
}

// errKeychainLength refuses a length the native API cannot carry exactly.
var errKeychainLength = errors.New("keychain length out of range")

// nativeInputLen reports whether n fits the UInt32 length the Security API
// takes, so a conversion never truncates a service, account or secret.
func nativeInputLen(n int) bool { return n >= 0 && uint64(n) <= math.MaxUint32 }

// nativeOutputLen reports whether a returned UInt32 length fits the C int that
// C.GoBytes takes; a larger value would become negative and panic.
func nativeOutputLen(n uint64) bool { return n <= math.MaxInt32 }

func (k *keychainStore) keychainBackend() keychainBackend {
	if k.backend != nil {
		return k.backend
	}
	return defaultKeychainBackend()
}
