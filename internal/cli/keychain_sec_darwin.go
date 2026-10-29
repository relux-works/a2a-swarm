//go:build darwin && cgo

package cli

/*
#cgo CFLAGS: -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <stdlib.h>
#include <Security/Security.h>

// Every operation opens the explicit keychain file and disallows user
// interaction first, so an untrusted ACL or a locked keychain returns an
// error instead of a prompt.
static OSStatus a2a_open(const char *path, SecKeychainRef *kc) {
	OSStatus st = SecKeychainSetUserInteractionAllowed(false);
	if (st != errSecSuccess) {
		return st;
	}
	return SecKeychainOpen(path, kc);
}

static OSStatus a2a_find(const char *path, const char *svc, UInt32 svcLen, const char *acct, UInt32 acctLen, void **data, UInt32 *len) {
	SecKeychainRef kc = NULL;
	OSStatus st = a2a_open(path, &kc);
	if (st != errSecSuccess) {
		return st;
	}
	st = SecKeychainFindGenericPassword(kc, svcLen, svc, acctLen, acct, len, data, NULL);
	CFRelease(kc);
	return st;
}

static OSStatus a2a_add(const char *path, const char *svc, UInt32 svcLen, const char *acct, UInt32 acctLen, const void *data, UInt32 len) {
	SecKeychainRef kc = NULL;
	OSStatus st = a2a_open(path, &kc);
	if (st != errSecSuccess) {
		return st;
	}
	// A NULL item reference and no explicit access: the item gets the default
	// ACL, which trusts only the creating application.
	st = SecKeychainAddGenericPassword(kc, svcLen, svc, acctLen, acct, len, data, NULL);
	CFRelease(kc);
	return st;
}

static OSStatus a2a_update(const char *path, const char *svc, UInt32 svcLen, const char *acct, UInt32 acctLen, const void *data, UInt32 len) {
	SecKeychainRef kc = NULL;
	OSStatus st = a2a_open(path, &kc);
	if (st != errSecSuccess) {
		return st;
	}
	SecKeychainItemRef item = NULL;
	st = SecKeychainFindGenericPassword(kc, svcLen, svc, acctLen, acct, NULL, NULL, &item);
	if (st == errSecItemNotFound) {
		st = SecKeychainAddGenericPassword(kc, svcLen, svc, acctLen, acct, len, data, NULL);
	} else if (st == errSecSuccess) {
		st = SecKeychainItemModifyAttributesAndData(item, NULL, len, data);
		CFRelease(item);
	}
	CFRelease(kc);
	return st;
}

static void a2a_free(void *data) {
	SecKeychainItemFreeContent(NULL, data);
}
*/
import "C"

import (
	"context"
	"fmt"
	"unsafe"
)

type secBackend struct{}

func defaultKeychainBackend() keychainBackend { return secBackend{} }

// secStatus maps Security.framework statuses to the store's closed set. An
// unknown status keeps only its number.
func secStatus(st C.OSStatus) error {
	switch int32(st) {
	case 0:
		return nil
	case -25300: // errSecItemNotFound
		return errKeychainMissing
	case -25299: // errSecDuplicateItem
		return errKeychainDuplicate
	case -25293, -25308, -25315, -128, -25243, -61:
		// errSecAuthFailed, errSecInteractionNotAllowed, errSecInteractionRequired,
		// errSecUserCanceled, errSecNoAccessForItem, errSecWrPerm.
		return errKeychainInteraction
	}
	return fmt.Errorf("keychain status %d", int32(st))
}

type cstrings struct{ path, svc, acct *C.char }

func newCStrings(keychain, service, account string) cstrings {
	return cstrings{C.CString(keychain), C.CString(service), C.CString(account)}
}

func (c cstrings) free() {
	C.free(unsafe.Pointer(c.path))
	C.free(unsafe.Pointer(c.svc))
	C.free(unsafe.Pointer(c.acct))
}

func (secBackend) find(_ context.Context, keychain, service, account string) ([]byte, error) {
	c := newCStrings(keychain, service, account)
	defer c.free()
	var data unsafe.Pointer
	var n C.UInt32
	st := C.a2a_find(c.path, c.svc, C.UInt32(len(service)), c.acct, C.UInt32(len(account)), &data, &n)
	if err := secStatus(st); err != nil {
		return nil, err
	}
	defer C.a2a_free(data)
	return C.GoBytes(data, C.int(n)), nil
}

func (secBackend) add(_ context.Context, keychain, service, account string, secret []byte) error {
	return secWrite(keychain, service, account, secret, false)
}

func (secBackend) update(_ context.Context, keychain, service, account string, secret []byte) error {
	return secWrite(keychain, service, account, secret, true)
}

func secWrite(keychain, service, account string, secret []byte, update bool) error {
	if len(secret) == 0 {
		return fmt.Errorf("refusing empty secret")
	}
	c := newCStrings(keychain, service, account)
	defer c.free()
	data := C.CBytes(secret)
	defer func() {
		// Clear the C copy of the secret before releasing it.
		b := unsafe.Slice((*byte)(data), len(secret))
		for i := range b {
			b[i] = 0
		}
		C.free(data)
	}()
	if update {
		return secStatus(C.a2a_update(c.path, c.svc, C.UInt32(len(service)), c.acct, C.UInt32(len(account)), data, C.UInt32(len(secret))))
	}
	return secStatus(C.a2a_add(c.path, c.svc, C.UInt32(len(service)), c.acct, C.UInt32(len(account)), data, C.UInt32(len(secret))))
}
