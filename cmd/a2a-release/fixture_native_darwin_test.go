//go:build darwin && arm64 && cgo

package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"debug/macho"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/fixturecompare"
)

func fixtureTool(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if b, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v: %s", args[0], err, b)
	}
}

func commandFixturePair(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "main.c")
	if err := os.WriteFile(src, []byte("int main(void) { return 0; }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var pins strings.Builder
	for _, name := range []string{"a2a", "a2a-keychain-status"} {
		path := filepath.Join(dir, name)
		fixtureTool(t, "/usr/bin/xcrun", "clang", "-target", "arm64-apple-macos13.0", src, "-o", path)
		fixtureTool(t, "/usr/bin/codesign", "--force", "--sign", "-", "--identifier", "fixture."+name, "--options", "runtime", path)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&pins, "%x  %s\n", sha256.Sum256(b), name)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(pins.String()), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Actual CLI diagnostics exit 2 with explicit findings, never a release pass.
// This uses self-created ad-hoc images only; no identity or Keychain item access.
func TestNativeFixtureCommandExit(t *testing.T) {
	dir := commandFixturePair(t)
	out, code := releaseProcess(t, nil, "fixture-compare", dir, dir, filepath.Join(t.TempDir(), "evidence"))
	if code != 2 || !strings.Contains(out, fixturecompare.Label) || !strings.Contains(out, "timestamp-missing") ||
		!strings.Contains(out, "ad-hoc-companion") || !strings.Contains(out, `"release_verified":false`) || !strings.Contains(out, `"completed":true`) {
		t.Fatalf("diagnostic misclassified: code=%d %s", code, out)
	}
}

// Attach a CMS signed by a disposable on-disk self-signed certificate, without
// Keychain access. This preserves parseable non-ad-hoc signing metadata without
// a secure timestamp. It witnesses the early timestamp gate, not release trust.
func timestampMetadata(t *testing.T, path string) {
	t.Helper()
	f, err := macho.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var signatureOffset, signatureSize, commandOffset int
	loadOffset := 32 // fixture is a 64-bit Mach-O
	for _, load := range f.Loads {
		raw := load.Raw()
		if len(raw) < 16 || f.ByteOrder.Uint32(raw[:4]) != 0x1d {
			loadOffset += len(raw)
			continue
		} // LC_CODE_SIGNATURE
		signatureOffset = int(f.ByteOrder.Uint32(raw[8:12]))
		signatureSize = int(f.ByteOrder.Uint32(raw[12:16]))
		commandOffset = loadOffset
		break
	}
	if signatureOffset <= 0 || signatureSize < 12 || signatureOffset+signatureSize != len(b) {
		t.Fatal("invalid fixture signature bounds")
	}
	blob := b[signatureOffset:]
	if binary.BigEndian.Uint32(blob[:4]) != 0xfade0cc0 {
		t.Fatal("fixture signature superblob missing")
	}
	count := int(binary.BigEndian.Uint32(blob[8:12]))
	if count < 1 || 12+count*8 > len(blob) {
		t.Fatal("invalid signature index")
	}
	type slot struct {
		kind uint32
		data []byte
	}
	var slots []slot
	var cd []byte
	for i := range count {
		pos := 12 + i*8
		kind := binary.BigEndian.Uint32(blob[pos : pos+4])
		start := int(binary.BigEndian.Uint32(blob[pos+4 : pos+8]))
		if start < 0 || start+8 > len(blob) {
			t.Fatal("invalid blob bounds")
		}
		length := int(binary.BigEndian.Uint32(blob[start+4 : start+8]))
		if length < 8 || start+length > len(blob) {
			t.Fatal("invalid blob size")
		}
		data := append([]byte{}, blob[start:start+length]...)
		if kind == 0 {
			if len(data) < 16 || binary.BigEndian.Uint32(data[:4]) != 0xfade0c02 {
				t.Fatal("CodeDirectory missing")
			}
			flags := binary.BigEndian.Uint32(data[12:16])
			binary.BigEndian.PutUint32(data[12:16], (flags&^uint32(2))|0x10000)
			cd = data
		}
		if kind != 0x10000 {
			slots = append(slots, slot{kind, data})
		}
	}
	if cd == nil {
		t.Fatal("CodeDirectory missing")
	}
	cms := fixtureCMS(t, cd)
	wrapper := make([]byte, 8)
	binary.BigEndian.PutUint32(wrapper[:4], 0xfade0b01)
	binary.BigEndian.PutUint32(wrapper[4:8], uint32(8+len(cms)))
	slots = append(slots, slot{0x10000, append(wrapper, cms...)})
	encoded := make([]byte, 12+8*len(slots))
	binary.BigEndian.PutUint32(encoded[:4], 0xfade0cc0)
	binary.BigEndian.PutUint32(encoded[8:12], uint32(len(slots)))
	for i, s := range slots {
		binary.BigEndian.PutUint32(encoded[12+i*8:16+i*8], s.kind)
		binary.BigEndian.PutUint32(encoded[16+i*8:20+i*8], uint32(len(encoded)))
		encoded = append(encoded, s.data...)
	}
	binary.BigEndian.PutUint32(encoded[4:8], uint32(len(encoded)))
	f.ByteOrder.PutUint32(b[commandOffset+12:commandOffset+16], uint32(len(encoded)))
	// Page hashes deliberately remain stale after header changes: this witness
	// must never qualify as a strict valid image or a release signature.
	if err := os.WriteFile(path, append(b[:signatureOffset], encoded...), 0755); err != nil {
		t.Fatal(err)
	}
}

func fixtureCMS(t *testing.T, codeDirectory []byte) []byte {
	t.Helper()
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "disposable fixture"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"cd":              codeDirectory,
		"certificate.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		"private.pem":     pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	fixtureTool(t, "/usr/bin/openssl", "smime", "-sign", "-binary", "-in", filepath.Join(dir, "cd"),
		"-signer", filepath.Join(dir, "certificate.pem"), "-inkey", filepath.Join(dir, "private.pem"),
		"-outform", "DER", "-out", filepath.Join(dir, "cms"), "-nosmimecap", "-md", "sha256")
	data, err := os.ReadFile(filepath.Join(dir, "cms"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Both unmodified legacy commands still exit 1 specifically on timestamp-missing.
// Paired native fixture-mode success above does not weaken this release gate.
// The malformed metadata witness proves ordering only, not a valid CMS signer.
func TestLegacyTimestampGate(t *testing.T) {
	dir := commandFixturePair(t)
	timestampMetadata(t, filepath.Join(dir, "a2a"))
	for _, command := range []string{"verify", "compare"} {
		t.Run(command, func(t *testing.T) {
			args := []string{command, dir}
			if command == "compare" {
				args = append(args, dir)
			}
			args = append(args, filepath.Join(t.TempDir(), "evidence"))
			out, code := releaseProcess(t, nil, args...)
			if code != 1 || !strings.Contains(out, "timestamp-missing") || strings.Contains(out, fixturecompare.Label) {
				t.Fatalf("legacy timestamp gate changed: code=%d %s", code, out)
			}
		})
	}
}
