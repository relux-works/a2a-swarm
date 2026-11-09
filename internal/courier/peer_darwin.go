//go:build darwin

package courier

import (
	"golang.org/x/sys/unix"
	"net"
)

func peerUID(c *net.UnixConn) (uint32, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, errRefused
	}
	var uid uint32
	var probe error
	err = raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		probe = e
		if e == nil {
			uid = cred.Uid
		}
	})
	if err != nil || probe != nil {
		return 0, errRefused
	}
	return uid, nil
}
