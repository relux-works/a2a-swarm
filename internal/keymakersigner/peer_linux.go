//go:build linux

package keymakersigner

import (
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func ownedDirectory(st os.FileInfo, uid uint32) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && (s.Uid == 0 || s.Uid == uid)
}

func ownedSocket(st os.FileInfo, uid, gid uint32) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && s.Uid == uid && s.Gid == gid
}

func peerUID(conn net.Conn, uid uint32) bool {
	c, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return false
	}
	var cred *unix.Ucred
	var sockErr error
	err = raw.Control(func(fd uintptr) {
		cred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	return err == nil && sockErr == nil && cred != nil && cred.Uid == uid
}
