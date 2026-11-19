//go:build darwin || linux

package config

import (
	"bytes"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// fifoSubstitutionBound is a hang guard for a transaction that must finish
// promptly. Reaching it is a failure of its own, never the FIFO assertion.
const fifoSubstitutionBound = 30 * time.Second

// allGoroutineStacks returns every goroutine's stack for failure diagnostics.
func allGoroutineStacks() []byte {
	buf := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return buf[:n]
		}
		buf = make([]byte, 2*len(buf))
	}
}

// fifoReader reports, without blocking, whether some opener is waiting on or
// holds the FIFO at path for reading. A non-blocking write-only open of a
// FIFO fails with ENXIO while it has no reader, so it never succeeds for a
// FIFO nobody reads. A successful open is returned for the caller to hold:
// it wakes a reader blocked in its own open, which is the bounded rescue.
// The stacks are taken just before that open. Only this test's own FIFO is
// ever opened for writing: a path that is not a FIFO, or a descriptor that
// turns out not to be one, is left or closed unwritten.
func fifoReader(path string) (*os.File, []byte) {
	if info, err := os.Lstat(path); err != nil || info.Mode().Type() != os.ModeNamedPipe {
		return nil, nil
	}
	stacks := allGoroutineStacks()
	w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil
	}
	if info, err := w.Stat(); err != nil || info.Mode().Type() != os.ModeNamedPipe {
		w.Close()
		return nil, nil
	}
	return w, stacks
}

// releaseFIFO is the hang guard's rescue on this test's own FIFO: opening and
// closing a non-blocking read-write endpoint wakes an opener blocked on it,
// and a reader then sees end of file.
func releaseFIFO(path string) {
	if info, err := os.Lstat(path); err != nil || info.Mode().Type() != os.ModeNamedPipe {
		return
	}
	if f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
		f.Close()
	}
}

// Proves the pre-open regular-file check through the public transaction. A
// no-op callback replaces only the loaded state path, by rename, with a 0600
// substitute. The regular-file neighbour holds the exact input bytes and is
// elided: the path still names the substitute, and its descriptor is the one
// synced, so a substituted path does reach the elision open. A 0600 FIFO
// substitute must be rejected before any open: the transaction finishes
// within the bound, no opener ever reads the FIFO, the elided path claims no
// sync, and the ordinary replacement restores the exact input as a regular
// 0600 file. Removing only the pre-open check makes the elision open block on
// the FIFO before descriptor checks run; a non-blocking writer probe then
// finds that reader and releases it, the fallback still restores the state,
// and the final assertion fails. Limits: Linux and Darwin FIFO semantics; a
// probe can wake a reader only while the path still names this test's FIFO.
func TestUpdateStateUnchangedFIFOSubstitutionFallsBack(t *testing.T) {
	t.Run("regular file neighbour elided", func(t *testing.T) {
		p, raw := seedSyntheticBusyState(t)
		path := p.StateFile(busyStateName)
		var rec syncRecord
		swapOrdinaryStateSync(t, recordingSyncer(&rec, nil, -1, nil))
		var substitute os.FileInfo
		got, err := UpdateState(p, busyStateName, func(*State) error {
			tmp := path + ".synthetic-regular"
			if err := os.WriteFile(tmp, raw, 0o600); err != nil {
				return err
			}
			if err := os.Rename(tmp, path); err != nil {
				return err
			}
			info, err := os.Lstat(path)
			substitute = info
			return err
		})
		if err != nil || substitute == nil {
			t.Fatalf("regular substitute: err=%v", err)
		}
		final, err := os.Lstat(path)
		if err != nil || !os.SameFile(substitute, final) {
			t.Fatalf("elided no-op did not keep the regular substitute: %v", err)
		}
		if len(rec.files) != 1 || !os.SameFile(rec.files[0], substitute) || len(rec.dirs) != 2 {
			t.Fatalf("elision did not sync the substitute: files=%d dirs=%v", len(rec.files), rec.dirs)
		}
		if !bytes.Equal(raw, busyDisk(t, p)) || got.NextBatch != "s-synthetic-batch" || !validWriterSeal(got) {
			t.Fatal("elided no-op did not return the verified input")
		}
	})
	t.Run("FIFO substitute replaced without opening it", func(t *testing.T) {
		p, raw := seedSyntheticBusyState(t)
		path := p.StateFile(busyStateName)
		var rec syncRecord
		swapOrdinaryStateSync(t, recordingSyncer(&rec, nil, -1, nil))
		type outcome struct {
			got State
			err error
		}
		substituted := make(chan struct{})
		finished := make(chan outcome, 1)
		go func() {
			got, err := UpdateState(p, busyStateName, func(*State) error {
				tmp := path + ".synthetic-fifo"
				if err := syscall.Mkfifo(tmp, 0o600); err != nil {
					return err
				}
				if err := os.Rename(tmp, path); err != nil {
					return err
				}
				close(substituted)
				return nil
			})
			finished <- outcome{got, err}
		}()
		var reader *os.File
		var stacks []byte
		t.Cleanup(func() {
			if reader != nil {
				reader.Close()
			}
		})
		wait := substituted
		bound := time.NewTimer(fifoSubstitutionBound)
		defer bound.Stop()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		var res outcome
	poll:
		for {
			select {
			case res = <-finished:
				break poll
			case <-wait:
				wait = nil
			case <-tick.C:
				if wait == nil && reader == nil {
					reader, stacks = fifoReader(path)
				}
			case <-bound.C:
				dump := allGoroutineStacks()
				if reader != nil {
					reader.Close()
					reader = nil
				}
				releaseFIFO(path)
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
				}
				t.Fatalf("transaction did not finish within %v; goroutines before rescue:\n%s", fifoSubstitutionBound, dump)
			}
		}
		opened := reader != nil
		if reader != nil {
			reader.Close()
			reader = nil
		}
		if res.err != nil {
			t.Fatalf("FIFO substitution: %v", res.err)
		}
		final, err := os.Lstat(path)
		if err != nil || !final.Mode().IsRegular() || final.Mode().Perm() != 0o600 {
			t.Fatalf("fallback did not restore a regular 0600 state file: %v", err)
		}
		if !bytes.Equal(raw, busyDisk(t, p)) || res.got.NextBatch != "s-synthetic-batch" || !validWriterSeal(res.got) {
			t.Fatal("fallback did not persist the transaction's own representation")
		}
		if len(rec.files) != 0 || len(rec.dirs) != 0 {
			t.Fatalf("elided path claimed a FIFO: files=%d dirs=%v", len(rec.files), rec.dirs)
		}
		if opened {
			t.Logf("goroutines when the FIFO reader was found:\n%s", stacks)
			t.Fatal("elision opened the substituted FIFO for reading before rejecting it")
		}
	})
}
