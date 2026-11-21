//go:build darwin

package config

import (
	"os"
	"syscall"
	"testing"
)

func TestPlatformFileChangeTimeMatchesStatCTime(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("directory stat has unexpected type %T", info.Sys())
	}
	want := st.Ctimespec.Sec*1_000_000_000 + st.Ctimespec.Nsec
	got, qualified := platformFileChangeTimeNS(info)
	if !qualified || got != want {
		t.Fatalf("platform change time = (%d, %v), want ctime %d", got, qualified, want)
	}
}
