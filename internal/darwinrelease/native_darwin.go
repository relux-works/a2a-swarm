//go:build darwin && arm64 && cgo

package darwinrelease

/*
#cgo CFLAGS: -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>

static CFStringRef text(const char *p) { return CFStringCreateWithCString(NULL,p,kCFStringEncodingUTF8); }
static OSStatus compile_requirement(const char *expr, CFDataRef *data) {
 CFStringRef s=text(expr); SecRequirementRef r=NULL;
 OSStatus e=SecRequirementCreateWithString(s,kSecCSDefaultFlags,&r); CFRelease(s);
 if(!e) { e=SecRequirementCopyData(r,kSecCSDefaultFlags,data); CFRelease(r); } return e;
}
static OSStatus check_code(const char *path,const char *expr,CFDataRef *dr,CFStringRef *raw) {
 CFURLRef url=CFURLCreateFromFileSystemRepresentation(NULL,(const UInt8*)path,strlen(path),false);
 SecStaticCodeRef code=NULL; SecRequirementRef expected=NULL; CFDictionaryRef info=NULL;
 CFStringRef s=text(expr); OSStatus e=SecRequirementCreateWithString(s,kSecCSDefaultFlags,&expected);CFRelease(s);
 if(!e)e=SecStaticCodeCreateWithPath(url,kSecCSDefaultFlags,&code);CFRelease(url);
 if(!e)e=SecCodeCopySigningInformation((SecCodeRef)code,kSecCSSigningInformation|kSecCSRequirementInformation,&info);
 if(!e) {
  SecRequirementRef actual=(SecRequirementRef)CFDictionaryGetValue(info,kSecCodeInfoDesignatedRequirement);
  CFNumberRef flags=(CFNumberRef)CFDictionaryGetValue(info,kSecCodeInfoFlags);int bits=0;
  if(!actual||!flags||!CFNumberGetValue(flags,kCFNumberIntType,&bits))e=errSecCSUnsigned;
  else if(bits&2)e=-60001;
  else if(!(bits&0x10000))e=-60002;
  else if(!CFDictionaryGetValue(info,kSecCodeInfoTimestamp))e=-60003;
  if(!e)e=SecStaticCodeCheckValidity(code,kSecCSStrictValidate|kSecCSCheckAllArchitectures,expected);
  if(!e)e=SecRequirementCopyData(actual,kSecCSDefaultFlags,dr);
  if(!e)e=SecRequirementCopyString(actual,kSecCSDefaultFlags,raw);
 }
 if(info)CFRelease(info);if(code)CFRelease(code);if(expected)CFRelease(expected);return e;
}
static OSStatus import_signer(const char *path,const char *password,const void *p12,long size,const char *passphrase,void **handle,CFDataRef *certificate) {
 SecKeychainRef *keychain=(SecKeychainRef*)handle;
 OSStatus e=SecKeychainSetUserInteractionAllowed(false);if(e)return e;
 e=SecKeychainCreate(path,(UInt32)strlen(password),password,false,NULL,keychain);
 if(e)return e;
 SecTrustedApplicationRef app=NULL;SecAccessRef access=NULL;CFArrayRef trusted=NULL,items=NULL;
 CFDataRef data=CFDataCreate(NULL,p12,size);CFStringRef pass=text(passphrase);
 e=SecTrustedApplicationCreateFromPath("/usr/bin/codesign",&app);
 if(!e) { const void *apps[]={app};trusted=CFArrayCreate(NULL,apps,1,&kCFTypeArrayCallBacks);e=SecAccessCreate(CFSTR("a2a release signing"),trusted,&access); }
 if(!e) {
  const void *keys[]={kSecImportExportPassphrase,kSecImportExportKeychain,kSecImportExportAccess};
  const void *values[]={pass,*keychain,access};
  CFDictionaryRef options=CFDictionaryCreate(NULL,keys,values,3,&kCFTypeDictionaryKeyCallBacks,&kCFTypeDictionaryValueCallBacks);
  e=SecPKCS12Import(data,options,&items);CFRelease(options);
 }
 if(!e) {
  if(CFArrayGetCount(items)!=1)e=errSecParam;
  else {
   CFDictionaryRef item=(CFDictionaryRef)CFArrayGetValueAtIndex(items,0);
   SecIdentityRef identity=(SecIdentityRef)CFDictionaryGetValue(item,kSecImportItemIdentity);SecCertificateRef cert=NULL;
   if(!identity)e=errSecParam;else e=SecIdentityCopyCertificate(identity,&cert);
   if(!e){*certificate=SecCertificateCopyData(cert);if(!*certificate)e=errSecParam;CFRelease(cert);}
  }
 }
 if(items)CFRelease(items);if(access)CFRelease(access);if(trusted)CFRelease(trusted);if(app)CFRelease(app);CFRelease(pass);CFRelease(data);
 return e;
}
static OSStatus delete_signer(void *k) { if(!k)return 0; OSStatus e=SecKeychainDelete((SecKeychainRef)k);CFRelease(k);return e; }
static char *signer_path(void *k) { UInt32 n=4096;char *p=calloc(n,1);if(SecKeychainGetPath((SecKeychainRef)k,&n,p)){free(p);return NULL;}return p; }
// User-domain keychain search list, by ordered keychain paths. An empty list
// is set as an explicit empty array, never NULL.
static OSStatus copy_user_list(CFArrayRef *out) { *out=NULL; return SecKeychainCopyDomainSearchList(kSecPreferencesDomainUser,out); }
static CFIndex list_count(CFArrayRef a) { return a ? CFArrayGetCount(a) : 0; }
static char *list_path_at(CFArrayRef a, CFIndex i) { return signer_path((void*)CFArrayGetValueAtIndex(a,i)); }
static void release_list(CFArrayRef a) { if(a) CFRelease(a); }
static OSStatus set_user_list(char **paths, int n) {
 CFMutableArrayRef arr=CFArrayCreateMutable(NULL,n,&kCFTypeArrayCallBacks); if(!arr) return errSecAllocate;
 for(int i=0;i<n;i++){ SecKeychainRef k=NULL; OSStatus e=SecKeychainOpen(paths[i],&k); if(e){CFRelease(arr);return e;} CFArrayAppendValue(arr,k); CFRelease(k); }
 OSStatus e=SecKeychainSetDomainSearchList(kSecPreferencesDomainUser,arr); CFRelease(arr); return e;
}
static char *copy_text(CFStringRef s) {
 CFIndex n=CFStringGetMaximumSizeForEncoding(CFStringGetLength(s),kCFStringEncodingUTF8)+1;
 char *b=malloc(n);if(!CFStringGetCString(s,b,n,kCFStringEncodingUTF8)){free(b);return NULL;}return b;
}
*/
import "C"

