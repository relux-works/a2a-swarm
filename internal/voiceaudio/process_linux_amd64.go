package voiceaudio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const maxExecutableBytes = 16 << 20

func snapshot(ctx context.Context, s *scratch, p Profile) Reason {
	if p.Executable == "" || p.SHA256 == "" {
		return MissingProfile
	}
	if p.ID != ProfileID || p.Platform != "linux/amd64" || !filepath.IsAbs(p.Executable) ||
		filepath.Clean(p.Executable) != p.Executable || !safePath(filepath.Dir(p.Executable), false) {
		return ProfileMismatch
	}
	pin, err := hex.DecodeString(p.SHA256)
	if err != nil || len(pin) != sha256.Size {
		return ProfileMismatch
	}
	fi, err := os.Lstat(p.Executable)
	if os.IsNotExist(err) {
		return MissingProfile
	}
	if err != nil || !ownedRegular(fi) || fi.Mode().Perm()&0100 == 0 || fi.Size() <= 0 || fi.Size() > maxExecutableBytes {
		return ProfileMismatch
	}
	f, err := os.Open(p.Executable)
	if err != nil {
		return MissingProfile
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(fi, opened) {
		return ProfileMismatch
	}
	image, err := elf.NewFile(f)
	if err != nil {
		return ProfileMismatch
	}
	if image.Class != elf.ELFCLASS64 || image.Data != elf.ELFDATA2LSB ||
		image.Machine != elf.EM_X86_64 || image.Type != elf.ET_EXEC {
		return ProfileMismatch
	}
	for _, program := range image.Progs {
		if program.Type == elf.PT_INTERP || program.Type == elf.PT_DYNAMIC {
			return ProfileMismatch
		}
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return ProfileMismatch
	}
	out, err := s.create("decoder", 0700)
	if err != nil {
		return UnsafeTMPDIR
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, hash), io.LimitReader(contextReader{ctx, f}, maxExecutableBytes+1))
	closeErr := out.Close()
	if reason := contextReason(ctx); reason != "" {
		return reason
	}
	if copyErr != nil || closeErr != nil || n != opened.Size() || n > maxExecutableBytes || !bytes.Equal(hash.Sum(nil), pin) {
		return ProfileMismatch
	}
	// Check the actual copied image too, rather than trusting two reads of a
	// mutable source descriptor. The hash authenticates exactly these bytes.
	copied, err := s.root.Open("decoder")
	if err != nil {
		return ProfileMismatch
	}
	defer copied.Close()
	image, err = elf.NewFile(copied)
	if err != nil || image.Class != elf.ELFCLASS64 || image.Data != elf.ELFDATA2LSB ||
		image.Machine != elf.EM_X86_64 || image.Type != elf.ET_EXEC {
		return ProfileMismatch
	}
	for _, program := range image.Progs {
		if program.Type == elf.PT_INTERP || program.Type == elf.PT_DYNAMIC {
			return ProfileMismatch
		}
	}
	return ""
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

type boundedOutput struct {
	bytes.Buffer
	overflow bool
	stop     func()
}

// Count and discard diagnostics without retaining their contents.
type boundedDiagnostic struct {
	seen     int
	overflow bool
	stop     func()
}

func (w *boundedDiagnostic) Write(p []byte) (int, error) {
	n := len(p)
	if n > MaxDiagnosticBytes-w.seen {
		w.seen = MaxDiagnosticBytes + 1
		w.overflow = true
		w.stop()
		return n, io.ErrShortWrite
	}
	w.seen += n
	return n, nil
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	room := len(nativeHeader) + MaxPCMBytes + 1 - w.Len()
	if room > 0 {
		w.Buffer.Write(p[:min(room, n)])
	}
	if w.Len() > len(nativeHeader)+MaxPCMBytes {
		w.overflow = true
		if w.stop != nil {
			w.stop()
		}
		return n, io.ErrShortWrite
	}
	return n, nil
}

func runNative(ctx context.Context, s *scratch) ([]byte, Reason) {
	cmd := exec.CommandContext(ctx, filepath.Join(s.path, "decoder"))
	cmd.Dir = s.path
	cmd.Env = []string{"HOME=" + filepath.Join(s.path, "home"), "TMPDIR=" + filepath.Join(s.path, "tmp"),
		"XDG_CACHE_HOME=" + filepath.Join(s.path, "cache"), "LANG=C", "LC_ALL=C"}
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stop := func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.Cancel = stop
	cmd.WaitDelay = 200 * time.Millisecond
	output := &boundedOutput{stop: func() { _ = stop() }}
	diagnostic := &boundedDiagnostic{stop: func() { _ = stop() }}
	cmd.Stdout = output
	cmd.Stderr = diagnostic
	if err := cmd.Start(); err != nil {
		if reason := contextReason(ctx); reason != "" {
			return nil, reason
		}
		return nil, DecodeFailed
	}
	err := cmd.Wait() // Always join/reap the child and its pipe-copy goroutines.
	_ = stop()        // Defensive process-group cleanup even on successful direct exit.
	if reason := contextReason(ctx); reason != "" {
		return nil, reason
	}
	if output.overflow {
		return nil, DurationLimit
	}
	if diagnostic.overflow {
		return nil, DiagnosticLimit
	}
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			switch exit.ExitCode() {
			case 71:
				return nil, Malformed
			case 73:
				return nil, ProfileMismatch
			case 74:
				return nil, DurationLimit
			}
		}
		return nil, DecodeFailed
	}
	return output.Bytes(), ""
}
