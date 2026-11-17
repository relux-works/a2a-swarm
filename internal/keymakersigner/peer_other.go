//go:build !linux

package keymakersigner

import (
	"net"
	"os"
)

// Native peer authentication is not qualified on other platforms. Refuse before
// connecting rather than silently weakening the Linux endpoint pins.
func ownedDirectory(os.FileInfo, uint32) bool      { return false }
func ownedSocket(os.FileInfo, uint32, uint32) bool { return false }
func peerUID(net.Conn, uint32) bool                { return false }