import (
	"crypto/rand"
	"crypto/sha1" // codesign's certificate selector, never an artifact digest
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"unsafe"
)

func CompileRequirement(expr string) ([]byte, error) {
	s := C.CString(expr)
	defer C.free(unsafe.Pointer(s))
	var data C.CFDataRef
	if C.compile_requirement(s, &data) != 0 {
		return nil, errors.New("requirement-compile")
	}
	defer C.CFRelease(C.CFTypeRef(data))
	return C.GoBytes(unsafe.Pointer(C.CFDataGetBytePtr(data)), C.int(C.CFDataGetLength(data))), nil
}
func nativeVerify(path, expr string) (string, []byte, error) {
	p, s := C.CString(path), C.CString(expr)
	defer C.free(unsafe.Pointer(p))
	defer C.free(unsafe.Pointer(s))
	var data C.CFDataRef
	var raw C.CFStringRef
	status := C.check_code(p, s, &data, &raw)
	if status != 0 {
		if data != 0 {
			C.CFRelease(C.CFTypeRef(data))
		}
		if raw != 0 {
			C.CFRelease(C.CFTypeRef(raw))
		}
		switch int32(status) {
		case -60001:
			return "", nil, errors.New("ad-hoc-signature")
		case -60002:
			return "", nil, errors.New("hardened-runtime-missing")
		case -60003:
			return "", nil, errors.New("timestamp-missing")
		}
		return "", nil, errors.New("native-signature-requirement")
	}
	defer C.CFRelease(C.CFTypeRef(data))
	defer C.CFRelease(C.CFTypeRef(raw))
	compiled := C.GoBytes(unsafe.Pointer(C.CFDataGetBytePtr(data)), C.int(C.CFDataGetLength(data)))
	expected, e := CompileRequirement(expr)
	if e != nil || matchCompiledRequirement(expected, compiled) != nil {
		return "", nil, errors.New("full-designated-requirement-mismatch")
	}
	text := C.copy_text(raw)
	if text == nil {
		return "", nil, errors.New("requirement-extract")
	}
	defer C.free(unsafe.Pointer(text))
	return C.GoString(text), compiled, nil
}

