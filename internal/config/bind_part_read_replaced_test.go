package config

import (
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestBindPartReadSurvivesReplacement proves that a read of the sidecar which
// finds the file replaced under it starts again and returns the file that is
// there now, and that the State of the identity loads through it. A writer
// replaces the sidecar by rename and readers take no lock, so every commit of
// a cursor, a lease or a status can fall inside a read.
//
// Regression witness: the read answered "bind state sidecar is invalid" for
// such a replacement, and a command or hook of a marked identity failed with
// it whenever another process committed at that moment.
//
// Controls: a file that is replaced under every attempt is still reported as
// invalid, after bindPartReadAttempts attempts; and a sidecar that is invalid
// in itself is reported at once, without another attempt.
func TestBindPartReadSurvivesReplacement(t *testing.T) {
	p, _, view := bindFixture(t, "s0", false)
	owner := BindOperationOwner{Holder: "consumer", Host: "host-1", PID: os.Getpid(), Birth: "birth-1"}
	alive := func(BindOperationOwner) bool { return false }
	replace := func(i int) {
		t.Helper()
		token := strings.Repeat(string(rune('a'+i%6)), 32)
		if _, err := BeginBindCursor(p, bindTestName, token, "delivery", view.NextBatch, "", owner, alive); err != nil {
			t.Fatalf("replacement %d, reserve: %v", i, err)
		}
		if err := AbortBindCursor(p, bindTestName, token); err != nil {
			t.Fatalf("replacement %d, release: %v", i, err)
		}
	}
	t.Cleanup(func() { afterBindPartOpen = nil })

	// One replacement inside the read.
	opens := 0
	afterBindPartOpen = func() {
		opens++
		if opens == 1 {
			afterBindPartOpen = nil
			replace(0)
			afterBindPartOpen = func() { opens++ }
		}
	}
	st, err := LoadState(p, bindTestName)
	if err != nil || st.NextBatch != view.NextBatch || !BindPartMarked(st) {
		t.Fatalf("load across one replacement: cursor=%q err=%v", st.NextBatch, err)
	}
	if opens < 2 {
		t.Fatalf("the read opened the sidecar %d time(s); the replacement was not inside a read", opens)
	}

	// Control: replaced under every attempt.
	opens = 0
	var hook func()
	hook = func() {
		opens++
		afterBindPartOpen = nil
		replace(opens)
		afterBindPartOpen = hook
	}
	afterBindPartOpen = hook
	if _, _, err := readBindPart(p, bindTestName); !errors.Is(err, ErrBindPartInvalid) {
		t.Fatalf("a file replaced under every attempt = %v, want ErrBindPartInvalid", err)
	}
	if opens != bindPartReadAttempts {
		t.Fatalf("the read made %d attempts, want %d", opens, bindPartReadAttempts)
	}

	// Control: an invalid document is not read again.
	opens = 0
	afterBindPartOpen = func() { opens++ }
	data, err := os.ReadFile(p.BindPartFile(bindTestName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.BindPartFile(bindTestName), append(data, '{'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readBindPart(p, bindTestName); !errors.Is(err, ErrBindPartInvalid) || opens != 1 {
		t.Fatalf("an invalid document: err=%v after %d attempt(s), want ErrBindPartInvalid after 1", err, opens)
	}
}

// TestBindPartLockFreeReadsUnderCommits is the same claim without a seam:
// while one goroutine commits the sidecar as fast as it can, several others
// load the State without a lock, and none of the loads fails. It is a stress
// run, so a pass shows no failure in this many reads and proves nothing about
// a rarer interleaving; the seam test above is the witness.
func TestBindPartLockFreeReadsUnderCommits(t *testing.T) {
	p, _, view := bindFixture(t, "s0", false)
	owner := BindOperationOwner{Holder: "consumer", Host: "host-1", PID: os.Getpid(), Birth: "birth-1"}
	alive := func(BindOperationOwner) bool { return false }
	stop := make(chan struct{})
	var commits atomic.Int64
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			token := strings.Repeat(string(rune('a'+i%6)), 32)
			if _, err := BeginBindCursor(p, bindTestName, token, "delivery", view.NextBatch, "", owner, alive); err != nil {
				continue
			}
			if AbortBindCursor(p, bindTestName, token) == nil {
				commits.Add(2)
			}
		}
	}()
	const readers, reads = 4, 1500
	failures := make(chan error, readers*reads)
	var wg sync.WaitGroup
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < reads; i++ {
				if _, err := LoadState(p, bindTestName); err != nil {
					failures <- err
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	writer.Wait()
	close(failures)
	n := 0
	var first error
	for err := range failures {
		if first == nil {
			first = err
		}
		n++
	}
	if n != 0 {
		t.Fatalf("%d of %d lock-free loads failed beside %d commits; first: %v", n, readers*reads, commits.Load(), first)
	}
	if commits.Load() < 100 {
		t.Fatalf("only %d commits ran beside the reads: the replacement was not exercised", commits.Load())
	}
}
