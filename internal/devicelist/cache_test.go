package devicelist

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Claim: the cache is keyed by h(D) and holds only octets that hash to a pin
// (O3.11, O3.12). Control: the example is cached under the pin of section 3.2
// and found under its hash. Negatives: the same octets under a pin with
// another hash, the octets with one added space (S29-16), and the octets
// under the right hash with another seq (O3.6) refuse DEVICE_LIST_MISMATCH;
// octets that hash to the pin and are no valid list refuse
// DEVICE_LIST_INVALID; a malformed pin refuses DEVICE_PIN_INVALID. Nothing is
// cached on a refusal.
// Claim: matching pinned hash and sequence cache successfully, while invalid additions leave a new cache empty.
func TestCacheByHash(t *testing.T) {
	cache := mustCache(t, 0)
	pin := Pin{Seq: 5, SHA256: exampleHash}
	list, err := cache.Add([]byte(exampleList), pin)
	if err != nil || list.Hash() != exampleHash {
		t.Fatal("control:", err)
	}
	if held, ok := cache.Get(exampleHash); !ok || held != list {
		t.Fatal("control: the list is not found under h(D)")
	}
	other := mustParse(t, replaced(t, "phone P", "phone Q"))
	if _, ok := cache.Get(other.Hash()); ok {
		t.Fatal("a hash that was never added is found")
	}
	cache = mustCache(t, 0)
	if _, err := cache.Add([]byte(exampleList), pinOf(other)); err != DeviceListMismatch {
		t.Fatal("octets under a pin with another hash:", err)
	}
	if _, err := cache.Add([]byte(exampleList+" "), pin); err != DeviceListMismatch {
		t.Fatal("octets with one added space:", err)
	}
	if _, err := cache.Add([]byte(exampleList), Pin{Seq: 6, SHA256: exampleHash}); err != DeviceListMismatch {
		t.Fatal("octets under a pin with another seq:", err)
	}
	notAList := []byte(exampleList + " ")
	if _, err := cache.Add(notAList, Pin{Seq: 5, SHA256: HashOf(notAList)}); !errors.Is(err, DeviceListInvalid) {
		t.Fatal("octets that are no list:", err)
	}
	if _, err := cache.Add([]byte(exampleList), Pin{Seq: 5, SHA256: strings.ToUpper(exampleHash)}); err != DevicePinInvalid {
		t.Fatal("malformed pin:", err)
	}
	if cache.Len() != 0 {
		t.Fatal("a refused list was cached")
	}
}

// Claim: the cache is bounded. With a bound of 2, a third list drops the
// least recently used one, and its hash then misses. Control: the two lists
// that stay are found, and a list that was read stays over one that was not.
// Claim: cache capacity two evicts the least recently used list while retaining both remaining entries.
func TestCacheBounded(t *testing.T) {
	add := func(cache *Cache, seq string) *List {
		octets := replaced(t, `"seq":5`, `"seq":`+seq)
		list, err := cache.Add(octets, pinOf(mustParse(t, octets)))
		if err != nil {
			t.Fatal(err)
		}
		return list
	}
	cache := mustCache(t, 2)
	first, second := add(cache, "6"), add(cache, "7")
	if _, ok := cache.Get(first.Hash()); !ok {
		t.Fatal("control: first list not held")
	}
	third := add(cache, "8")
	if cache.Len() != 2 {
		t.Fatalf("cache holds %d lists over a bound of 2", cache.Len())
	}
	if _, ok := cache.Get(second.Hash()); ok {
		t.Fatal("the least recently used list was not dropped")
	}
	if _, ok := cache.Get(first.Hash()); !ok {
		t.Fatal("the list that was read was dropped")
	}
	if _, ok := cache.Get(third.Hash()); !ok {
		t.Fatal("the newest list is not held")
	}
}

// Claim: invalid cache bounds are refused rather than clamped; zero selects the default.
// 0 selects the
// default; a negative bound and one above the maximum are refused as
// ErrInvalid with no cache. Control: the maximum itself and 1 are taken as
// given.
func TestCacheBoundIsRefusedNotClamped(t *testing.T) {
	if mustCache(t, 0).max != DefaultCacheLists || mustCache(t, 1).max != 1 || mustCache(t, MaxCacheLists).max != MaxCacheLists {
		t.Fatal("control: a bound inside the range was not taken as given")
	}
	for _, max := range []int{-1, MaxCacheLists + 1} {
		if cache, err := NewCache(max); err != ErrInvalid || cache != nil {
			t.Fatalf("a bound of %d: %v", max, err)
		}
	}
}

// fetchImport names the packages through which a list could be fetched from
// a hint or a URL (O3.13, O3.14).
func fetchImport(path string) bool {
	return path == "net" || strings.HasPrefix(path, "net/") || path == "os/exec" || path == "plugin" || strings.Contains(path, "websocket")
}

// Claim: the package has no fetch path. No production file imports a network
// or process package, so a list can enter only as octets handed to Cache.Add.
// Controls: the predicate flags each such import and admits the ones the
// package uses, and the scan must have seen the production files.
// Claim: no production package source imports the listed network or process-launch packages.
func TestNoFetchPath(t *testing.T) {
	for _, path := range []string{"net", "net/http", "net/url", "os/exec", "github.com/coder/websocket"} {
		if !fetchImport(path) {
			t.Fatalf("predicate control missed %s", path)
		}
	}
	for _, path := range []string{"encoding/json", "crypto/sha256", "os", "golang.org/x/sys/unix"} {
		if fetchImport(path) {
			t.Fatalf("predicate control refused allowed %s", path)
		}
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(file, err)
		}
		scanned++
		for _, spec := range parsed.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(file, err)
			}
			if fetchImport(path) {
				t.Fatalf("%s imports %s", file, path)
			}
		}
	}
	if scanned < 8 {
		t.Fatalf("only %d production files scanned", scanned)
	}
}
