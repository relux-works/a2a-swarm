//go:build linux

package keymakersigner

import (
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Package-private lookup seams let tests isolate each pin without changing
// ownership or provisioning another OS account. Production uses native facts.
var (
	lookupOwnership = filesystemOwnership
	lookupPeerUID   = kernelPeerUID
)

func filesystemOwnership(st os.FileInfo) (uint32, uint32, bool) {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return s.Uid, s.Gid, true
}

func ownedDirectory(st os.FileInfo, uid uint32) bool {
	owner, _, ok := lookupOwnership(st)
	return ok && (owner == 0 || owner == uid)
}

func ownedSocket(st os.FileInfo, uid, gid uint32) bool {
	owner, group, ok := lookupOwnership(st)
	return ok && owner == uid && group == gid
}

func peerUID(conn net.Conn, uid uint32) bool {
	peer, ok := lookupPeerUID(conn)
	return ok && peer == uid
}

func kernelPeerUID(conn net.Conn) (uint32, bool) {
	c, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, false
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, false
	}
	var cred *unix.Ucred
	var sockErr error
	err = raw.Control(func(fd uintptr) {
		cred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err != nil || sockErr != nil || cred == nil {
		return 0, false
	}
	return cred.Uid, true
}
