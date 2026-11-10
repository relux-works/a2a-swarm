//go:build darwin || linux

package e2ee

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func testRoot(t testing.TB) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}
func fixture(t testing.TB) (*FileStore, *LockedStore, *FileCustody, Pointer, *Inventory) {
	t.Helper()
	s, err := OpenFileStore(filepath.Join(testRoot(t), "crypto"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	l, err := s.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !l.closed {
			l.Close()
		}
	})
	n := Namespace{"https://example.test", "example.test", "legacy", "file", LegacyTokenService, PasswordService, "agents", "@agent:example.test"}
	inv := new(Inventory)
	c, err := NewFileCustody(l, n, []string{LegacyTokenService, PasswordService}, inv)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewPointer(n, strings.Repeat("a", 64), "DEVICE")
	if err != nil {
		t.Fatal(err)
	}
	return s, l, c, p, inv
}
func mustPointerRoot(t testing.TB, c *FileCustody, p Pointer, inv *Inventory) Root {
	t.Helper()
	if err := c.CreatePointer(p); err != nil {
		t.Fatal(err)
	}
	r, err := NewRoot(p, inv)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.CreateRoot(p, r); err != nil {
		t.Fatal(err)
	}
	return r
}
func codecFor(t testing.TB, r Root, inv *Inventory) *Codec {
	t.Helper()
	key, err := r.WrappingKey(inv)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCodec(key, inv)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Reopen independent directory/lock handles to the fixture inode. This models
// coordinator restart, not a physical power cut or loss of the OS write cache.
func reopenLease(t testing.TB, s *FileStore, l *LockedStore) *LockedStore {
	t.Helper()
	fd, err := unix.Openat(int(s.dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Close(); err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}
	if err = s.dir.Close(); err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}
	s.dir = os.NewFile(uintptr(fd), "e2ee-test-reopened")
	next, err := s.Lock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { next.Close() })
	return next
}

