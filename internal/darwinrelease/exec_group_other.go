//go:build !unix

package darwinrelease

import "os/exec"

func ownProcessGroup(*exec.Cmd)   {}
func reapOwnGroup(*exec.Cmd) bool { return false }
