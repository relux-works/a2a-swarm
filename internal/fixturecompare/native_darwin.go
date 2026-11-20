//go:build darwin && arm64 && cgo

package fixturecompare

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>

// Deliberately separate from the release verifier: inspect actual fixture
// requirements, retaining missing release predicates as diagnostic findings.
static OSStatus fixture_signature(const char *path, CFDataRef *dr, CFStringRef *raw, int *flags, int *timestamp) {
 CFURLRef url=CFURLCreateFromFileSystemRepresentation(NULL,(const UInt8*)path,strlen(path),false);
 SecStaticCodeRef code=NULL; CFDictionaryRef info=NULL;
 OSStatus e=SecStaticCodeCreateWithPath(url,kSecCSDefaultFlags,&code);CFRelease(url);
 if(!e)e=SecStaticCodeCheckValidity(code,kSecCSStrictValidate|kSecCSCheckAllArchitectures,NULL);
 if(!e)e=SecCodeCopySigningInformation((SecCodeRef)code,kSecCSSigningInformation|kSecCSRequirementInformation,&info);
 if(!e) {
  SecRequirementRef actual=(SecRequirementRef)CFDictionaryGetValue(info,kSecCodeInfoDesignatedRequirement);
  CFNumberRef bits=(CFNumberRef)CFDictionaryGetValue(info,kSecCodeInfoFlags);
  if(!actual||!bits||CFGetTypeID(bits)!=CFNumberGetTypeID()||!CFNumberGetValue(bits,kCFNumberIntType,flags))e=errSecCSUnsigned;
  // Require the strictly verified image to satisfy its complete actual DR too.
  if(!e)e=SecStaticCodeCheckValidity(code,kSecCSStrictValidate|kSecCSCheckAllArchitectures,actual);
  if(!e)e=SecRequirementCopyData(actual,kSecCSDefaultFlags,dr);
  if(!e)e=SecRequirementCopyString(actual,kSecCSDefaultFlags,raw);
  *timestamp=CFDictionaryGetValue(info,kSecCodeInfoTimestamp)!=NULL;
 }
 if(info)CFRelease(info);if(code)CFRelease(code);return e;
}
static char *fixture_text(CFStringRef s) {
 CFIndex n=CFStringGetMaximumSizeForEncoding(CFStringGetLength(s),kCFStringEncodingUTF8)+1;
 char *p=malloc(n);if(!p)return NULL;
 if(!CFStringGetCString(s,p,n,kCFStringEncodingUTF8)){free(p);return NULL;}return p;
}
*/
import "C"

import (
	"errors"
	"strings"
	"unsafe"
)

func inspect(path string) (signature, error) {
	var sig signature
	if strings.ContainsRune(path, 0) {
		return sig, errors.New("fixture-path")
	}
	if err := inspectExecutable(path); err != nil {
		return sig, err
	}
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))
	var data C.CFDataRef
	var raw C.CFStringRef
	var flags, timestamp C.int
	status := C.fixture_signature(p, &data, &raw, &flags, &timestamp)
	if data != 0 {
		defer C.CFRelease(C.CFTypeRef(data))
	}
	if raw != 0 {
		defer C.CFRelease(C.CFTypeRef(raw))
	}
	if status != 0 || data == 0 || raw == 0 {
		return sig, errors.New("fixture-strict-signature")
	}
	text := C.fixture_text(raw)
	if text == nil {
		return sig, errors.New("fixture-requirement-extract")
	}
	defer C.free(unsafe.Pointer(text))
	sig.raw = C.GoString(text)
	sig.compiled = C.GoBytes(unsafe.Pointer(C.CFDataGetBytePtr(data)), C.int(C.CFDataGetLength(data)))
	sig.adHoc = flags&2 != 0
	sig.runtime = flags&0x10000 != 0
	sig.timestamp = timestamp != 0
	return sig, nil
}