// A durable pointer locates exactly the same create-only root after reopening;
// absent, duplicate, changed-binding and missing-locator reads never invent roots.
func TestCustodyRestartAndRefusals(t *testing.T) {
	s, l, c, p, inv := fixture(t)
	if _, err := c.ReadRoot(p); err != ErrUnavailable {
		t.Fatalf("absent: %v", err)
	}
	r := mustPointerRoot(t, c, p, inv)
	before, err := l.read("secrets.json", MaxItem)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.CreateRoot(p, r); err != ErrExists {
		t.Fatalf("duplicate: %v", err)
	}
	after, _ := l.read("secrets.json", MaxItem)
	if !bytes.Equal(before, after) {
		t.Fatal("duplicate changed root")
	}
	changed := p
	changed.Binding.Device = "OTHER"
	if _, err = c.ReadRoot(changed); err != ErrIntegrity {
		t.Fatalf("binding: %v", err)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := s.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	c.lease = second
	actual, err := c.ReadPointer(p.Binding)
	if err != nil || actual != p {
		t.Fatal("locator changed", err)
	}
	got, err := c.ReadRoot(actual)
	if err != nil || got != r {
		t.Fatal("root changed", err)
	}
	if err = unix.Unlinkat(int(s.dir.Fd()), "generation.json", 0); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ReadRoot(p); err != ErrUnavailable {
		t.Fatalf("missing pointer: %v", err)
	}
	if err = c.CreatePointer(p); err != ErrConflict {
		t.Fatalf("orphaned roots must refuse: %v", err)
	}
}

// Every create-only publication boundary leaves either no item or the exact
// same item. Restart reuses root.G after pointer-only and root-before-state loss.
func TestPointerAndRootInterruptions(t *testing.T) {
	steps := []string{"write", "file-sync", "publish", "dir-sync", "unlink", "cleanup-sync"}
	for _, item := range []string{"generation.json", "secrets.json"} {
		for _, step := range steps {
			t.Run(item+"/"+step, func(t *testing.T) {
				s, _, c, p, inv := fixture(t)
				r, err := NewRoot(p, inv)
				if err != nil {
					t.Fatal(err)
				}
				if item == "secrets.json" {
					if err = c.CreatePointer(p); err != nil {
						t.Fatal(err)
					}
				}
				planted := false
				s.fault = func(label string) error {
					if label == item+":"+step {
						planted = true
						return syscall.EIO
					}
					return nil
				}
				if item == "generation.json" {
					err = c.CreatePointer(p)
				} else {
					err = c.CreateRoot(p, r)
				}
				if !planted || err != ErrIO {
					t.Fatalf("control not reached: %v", err)
				}
				s.fault = nil
				c.lease = reopenLease(t, s, c.lease)
				if item == "generation.json" {
					got, readErr := c.ReadPointer(p.Binding)
					if step == "write" || step == "file-sync" || step == "publish" {
						if readErr != ErrUnavailable {
							t.Fatalf("published before boundary: %v", readErr)
						}
						if err = c.CreatePointer(p); err != nil {
							t.Fatal(err)
						}
					} else if readErr != nil || got != p {
						t.Fatal("durable locator changed", readErr)
					}
					if err = c.CreateRoot(p, r); err != nil {
						t.Fatal(err)
					}
				} else {
					got, readErr := c.ReadRoot(p)
					if step == "write" || step == "file-sync" || step == "publish" {
						if readErr != ErrUnavailable {
							t.Fatalf("published before boundary: %v", readErr)
						}
						if err = c.CreateRoot(p, r); err != nil {
							t.Fatal(err)
						}
					} else if readErr != nil || got != r {
						t.Fatal("root readback changed", readErr)
					}
				}
				got, err := c.ReadPointer(p.Binding)
				if err != nil || got.Account() != p.Account() {
					t.Fatal("recovery chose a new generation", err)
				}
				root, err := c.ReadRoot(got)
				if err != nil || root != r {
					t.Fatal("recovery lost original root", err)
				}
			})
		}
	}
}

// Valid 0700/0600 objects are usable; unsafe directories/files and symlinks at
// both intermediate and leaf components refuse without mode repair or writes.
func TestSafeOpenModesAndSymlinks(t *testing.T) {
	root := testRoot(t)
	dir := filepath.Join(root, "crypto")
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, mode := range []os.FileMode{0755, 0770, 0700 | os.ModeSticky} {
		if err = os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		if _, err = OpenFileStore(dir); err != ErrUnsafe {
			t.Fatalf("directory mode %o: %v", mode, err)
		}
		st, _ := os.Stat(dir)
		if st.Mode().Perm() != mode.Perm() {
			t.Fatal("repaired mode")
		}
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "alias")
	if err = os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenFileStore(link); err != ErrUnsafe {
		t.Fatalf("directory symlink: %v", err)
	}
	if _, err = OpenFileStore(filepath.Join(link, "nested")); err != ErrUnsafe {
		t.Fatalf("ancestor symlink: %v", err)
	}
	l, err := s.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	target := filepath.Join(root, "sentinel")
	if err = os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(target, filepath.Join(dir, "secrets.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = l.read("secrets.json", 100); err != ErrUnsafe {
		t.Fatalf("leaf read: %v", err)
	}
	if err = l.publish("secrets.json", []byte("replace"), false); err != ErrUnsafe {
		t.Fatalf("leaf write: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "unchanged" {
		t.Fatal("modified symlink target")
	}
	if err = os.WriteFile(filepath.Join(dir, "unsafe"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = l.read("unsafe", 100); err != ErrUnsafe {
		t.Fatalf("unsafe item: %v", err)
	}
	if err = l.publish("unsafe", []byte("new"), false); err != ErrUnsafe {
		t.Fatal("repaired unsafe file")
	}
	// Wrong-UID predicate is exercised without requiring chown/root on this bench.
	f, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) != nil {
		t.Fatal("fstat")
	}
	if validateStat(&st, os.Geteuid(), false) != nil {
		t.Fatal("valid owner rejected")
	}
	if validateStat(&st, os.Geteuid()+1, false) != ErrUnsafe {
		t.Fatal("wrong owner admitted")
	}
}

// An explicit 0700 root works; 0750/0755/0777 and foreign ownership refuse with
// no repair or item changes. Foreign UID is planted at the shared Fstat owner
// validation seam: this unprivileged test does not chown a real directory.
func TestCustodyRootBoundary(t *testing.T) {
	root := filepath.Join(testRoot(t), "custody")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := OpenFileStore(root)
	if err != nil {
		t.Fatal("0700 root", err)
	}
	lease, err := store.Lock(t.Context())
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err = lease.publish("sentinel", []byte("unchanged"), true); err != nil {
		lease.Close()
		store.Close()
		t.Fatal(err)
	}
	var st unix.Stat_t
	if unix.Fstat(int(store.dir.Fd()), &st) != nil {
		t.Fatal("root fstat")
	}
	if err = validateStat(&st, os.Geteuid(), true); err != nil {
		t.Fatal("effective owner", err)
	}
	foreign := st
	foreign.Uid ^= 1
	if err = validateStat(&foreign, os.Geteuid(), true); err != ErrUnsafe {
		t.Fatal("foreign root owner rejection", err)
	}
	lease.Close()
	store.Close()
	for _, mode := range []os.FileMode{0750, 0755, 0777} {
		if err = os.Chmod(root, mode); err != nil {
			t.Fatal(err)
		}
		if _, err = OpenFileStore(root); err != ErrUnsafe {
			t.Fatalf("root mode %o rejection: %v", mode, err)
		}
		stat, err := os.Stat(root)
		if err != nil || stat.Mode().Perm() != mode {
			t.Fatal("root mode repaired", err)
		}
		data, err := os.ReadFile(filepath.Join(root, "sentinel"))
		if err != nil || string(data) != "unchanged" {
			t.Fatal("root refusal modified item", err)
		}
	}
	for _, invalid := range []string{"", ".", "~/custody", "$HOME/custody"} {
		if _, err = OpenFileStore(invalid); err != ErrUnsafe {
			t.Fatal("implicit/relative root accepted", err)
		}
	}
	if err = os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	store, err = OpenFileStore(root)
	if err != nil {
		t.Fatal("0700 root retry", err)
	}
	defer store.Close()
	lease, err = store.Lock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	data, err := lease.read("sentinel", 100)
	if err != nil || string(data) != "unchanged" {
		t.Fatal("0700 positive readback", err)
	}
}

// The codec roundtrip is the positive control for wrong key/AAD, modified tag,
// unknown version, truncation and oversized headers. The planted tag violation
// must fail specifically with integrity, not an unrelated setup error.
func TestSealedCodecIntegrity(t *testing.T) {
	_, l, c, p, inv := fixture(t)
	r := mustPointerRoot(t, c, p, inv)
	codec := codecFor(t, r, inv)
	raw := []byte(`{"account":"planted-account-secret"}`)
	if err := l.WriteSealed(codec, p.Binding, raw, true); err != nil {
		t.Fatal(err)
	}
	got, err := l.ReadSealed(codec, p.Binding)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("roundtrip", err)
	}
	encoded, err := l.read("state.bin", MaxSnapshot+envelopeOverhead)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := NewCodec(bytes.Repeat([]byte{7}, 32), inv)
	if _, err = other.Unseal(p.Binding, encoded); err != ErrIntegrity {
		t.Fatalf("wrong key: %v", err)
	}
	bind := p.Binding
	bind.Identity = "@other:example.test"
	if _, err = codec.Unseal(bind, encoded); err != ErrIntegrity {
		t.Fatalf("wrong binding: %v", err)
	}
	damaged := append([]byte(nil), encoded...)
	damaged[len(damaged)-1] ^= 1
	if _, err = codec.Unseal(p.Binding, damaged); err != ErrIntegrity {
		t.Fatalf("known violation missed: %v", err)
	}
	for _, bad := range [][]byte{encoded[:10], append([]byte("UNKNOWN!"), encoded[8:]...), append(append([]byte(nil), encoded...), 0)} {
		if _, err = codec.Unseal(p.Binding, bad); err != ErrCodec {
			t.Fatalf("bad codec: %v", err)
		}
	}
	if _, err = codec.Seal(p.Binding, make([]byte, MaxSnapshot+1)); err != ErrCapacity {
		t.Fatalf("oversize: %v", err)
	}
}

// ENOSPC and every replacement fsync/publish interruption preserve an entire
// old or new sealed snapshot. A normal write beside each plant still succeeds.
func TestSnapshotDurabilityFaults(t *testing.T) {
	for _, step := range []string{"write", "file-sync", "publish", "dir-sync"} {
		t.Run(step, func(t *testing.T) {
			s, l, c, p, inv := fixture(t)
			r := mustPointerRoot(t, c, p, inv)
			codec := codecFor(t, r, inv)
			if err := l.WriteSealed(codec, p.Binding, []byte("old snapshot"), true); err != nil {
				t.Fatal(err)
			}
			reached := false
			s.fault = func(label string) error {
				if label == "state.bin:"+step {
					reached = true
					if step == "write" {
						return syscall.ENOSPC
					}
					return syscall.EIO
				}
				return nil
			}
			if err := l.WriteSealed(codec, p.Binding, []byte("new snapshot"), false); err != ErrIO || !reached {
				t.Fatalf("plant not reached: %v", err)
			}
			s.fault = nil
			l = reopenLease(t, s, l)
			c.lease = l
			got, err := l.ReadSealed(codec, p.Binding)
			want := "old snapshot"
			if step == "dir-sync" {
				want = "new snapshot"
			}
			if err != nil || string(got) != want {
				t.Fatal("partial snapshot", err)
			}
			if err = l.WriteSealed(codec, p.Binding, []byte("new snapshot"), false); err != nil {
				t.Fatal(err)
			}
			got, err = l.ReadSealed(codec, p.Binding)
			if err != nil || string(got) != "new snapshot" {
				t.Fatal("positive replacement", err)
			}
		})
	}
}

// Fixed password sections have exact service+full-MXID binding, bounded values,
// create-only semantics and no unsafe encodings; failures leave both stores intact.
func TestCredentialSectionsAndCollisions(t *testing.T) {
	_, l, c, p, inv := fixture(t)
	r := mustPointerRoot(t, c, p, inv)
	original, _ := l.read("secrets.json", MaxItem)
	mxid := "@agent:example.test"
	password := []byte("planted-password-123456789")
	if err := l.CreatePassword(mxid, password, inv); err != nil {
		t.Fatal(err)
	}
	section, err := AccountPasswordSection(mxid)
	if err != nil || section.Service != PasswordService || section.Account != mxid {
		t.Fatal("section", err)
	}
	got, err := l.ReadPassword(mxid, inv)
	if err != nil || !bytes.Equal(got, password) {
		t.Fatal("password readback", err)
	}
	if _, err = l.ReadPassword("@other:example.test", inv); err != ErrUnavailable {
		t.Fatalf("absent item: %v", err)
	}
	if err = l.CreatePassword(mxid, []byte("different-password-123"), inv); err != ErrExists {
		t.Fatalf("duplicate password: %v", err)
	}
	for _, bad := range []string{"short", strings.Repeat("a", 257), "password\nline-injection-123", "password\rline-injection-123", "password\x00control-123", "password line-injection-123"} {
		if err = l.CreatePassword(mxid, []byte(bad), inv); err != ErrInvalid {
			t.Fatalf("unsafe password: %v", err)
		}
	}
	collision := append([]string{LegacyTokenService}, p.Service())
	if err = ValidateServices(p, collision); err != ErrConflict {
		t.Fatalf("service collision: %v", err)
	}
	c.reserved = collision
	if _, err = c.ReadRoot(p); err != ErrConflict {
		t.Fatal("collision read", err)
	}
	if err = c.CreateRoot(p, r); err != ErrConflict {
		t.Fatal("collision write", err)
	}
	c.reserved = nil
	after, _ := l.read("secrets.json", MaxItem)
	if !bytes.Equal(original, after) {
		t.Fatal("root modified")
	}
	got, err = l.ReadPassword(mxid, inv)
	if err != nil || !bytes.Equal(got, password) {
		t.Fatal("password modified", err)
	}
	for _, identity := range []string{"agent", "@agent:example.test\n", "@agent:", strings.Repeat("x", 256)} {
		if _, err = AccountPasswordSection(identity); err != ErrInvalid {
			t.Fatal("bad identity admitted")
		}
	}
	if _, err = encodeRoot(p, Root{Version: 1, Binding: p.Binding, Seeds: r.Seeds, Wrap: r.Wrap + "\n"}, inv); err != ErrCodec {
		t.Fatalf("newline root encoding: %v", err)
	}
}

// Strict root/pointer records reject duplicate/unknown fields, noncanonical
// base64 and binding tampering, beside exact canonical decoding.
func TestCustodyCodecs(t *testing.T) {
	_, _, _, p, inv := fixture(t)
	r, err := NewRoot(p, inv)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeRoot(p, r, inv)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeRoot(p, encoded, inv)
	if err != nil || got != r {
		t.Fatal("valid bundle", err)
	}
	raw, _ := json.Marshal(r)
	for _, tc := range []struct {
		raw  string
		want error
	}{
		{string(raw[:len(raw)-1]) + `,"v":1}`, ErrCodec},
		{string(raw[:len(raw)-1]) + `,"extra":true}`, ErrCodec},
		{strings.Replace(string(raw), `"v":1`, `"v":2`, 1), ErrIntegrity},
	} {
		b := []byte(rootPrefix + base64.RawURLEncoding.EncodeToString([]byte(tc.raw)))
		if _, err = decodeRoot(p, b, inv); err != tc.want {
			t.Fatal("bad root rejection", err)
		}
	}
	for _, bad := range [][]byte{append(append([]byte(nil), encoded...), '\n'), append(append([]byte(nil), encoded...), '='), []byte(rootPrefix + "!"), make([]byte, MaxItem+1)} {
		if _, err = decodeRoot(p, bad, inv); err != ErrCodec {
			t.Fatal("bad encoding rejection", err)
		}
	}
	var dest Pointer
	if err = strictJSON([]byte(`{"v":1,"v":1}`), &dest, 4096); err != ErrCodec {
		t.Fatal("duplicate pointer")
	}
	if err = strictJSON([]byte(`{"unknown":1}`), &dest, 4096); err != ErrCodec {
		t.Fatal("unknown pointer")
	}
	if TupleHash("a:b", "c") == TupleHash("a", "b:c") {
		t.Fatal("ambiguous namespace")
	}
}

// Each emitted raw/base64/URL/hex/JSON encoding of planted loaded values is
// removed; typed diagnostics suppress even unknown body-bearing library errors.
func TestSecretInventory(t *testing.T) {
	secret := []byte("planted-secret-+/\"\\12345")
	inv := new(Inventory)
	inv.Register(secret)
	quoted, _ := json.Marshal(string(secret))
	encodings := []string{string(secret), string(quoted), base64.StdEncoding.EncodeToString(secret), base64.RawStdEncoding.EncodeToString(secret), base64.URLEncoding.EncodeToString(secret), base64.RawURLEncoding.EncodeToString(secret), hex.EncodeToString(secret), strings.ToUpper(hex.EncodeToString(secret)), url.QueryEscape(string(secret)), numericBytes(secret)}
	for i, encoded := range encodings {
		if got := inv.Scrub("prefix " + encoded + " suffix"); strings.Contains(got, encoded) {
			t.Fatalf("encoding %d survived", i)
		}
	}
	if inv.Scrub("public diagnostic") != "public diagnostic" {
		t.Fatal("scrubber rejects everything")
	}
	err := engineError(errors.New("plaintext-body-unknown-not-registered"))
	if err != ErrCodec || strings.Contains(err.Error(), "plaintext-body") {
		t.Fatal("library error leaked")
	}
}

// Local exclusion refuses a contender, and release permits a new holder. Every wait observes completion
// or failure instead of waiting forever on a readiness signal.
func TestGlobalCryptoLock(t *testing.T) {
	s, l, _, _, _ := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		held, err := s.Lock(ctx)
		if held != nil {
			held.Close()
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != ErrLock {
			t.Fatalf("concurrent lock: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lock wait did not terminate")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := s.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	next.Close()

}

// Witness Working must emit only its label (this assertion fails on b719459c);
// Root/Codec are controls. Every secret/token container is checked for every
// requested verb, against raw, hex, decimal-array and base64 planted encodings.
// A method-free legacy view proves the label oracle detects an unprotected type.
func TestValueFreeFormatting(t *testing.T) {
	store, lease, c, p, inv := fixture(t)
	r := mustPointerRoot(t, c, p, inv)
	secret := bytes.Repeat([]byte{128, 9, 171, 127}, 8)
	for i := range r.Seeds {
		r.Seeds[i] = base64.RawURLEncoding.EncodeToString(secret)
	}
	r.Wrap = base64.RawURLEncoding.EncodeToString(secret)
	codec := codecFor(t, r, inv)
	engine, w, _ := working(t)
	w.account.IdKeys.Ed25519.PrivateKey = append(append([]byte(nil), secret...), secret...)
	w.account.IdKeys.Curve25519.PrivateKey = append([]byte(nil), secret...)
	token := base64.RawStdEncoding.EncodeToString(secret)
	cursor := Cursor{Token: token}
	prepared := Prepared{Since: cursor, Next: cursor, Evidence: json.RawMessage(`{"body":"` + token + `"}`)}
	state := EngineState{Version: 1, Account: secret, Olm: map[string][]byte{"pickle": secret}}
	snapshot := Snapshot{Version: 1, Revision: 1, Engine: state, Prepared: &prepared}
	password := passwordRecord{Version: 1, Value: token}
	type legacyWorking Working
	if fmt.Sprintf("%v", (*legacyWorking)(w)) == "<e2ee-working>" {
		t.Fatal("formatter witness not discriminating")
	}
	values := []struct {
		name, label string
		value       any
	}{
		{"witness-Working", "<e2ee-working>", w}, {"Working-value", "<e2ee-working>", *w},
		{"control-Root", "<e2ee-root>", r}, {"Root-pointer", "<e2ee-root>", &r},
		{"control-Codec", "<e2ee-codec>", codec}, {"Codec-value", "<e2ee-codec>", *codec},
		{"Snapshot", "<e2ee-snapshot>", snapshot}, {"Snapshot-pointer", "<e2ee-snapshot>", &snapshot},
		{"Prepared", "<e2ee-prepared>", prepared}, {"Prepared-pointer", "<e2ee-prepared>", &prepared},
		{"Cursor", "<e2ee-cursor>", cursor}, {"Cursor-pointer", "<e2ee-cursor>", &cursor},
		{"EngineState", "<e2ee-engine-state>", state}, {"EngineState-pointer", "<e2ee-engine-state>", &state},
		{"PinRecord", "<e2ee-pin-record>", shapePin()},
		{"Inventory", "<e2ee-secret-inventory>", inv},
		{"Inventory-zero-value", "<e2ee-secret-inventory>", Inventory{}},
		{"Engine", "<e2ee-engine>", engine}, {"Engine-value", "<e2ee-engine>", *engine},
		{"PasswordRecord", "<e2ee-password-record>", password}, {"PasswordRecord-pointer", "<e2ee-password-record>", &password},
		{"FileCustody", "<e2ee-file-custody>", c}, {"FileCustody-value", "<e2ee-file-custody>", *c},
		{"FileStore", "<e2ee-file-store>", store}, {"FileStore-value", "<e2ee-file-store>", *store},
		{"LockedStore", "<e2ee-locked-store>", lease}, {"LockedStore-value", "<e2ee-locked-store>", *lease},
	}
	var encodings []string
	for _, raw := range [][]byte{secret, append(append([]byte(nil), secret...), secret...)} {
		encodings = append(encodings, string(raw), hex.EncodeToString(raw), strings.ToUpper(hex.EncodeToString(raw)), fmt.Sprint(raw), strings.ReplaceAll(fmt.Sprint(raw), " ", ","), base64.StdEncoding.EncodeToString(raw), base64.RawStdEncoding.EncodeToString(raw), base64.RawURLEncoding.EncodeToString(raw))
	}
	for _, tc := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
			output := fmt.Sprintf(verb, tc.value)
			if output != tc.label {
				t.Fatalf("%s %s is not a fixed label", tc.name, verb)
			}
			for _, encoded := range encodings {
				if strings.Contains(output, encoded) {
					t.Fatalf("%s %s exposes a planted encoding", tc.name, verb)
				}
			}
		}
	}
}

// Directory creation and lock-file fsync failures are surfaced at their exact
// boundary; retry uses the existing directory without repairing its metadata.
func TestDirectoryAndLockSyncInterruptions(t *testing.T) {
	for _, step := range []string{"mkdir:child-sync", "mkdir:parent-sync"} {
		t.Run(step, func(t *testing.T) {
			dir := filepath.Join(testRoot(t), "crypto")
			reached := false
			failed, err := openFileStore(dir, func(label string) error {
				if label == step {
					reached = true
					return syscall.EIO
				}
				return nil
			})
			if failed != nil || err != ErrIO || !reached {
				t.Fatalf("directory sync plant not reached: %v", err)
			}
			s, err := OpenFileStore(dir)
			if err != nil {
				t.Fatal("directory retry", err)
			}
			defer s.Close()
			lease, err := s.Lock(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			lease.Close()
		})
	}
	for _, step := range []string{"lock-file-sync", "lock-dir-sync"} {
		t.Run(step, func(t *testing.T) {
			s, err := OpenFileStore(filepath.Join(testRoot(t), "crypto"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			reached := false
			s.fault = func(label string) error {
				if label == step {
					reached = true
					return syscall.EIO
				}
				return nil
			}
			lease, err := s.Lock(t.Context())
			if lease != nil || err != ErrIO || !reached {
				t.Fatalf("lock sync plant not reached: %v", err)
			}
			s.fault = nil
			lease, err = s.Lock(t.Context())
			if err != nil {
				t.Fatal("lock retry", err)
			}
			lease.Close()
		})
	}
}

// Restart removes only validated orphan staging names while preserving the
// published root; a planted staging symlink refuses without following its target.
func TestOrphanStagingRecovery(t *testing.T) {
	s, l, c, p, inv := fixture(t)
	root := mustPointerRoot(t, c, p, inv)
	name := ".stage-" + strings.Repeat("a", 32)
	if err := l.publish(name, []byte("orphan staging bytes"), true); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	reached := false
	s.fault = func(label string) error {
		if label == "recovery-stage-sync" {
			reached = true
			return syscall.EIO
		}
		return nil
	}
	if failed, err := s.Lock(t.Context()); failed != nil || err != ErrIO || !reached {
		t.Fatal("stage cleanup sync plant", err)
	}
	s.fault = nil
	next, err := s.Lock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	c.lease = next
	got, err := c.ReadRoot(p)
	if err != nil || got != root {
		t.Fatal("cleanup removed root", err)
	}
	if _, err = next.read(name, 100); err != ErrUnavailable {
		t.Fatal("orphan stage retained", err)
	}
	target := filepath.Join(testRoot(t), "sentinel")
	if err = os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = unix.Symlinkat(target, int(s.dir.Fd()), name); err != nil {
		t.Fatal(err)
	}
	next.Close()
	if failed, err := s.Lock(t.Context()); failed != nil || err != ErrUnsafe {
		t.Fatal("unsafe stage admitted", err)
	}
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != "unchanged" {
		t.Fatal("stage symlink target modified", err)
	}
}

// Valid locator/bundle readback succeeds around each isolated corruption plant;
// conflicting generation, namespace and bundle binding fail without root repair.
func TestTamperedLocatorAndBundle(t *testing.T) {
	_, l, c, p, inv := fixture(t)
	r := mustPointerRoot(t, c, p, inv)
	originalRoot, _ := l.read("secrets.json", MaxItem)
	originalPointer, _ := json.Marshal(p)
	for _, change := range []func(Pointer) Pointer{
		func(v Pointer) Pointer { v.Binding.Generation = strings.Repeat("b", 32); return v },
		func(v Pointer) Pointer { v.Namespace = strings.Repeat("c", 64); return v },
		func(v Pointer) Pointer { v.Binding.Identity = "@other:example.test"; return v },
	} {
		if got, err := c.ReadRoot(p); err != nil || got != r {
			t.Fatal("valid root", err)
		}
		bad := change(p)
		raw, _ := json.Marshal(bad)
		if err := l.publish("generation.json", raw, false); err != nil {
			t.Fatal(err)
		}
		if _, err := c.ReadRoot(p); err != ErrIntegrity {
			t.Fatalf("tampered locator: %v", err)
		}
		unchanged, _ := l.read("secrets.json", MaxItem)
		if !bytes.Equal(unchanged, originalRoot) {
			t.Fatal("repaired root")
		}
		if err := l.publish("generation.json", originalPointer, false); err != nil {
			t.Fatal(err)
		}
	}
	changed := p
	changed.Binding.Device = "CONFLICT"
	badRoot := r
	badRoot.Binding = changed.Binding
	encoded, err := encodeRoot(changed, badRoot, inv)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.publish("secrets.json", encoded, false); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ReadRoot(p); err != ErrIntegrity {
		t.Fatalf("conflicting bundle: %v", err)
	}
	unchanged, _ := l.read("secrets.json", MaxItem)
	if !bytes.Equal(unchanged, encoded) {
		t.Fatal("repaired conflicting bundle")
	}
}
