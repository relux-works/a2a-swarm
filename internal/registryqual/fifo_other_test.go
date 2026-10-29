//go:build !unix

package registryqual

import "errors"

func mkfifo(string) error { return errors.New("no FIFO on this platform") }
