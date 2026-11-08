//go:build linux

package cli

import (
	"debug/elf"
	"fmt"
	"io"
	"runtime"
	"syscall"
)

func executableImage(st *syscall.Stat_t) string  { return fmt.Sprintf("%d:%d", st.Dev, st.Ino) }
func executableChanged(st *syscall.Stat_t) int64 { return st.Ctim.Sec*1e9 + st.Ctim.Nsec }

func executableRange(offset, size uint64, length int64) bool {
	return offset <= uint64(length) && size <= uint64(length)-offset
}

func completeTerminalExecutable(f io.ReaderAt, size int64) bool {
	image, err := elf.NewFile(f)
	if err != nil || (image.Type != elf.ET_EXEC && image.Type != elf.ET_DYN) {
		return false
	}
	want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64, "386": elf.EM_386, "arm": elf.EM_ARM, "riscv64": elf.EM_RISCV, "ppc64le": elf.EM_PPC64, "s390x": elf.EM_S390}[runtime.GOARCH]
	if want == elf.EM_NONE || image.Machine != want || len(image.Progs) == 0 {
		return false
	}
	loaded := false
	for _, p := range image.Progs {
		if !executableRange(p.Off, p.Filesz, size) {
			return false
		}
		if p.Type == elf.PT_LOAD && p.Flags&elf.PF_X != 0 {
			loaded = true
		}
	}
	for _, s := range image.Sections {
		if s.Type != elf.SHT_NOBITS && !executableRange(s.Offset, s.FileSize, size) {
			return false
		}
	}
	return loaded
}
