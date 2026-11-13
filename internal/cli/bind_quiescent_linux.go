//go:build linux

package cli

import (
	"bytes"
	"errors"
	"os"
	"strconv"
)

// processStart returns pid's kernel start time (field 22 of /proc/PID/stat)
// without forking. Together with a canonical Birth check at registration it
// identifies one process instance; a reused PID has another start time.
func processStart(pid int) (string, error) {
	if pid <= 1 {
		return "", errors.New("invalid pid")
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	// The command name is parenthesised and may contain spaces or ')'.
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 {
		return "", errors.New("malformed process stat")
	}
	fields := bytes.Fields(raw[end+1:])
	// fields[0] is field 3 (state); starttime is field 22.
	if len(fields) < 20 {
		return "", errors.New("malformed process stat")
	}
	return string(fields[19]), nil
}
