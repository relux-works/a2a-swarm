//go:build unix

package codexwake

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// SocketIdentity pins the actual endpoint object, so replacement at the same
// path cannot inherit an earlier launcher's generation.
func SocketIdentity(endpoint string) (string, error) {
	path, err := EndpointPath(endpoint)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", errors.New("runtime endpoint unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Getuid()) {
		return "", errors.New("runtime endpoint must be an owned private Unix socket")
	}
	return fmt.Sprintf("%d:%d:%d", stat.Dev, stat.Ino, info.ModTime().UnixNano()), nil
}
