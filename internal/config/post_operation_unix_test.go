//go:build darwin || linux

package config

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type operationForeignInfo struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (s operationForeignInfo) Sys() any { return &s.stat }
func TestPostJournalForeignOwnerRefused(t *testing.T) {
	_, j := operationFixture(t)
	st, err := j.root.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	if !postOperationOwner(st, true) {
		t.Fatal("positive own private directory refused")
	}
	foreign := operationForeignInfo{FileInfo: st, stat: *st.Sys().(*syscall.Stat_t)}
	foreign.stat.Uid = uint32(os.Geteuid()) + 1
	if postOperationOwner(foreign, true) {
		t.Fatal("foreign owner accepted")
	}
}

func TestPostJournalNonregularEntriesRefuseWithoutBlocking(t *testing.T) {
	p, j := operationFixture(t)
	mustPrepareOperation(t, j, operationCandidate("fifo", 1))
	path := filepath.Join(p.Base, "post-operations", "agents", "alice")
	if err := j.root.Remove("fifo.json"); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(path, "fifo.json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.Read("fifo"); err == nil {
		t.Fatal("FIFO record accepted")
	}
	if err := j.root.Remove(".lock"); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(path, ".lock"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.Prepare(operationCandidate("blocked", 1)); err == nil {
		t.Fatal("FIFO lock accepted")
	}
}