type signingKeychain struct {
	keychain       unsafe.Pointer
	path, selector string
}

func openSigningKeychain(path string, values map[string]string) (*signingKeychain, error) {
	random := make([]byte, 32)
	if _, e := rand.Read(random); e != nil {
		return nil, errors.New("keychain-random")
	}
	return openSigningKeychainWithPassword(path, values, hex.EncodeToString(random))
}

// openSigningKeychainWithPassword is openSigningKeychain with the keychain
// password supplied by the caller. Release code always passes a fresh random
// value; the native signing-keychain control passes a fixture value so that it
// can compare keychain access settings on the same import path.
func openSigningKeychainWithPassword(path string, values map[string]string, password string) (*signingKeychain, error) {
	p12, e := base64.StdEncoding.DecodeString(values["MACOS_SIGN_P12"])
	if e != nil || len(p12) == 0 {
		return nil, errors.New("p12-decode")
	}
	p, pw, pass := C.CString(path), C.CString(password), C.CString(values["MACOS_SIGN_P12_PASSWORD"])
	defer C.free(unsafe.Pointer(p))
	defer C.free(unsafe.Pointer(pw))
	defer C.free(unsafe.Pointer(pass))
	buf := C.CBytes(p12)
	defer C.free(buf)
	for i := range p12 {
		p12[i] = 0
	}
	k := &signingKeychain{path: path}
	var cert C.CFDataRef
	if C.import_signer(p, pw, buf, C.long(len(p12)), pass, &k.keychain, &cert) != 0 {
		if cleanup := k.close(); cleanup != nil {
			return nil, cleanup
		}
		return nil, errors.New("signing-keychain-import")
	}
	defer C.CFRelease(C.CFTypeRef(cert))
	actual := C.signer_path(k.keychain)
	if actual == nil {
		_ = k.close()
		return nil, errors.New("signing-keychain-path")
	}
	defer C.free(unsafe.Pointer(actual))
	k.path = C.GoString(actual)
	if filepath.Dir(k.path) != filepath.Dir(path) {
		_ = k.close()
		return nil, errors.New("signing-keychain-path")
	}
	b := C.GoBytes(unsafe.Pointer(C.CFDataGetBytePtr(cert)), C.int(C.CFDataGetLength(cert)))
	hash := sha1.Sum(b)
	k.selector = hex.EncodeToString(hash[:])
	return k, nil
}
func (k *signingKeychain) close() error {
	if k == nil || k.keychain == nil {
		return nil
	}
	status := C.delete_signer(k.keychain)
	k.keychain = nil
	if status != 0 {
		return errors.New("signing-keychain-cleanup")
	}
	if _, e := os.Lstat(k.path); !os.IsNotExist(e) {
		return errors.New("signing-keychain-remains")
	}
	return nil
}

// nativeSearchList reads and replaces the USER keychain search list through
// SecKeychainCopyDomainSearchList and SecKeychainSetDomainSearchList. Errors
// carry only a fixed token and the OSStatus number.
type nativeSearchList struct{}

func releaseSearchList() searchListOps { return nativeSearchList{} }

func (nativeSearchList) Get() ([]string, error) {
	var arr C.CFArrayRef
	if status := C.copy_user_list(&arr); status != 0 {
		return nil, errors.New("copy-failed:" + strconv.Itoa(int(status)))
	}
	defer C.release_list(arr)
	n := int(C.list_count(arr))
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		p := C.list_path_at(arr, C.CFIndex(i))
		if p == nil {
			return nil, errors.New("path-failed")
		}
		out = append(out, C.GoString(p))
		C.free(unsafe.Pointer(p))
	}
	return out, nil
}

func (nativeSearchList) Set(paths []string) error {
	cs := C.malloc(C.size_t(len(paths)+1) * C.size_t(unsafe.Sizeof(uintptr(0))))
	defer C.free(cs)
	list := unsafe.Slice((**C.char)(cs), len(paths)+1)
	for i, p := range paths {
		list[i] = C.CString(p)
	}
	defer func() {
		for i := range paths {
			C.free(unsafe.Pointer(list[i]))
		}
	}()
	if status := C.set_user_list((**C.char)(cs), C.int(len(paths))); status != 0 {
		return errors.New("set-failed:" + strconv.Itoa(int(status)))
	}
	return nil
}
