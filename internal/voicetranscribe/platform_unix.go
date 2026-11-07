//go:build linux || darwin

package voicetranscribe

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func safeDirectory(st os.FileInfo, private bool) bool {
	if st == nil || !st.IsDir() {
		return false
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	if private {
		return int(s.Uid) == os.Geteuid() && st.Mode().Perm() == 0700 &&
			st.Mode()&(os.ModeSticky|os.ModeSetgid|os.ModeSetuid) == 0
	}
	return (s.Uid == 0 || int(s.Uid) == os.Geteuid()) &&
		(st.Mode().Perm()&0022 == 0 || st.Mode()&os.ModeSticky != 0)
}

func safeArtifact(st os.FileInfo) bool {
	if st == nil || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 {
		return false
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && (s.Uid == 0 || int(s.Uid) == os.Geteuid())
}

func openReadOnly(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

func runEngine(parent context.Context, dir string) ([]byte, *Refusal) {
	ctx, cancel := context.WithTimeout(parent, InferenceDeadline)
	defer cancel()
	w := &limitedOutput{cancel: cancel}
	cmd := exec.CommandContext(ctx, filepath.Join(dir, "bin/sherpa-onnx-offline"),
		"--whisper-encoder=model/tiny-encoder.int8.onnx",
		"--whisper-decoder=model/tiny-decoder.int8.onnx",
		"--tokens=model/tiny-tokens.txt", "--whisper-task=transcribe",
		"--whisper-tail-paddings=300", "--num-threads=2", "--provider=cpu",
		"--decoding-method=greedy_search", "input.wav")
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + filepath.Join(dir, "home"),
		"TMPDIR=" + filepath.Join(dir, "tmp"), "XDG_CACHE_HOME=" + filepath.Join(dir, "cache"),
		"OMP_NUM_THREADS=2", "OPENBLAS_NUM_THREADS=2"}
	cmd.Stdout, cmd.Stderr = w, io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	killGroup := func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.Cancel = killGroup
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return nil, &Refusal{contextCode(ctx), InferenceStage}
		}
		return nil, &Refusal{BackendUnavailable, BackendStage}
	}
	err := cmd.Wait() // joins the direct child and the bounded stdout copier
	// Also kill a child group left behind by an early-exiting native parent.
	// No shell is used; the qualified official CLI itself spawns no children.
	killGroup()
	if w.overflow {
		return nil, &Refusal{OutputLimit, OutputStage}
	}
	if ctx.Err() != nil {
		return nil, &Refusal{contextCode(ctx), InferenceStage}
	}
	if err != nil {
		return nil, &Refusal{EngineFailed, InferenceStage}
	}
	return w.data, nil
}
