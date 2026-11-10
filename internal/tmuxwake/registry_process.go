package tmuxwake

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

const maxProcStatBytes = 4096

var linuxRegistryDomain = regexp.MustCompile(`^linux:[0-9a-f]{32}:pid:\[[1-9][0-9]*\]$`)

func validStartTicks(s string) bool {
	n, err := strconv.ParseUint(s, 10, 64)
	return err == nil && strconv.FormatUint(n, 10) == s
}

// Field 2 (comm) can contain whitespace and parentheses, including newlines.
// Its final ')' precedes state and the numeric fields; field 22 is starttime.
// Require the complete current Linux stat shape (through field 52), not a
// truncated prefix which happens to contain a plausible starttime token.
func procStatStart(data []byte, pid int) (string, error) {
	fail := errors.New("registry identity unavailable")
	prefix := strconv.Itoa(pid) + " ("
	end := strings.LastIndexByte(string(data), ')')
	if pid <= 1 || len(data) > maxProcStatBytes || !strings.HasPrefix(string(data), prefix) || end < len(prefix) || end+1 >= len(data) || data[end+1] != ' ' {
		return "", fail
	}
	fields := strings.Fields(string(data[end+2:]))
	if len(fields) < 50 || len(fields[0]) != 1 || !strings.ContainsAny(fields[0], "RSDZTWtXxKPI") {
		return "", fail
	}
	for _, field := range fields[1:] {
		number := strings.TrimPrefix(field, "-")
		if number == "" {
			return "", fail
		}
		for _, digit := range number {
			if digit < '0' || digit > '9' {
				return "", fail
			}
		}
	}
	if !validStartTicks(fields[19]) {
		return "", fail
	}
	return fields[19], nil
}

func registryIdentityFile(path string, limit int64) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("registry identity unavailable")
	}
	f := os.NewFile(uintptr(fd), "registry-identity")
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("registry identity unavailable")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || len(b) == 0 || int64(len(b)) > limit {
		return nil, errors.New("registry identity unavailable")
	}
	return b, nil
}

// Production supplies only /proc and /etc/machine-id. Explicit paths also let
// isolated fixtures exercise the file boundary without inspecting peer state.
// Read starttime around the namespace observation to detect PID replacement.
func linuxRegistryProcess(procRoot, machinePath string, pid int) (domain, ticks string, err error) {
	return linuxRegistryProcessUsing(procRoot, machinePath, pid, registryIdentityFile)
}

// The private reader seam permits deterministic read failures/replacement
// without racing or modifying a live process. Production always uses the
// bounded no-follow file reader above; there is no runtime override.
func linuxRegistryProcessUsing(procRoot, machinePath string, pid int, readFile func(string, int64) ([]byte, error)) (domain, ticks string, err error) {
	fail := errors.New("registry identity unavailable")
	if pid <= 1 {
		return "", "", fail
	}
	statPath := filepath.Join(procRoot, strconv.Itoa(pid), "stat")
	raw, err := readFile(statPath, maxProcStatBytes)
	if err != nil {
		return "", "", fail
	}
	ticks, err = procStatStart(raw, pid)
	if err != nil {
		return "", "", fail
	}
	machine, err := readFile(machinePath, 33)
	if err != nil {
		return "", "", fail
	}
	var link [64]byte
	n, err := syscall.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), "ns", "pid"), link[:])
	if err != nil || n == len(link) {
		return "", "", fail
	}
	domain = "linux:" + strings.TrimSuffix(string(machine), "\n") + ":" + string(link[:n])
	if !linuxRegistryDomain.MatchString(domain) {
		return "", "", fail
	}
	raw, err = readFile(statPath, maxProcStatBytes)
	if err != nil {
		return "", "", fail
	}
	after, err := procStatStart(raw, pid)
	if err != nil || after != ticks {
		return "", "", fail
	}
	return domain, ticks, nil
}
