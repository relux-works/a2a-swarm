package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// One fixed token and timestamp per completed refusal; no captures or errors.
func appendSendRefusal(s *session, refusal config.TerminalSendRefusal) error {
	path := filepath.Join(s.paths.Base, "state", s.name+".binding.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return errors.New("binding refusal log unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("binding refusal log must be a regular 0600 file")
	}
	_, err = fmt.Fprintf(f, "send_refusal=%s at=%s\n", tmuxwake.SafeSendReason(refusal.Reason), refusal.At.Format(time.RFC3339Nano))
	return err
}
