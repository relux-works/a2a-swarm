//go:build darwin

package cli

import (
	"debug/macho"
	"fmt"
	"os"
	"runtime"
	"syscall"
)

// lsof's D field is a hexadecimal device number including its 0x prefix.
func executableImage(st *syscall.Stat_t) string {
	return fmt.Sprintf("0x%x:%d", uint32(st.Dev), st.Ino)
}
func executableChanged(st *syscall.Stat_t) int64 { return st.Ctimespec.Sec*1e9 + st.Ctimespec.Nsec }
func executableRange(offset, size uint64, length int64) bool {
	return offset <= uint64(length) && size <= uint64(length)-offset
}

func completeTerminalExecutable(f *os.File, size int64) bool {
	want := map[string]macho.Cpu{"amd64": macho.CpuAmd64, "arm64": macho.CpuArm64}[runtime.GOARCH]
	if want == 0 {
		return false
	}
	image, err := macho.NewFile(f)
	if err == nil {
		return completeMachOExecutable(image, size, want)
	}
	fat, err := macho.NewFatFile(f)
	if err != nil {
		return false
	}
	found := false
	for _, arch := range fat.Arches {
		header := arch.FatArchHeader
		if !executableRange(uint64(header.Offset), uint64(header.Size), size) || !completeMachOExecutable(arch.File, int64(header.Size), header.Cpu) {
			return false
		}
		found = found || header.Cpu == want
	}
	return found
}

func completeMachOExecutable(image *macho.File, size int64, want macho.Cpu) bool {
	if image.Type != macho.TypeExec || image.Cpu != want {
		return false
	}
	loaded := false
	for _, load := range image.Loads {
		if seg, ok := load.(*macho.Segment); ok {
			if !executableRange(seg.Offset, seg.Filesz, size) {
				return false
			}
			if seg.Prot&4 != 0 && seg.Filesz > 0 {
				loaded = true
			}
		}
	}
	for _, s := range image.Sections {
		// S_ZEROFILL, S_GB_ZEROFILL and S_THREAD_LOCAL_ZEROFILL have no file bytes.
		kind := s.Flags & 0xff
		if kind != 1 && kind != 0xc && kind != 0x12 && !executableRange(uint64(s.Offset), s.Size, size) {
			return false
		}
	}
	return loaded
}
