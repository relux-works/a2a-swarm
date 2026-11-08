package courier

import (
	"regexp"
	"runtime"
	"strings"
	"unicode/utf8"
)

// ValidateSocketPath checks the v1 path bytes, without a pointer file's LF.
// UTF-8 is allowed; the 103-byte cap includes the leading slash. It does not
// clean the path or open it; station-home installation belongs to enroll.
func ValidateSocketPath(path string) error {
	if len(path) < 1 || len(path) > 103 || path[0] != '/' || !utf8.ValidString(path) {
		return errRefused
	}
	if strings.ContainsAny(path, "\x00\r\n") || strings.ContainsRune(path, '\uFEFF') {
		return errRefused
	}
	return nil
}

// DefaultSocket is platform-specific and does not consult identity config.
func DefaultSocket() string {
	if runtime.GOOS == "darwin" {
		return "/private/var/run/a2a/courier.sock"
	}
	return "/run/a2a/courier.sock"
}

var localName = regexp.MustCompile(`^[a-z0-9._=-]{1,64}$`)

// ValidateIdentity checks a bare v1 local identity selector, without a file LF.
func ValidateIdentity(identity string) error {
	if !localName.MatchString(identity) {
		return errRefused
	}
	return nil
}

// PointerFile encodes the v1 socket pointer as path bytes followed by one LF.
func PointerFile(path string) ([]byte, error) {
	if err := ValidateSocketPath(path); err != nil {
		return nil, err
	}
	return []byte(path + "\n"), nil
}

// ParsePointerFile accepts exactly one v1 pointer followed by one LF.
func ParsePointerFile(data []byte) (string, error) {
	if len(data) < 2 || data[len(data)-1] != '\n' {
		return "", errRefused
	}
	path := string(data[:len(data)-1])
	if err := ValidateSocketPath(path); err != nil {
		return "", err
	}
	return path, nil
}

// IdentityFile encodes a bare v1 identity selector followed by one LF.
func IdentityFile(identity string) ([]byte, error) {
	if err := ValidateIdentity(identity); err != nil {
		return nil, err
	}
	return []byte(identity + "\n"), nil
}

// ParseIdentityFile accepts exactly one v1 selector followed by one LF.
func ParseIdentityFile(data []byte) (string, error) {
	if len(data) < 2 || data[len(data)-1] != '\n' {
		return "", errRefused
	}
	identity := string(data[:len(data)-1])
	if err := ValidateIdentity(identity); err != nil {
		return "", err
	}
	return identity, nil
}
