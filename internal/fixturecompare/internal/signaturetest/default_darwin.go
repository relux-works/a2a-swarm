//go:build darwin && arm64 && cgo

// Package signaturetest exposes the native default-validation control to tests.
// Production inspection keeps its independent, fixed strict-validation flags.
package signaturetest

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
*/
import "C"

import (
	"strings"
	"unsafe"
)

// DefaultStatus invokes the same platform API as the comparator, with exactly
// kSecCSDefaultFlags. The codesign CLI can impose its own structural checks.
func DefaultStatus(path string) int32 {
	if strings.ContainsRune(path, 0) {
		return int32(C.errSecParam)
	}
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))
	url := C.CFURLCreateFromFileSystemRepresentation(0, (*C.UInt8)(unsafe.Pointer(p)), C.CFIndex(len(path)), 0)
	if url == 0 {
		return int32(C.errSecParam)
	}
	defer C.CFRelease(C.CFTypeRef(url))
	var code C.SecStaticCodeRef
	status := C.SecStaticCodeCreateWithPath(url, C.kSecCSDefaultFlags, &code)
	if code != 0 {
		defer C.CFRelease(C.CFTypeRef(code))
	}
	if status != 0 {
		return int32(status)
	}
	return int32(C.SecStaticCodeCheckValidity(code, C.kSecCSDefaultFlags, 0))
}
