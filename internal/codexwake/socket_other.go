//go:build !unix

package codexwake

import "errors"

func SocketIdentity(string) (string, error) {
	return "", errors.New("native runtime binding requires Unix")
}
