package config

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// TestMarkedStateCacheSameStampForeignBytes proves a consumer's cache never
// serves a marked State after the file was overwritten in place with other
// bytes of the same size and its modification time restored, so that inode,
// size, mtime and mode all still match the cached stamp. Each reader a bind
// consumer uses on its idle path is checked: the full State, the terminal
// projection, the idle-delivery view and the short lease-renewal path, which
// must fall back to a full load. The edited field is an ordinary one
// (a followed thread, the terminal outcome) that the sidecar does not carry,
// so only the State bytes can reveal the change.
//
// Control: the same overwrite of an unmarked State is detected by the cache's
// byte confirmation. The test covers a same-size overwrite only; a writer that
// replaces the file changes the stamp and is covered by the neighbouring tests.
func TestMarkedStateCacheSameStampForeignBytes(t *testing.T) {
	overwrite := func(t *testing.T, p Paths, name, old, new string) {
		t.Helper()
		path := p.StateFile(name)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		edited := bytes.Replace(raw, []byte(old), []byte(new), 1)
		if bytes.Equal(raw, edited) || len(raw) != len(edited) {
			t.Fatalf("same-size edit %q -> %q was not prepared", old, new)
		}
		cacheRestamp(t, path, edited)
	}

	t.Run("control: unmarked State", func(t *testing.T) {
		p := cachedPaths(t)
		if got := cachedLoad(t, p); len(got.Threads) != 1 || got.Threads[0] != "$root" {
			t.Fatalf("warm read = %v", got.Threads)
		}
		overwrite(t, p, "fixture", `"$root"`, `"$rooT"`)
		if got := cachedLoad(t, p); len(got.Threads) != 1 || got.Threads[0] != "$rooT" {
			t.Fatalf("unmarked State served from cache after a same-stamp overwrite: %v", got.Threads)
		}
	})

	t.Run("full State", func(t *testing.T) {
		p, _, _ := bindFixture(t, "s0", true)
		p = p.WithReadCache(NewReadCache(bindTestName))
		if got, err := LoadState(p, bindTestName); err != nil || len(got.Threads) != 1 || got.Threads[0] != "$keep" {
			t.Fatalf("warm read = %v err=%v", got.Threads, err)
		}
		overwrite(t, p, bindTestName, `"$keep"`, `"$keeP"`)
		got, err := LoadState(p, bindTestName)
		if err != nil {
			t.Fatalf("read after the overwrite: %v", err)
		}
		if len(got.Threads) != 1 || got.Threads[0] != "$keeP" {
			t.Fatalf("marked State served from cache after a same-stamp overwrite: %v", got.Threads)
		}
	})

	t.Run("terminal projection", func(t *testing.T) {
		p, _, _ := bindFixture(t, "s0", true)
		p = p.WithReadCache(NewReadCache(bindTestName))
		if b, err := LoadBindTerminal(p, bindTestName); err != nil || b == nil || b.Outcome != "running" {
			t.Fatalf("warm terminal read = %+v err=%v", b, err)
		}
		overwrite(t, p, bindTestName, `"running"`, `"runninG"`)
		b, err := LoadBindTerminal(p, bindTestName)
		if err != nil || b == nil {
			t.Fatalf("terminal read after the overwrite: %+v err=%v", b, err)
		}
		if b.Outcome != "runninG" {
			t.Fatalf("terminal projection served from cache after a same-stamp overwrite: %q", b.Outcome)
		}
	})

	t.Run("idle-delivery view", func(t *testing.T) {
		p, _, _ := bindFixture(t, "s0", true)
		p = p.WithReadCache(NewReadCache(bindTestName))
		if _, err := LoadState(p, bindTestName); err != nil {
			t.Fatal(err)
		}
		if view, hit, err := TryLoadCachedBindIdleDeliveryState(p, bindTestName); err != nil || !hit || !view.SealValid {
			t.Fatalf("warm idle view = %+v hit=%t err=%v", view, hit, err)
		}
		// The overwrite breaks the writer seal of the bytes on disk. A view that
		// still reports the seal valid was answered from the cached bytes.
		overwrite(t, p, bindTestName, `"$keep"`, `"$keeP"`)
		view, hit, err := TryLoadCachedBindIdleDeliveryState(p, bindTestName)
		if err != nil {
			t.Fatalf("idle view after the overwrite: %v", err)
		}
		if hit && view.SealValid {
			t.Fatalf("idle-delivery view served from cache after a same-stamp overwrite: %+v", view)
		}
	})

	t.Run("short lease renewal", func(t *testing.T) {
		p, _, _ := bindFixture(t, "s0", true)
		p = p.WithReadCache(NewReadCache(bindTestName))
		now := time.Now().UTC()
		renew := func(after time.Duration) {
			t.Helper()
			if _, err := AcquireOrRenewBindLease(p, bindTestName, "holder", "host-1", os.Getpid(), now.Add(after), LeaseTTL, LeaseTTL/2, nil); err != nil {
				t.Fatal(err)
			}
		}
		renew(0)
		if _, err := LoadState(p, bindTestName); err != nil {
			t.Fatal(err)
		}
		before := ReadStateDecodeStats()
		renew(time.Second)
		if d := ReadStateDecodeStats().LoadStateCalls - before.LoadStateCalls; d != 0 {
			t.Fatalf("control: the steady renewal loaded State %d times, so the short path was not taken", d)
		}
		overwrite(t, p, bindTestName, `"$keep"`, `"$keeP"`)
		before = ReadStateDecodeStats()
		renew(2 * time.Second)
		if d := ReadStateDecodeStats().LoadStateCalls - before.LoadStateCalls; d == 0 {
			t.Fatal("short lease renewal answered from the cached State after a same-stamp overwrite")
		}
	})
}
