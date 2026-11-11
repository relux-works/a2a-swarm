//go:build darwin && cgo

// untrusted-reader is the phase-2 sentinel's separately built reader (#138).
// Built on its own, it carries a different code identity from the a2a test
// binary that creates the sentinel item. It asks for an item's data with user
// interaction disallowed and prints exactly one line, "allowed", "missing",
// "refused <status>" or "error <status>": never the data, which it frees
// unread.
package main

/*
#cgo CFLAGS: -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <stdlib.h>
#include <Security/Security.h>

static OSStatus probe(const char *path, const char *svc, UInt32 svcLen, const char *acct, UInt32 acctLen) {
	OSStatus st = SecKeychainSetUserInteractionAllowed(false);
	if (st != errSecSuccess) {
		return st;
	}
	SecKeychainRef kc = NULL;
	st = SecKeychainOpen(path, &kc);
	if (st != errSecSuccess) {
		return st;
	}
	void *data = NULL;
	UInt32 len = 0;
	st = SecKeychainFindGenericPassword(kc, svcLen, svc, acctLen, acct, &len, &data, NULL);
	if (st == errSecSuccess) {
		SecKeychainItemFreeContent(NULL, data);
	}
	CFRelease(kc);
	return st;
}
*/
import "C"

import (
	"fmt"
	"math"
	"os"
	"unsafe"
)

func main() {
	if len(os.Args) != 4 || len(os.Args[2]) > math.MaxUint32 || len(os.Args[3]) > math.MaxUint32 {
		fmt.Fprintln(os.Stderr, "usage: untrusted-reader KEYCHAIN SERVICE ACCOUNT")
		os.Exit(2)
	}
	path, svc, acct := C.CString(os.Args[1]), C.CString(os.Args[2]), C.CString(os.Args[3])
	defer C.free(unsafe.Pointer(path))
	defer C.free(unsafe.Pointer(svc))
	defer C.free(unsafe.Pointer(acct))
	st := int32(C.probe(path, svc, C.UInt32(len(os.Args[2])), acct, C.UInt32(len(os.Args[3]))))
	switch st {
	case 0:
		fmt.Println("allowed")
	case -25300:
		fmt.Println("missing")
	case -25293, -25308, -25315, -128, -25243, -61:
		fmt.Printf("refused %d\n", st)
	default:
		fmt.Printf("error %d\n", st)
	}
}
