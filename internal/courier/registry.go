//go:build linux || darwin

package courier

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const maxRegistryBytes = 512 * 1024
const maxStations = 1024

// Entry is a binding owned by Courier. Secret contains the ORIGINAL 32 random
// bytes, encoded as canonical padded standard base64 in stations.json.
type Entry struct {
	Identity string `json:"identity"`
	UID      uint32 `json:"uid"`
	Secret   []byte `json:"secret"`
}

type registry map[string]Entry

// openDirectory walks from / with no-follow descriptors, rejecting even
// ancestor symlinks. Only the final directory may be created.
func openDirectory(path string, create bool) (*os.File, error) {
	return openDirectoryMode(path, create, 0700)
}

// Socket directories have a separate, explicit traversable access policy.
func openDirectoryMode(path string, create bool, mode uint32) (*os.File, error) {
	return openDirectoryModeMkdir(path, create, mode, unix.Mkdirat)
}
func openDirectoryModeMkdir(path string, create bool, mode uint32, mkdir func(int, string, uint32) error) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, errRefused
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errRefused
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, p := range parts {
		next, e := unix.Openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e == unix.ENOENT && create && i == len(parts)-1 {
			if e = mkdir(fd, p, mode); e == nil {
				next, e = unix.Openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				if e == nil {
					e = unix.Fchmod(next, mode)
					if e != nil {
						unix.Close(next)
					}
				}
			} else if e == unix.EEXIST {
				next, e = unix.Openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
				if e == nil {
					var st unix.Stat_t
					if unix.Fstat(next, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&unix.S_IFMT != unix.S_IFDIR || uint32(st.Mode)&07777 & ^mode != 0 {
						e = errRefused
					} else {
						e = unix.Fchmod(next, mode)
					}
					if e != nil {
						unix.Close(next)
					}
				}
			}
		}
		unix.Close(fd)
		if e != nil {
			return nil, errRefused
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&unix.S_IFMT != unix.S_IFDIR || uint32(st.Mode)&07777 != mode {
		f.Close()
		return nil, errRefused
	}
	if mode == 0711 && st.Gid != uint32(os.Getegid()) {
		f.Close()
		return nil, errRefused
	}
	return f, nil
}

func secureStat(fd int, dir bool) bool {
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		return false
	}
	return validMetadata(&st, dir, uint32(os.Geteuid()))
}

func validMetadata(st *unix.Stat_t, dir bool, uid uint32) bool {
	if st.Uid != uid {
		return false
	}
	if dir {
		return st.Mode&unix.S_IFMT == unix.S_IFDIR && st.Mode&07777 == 0700
	}
	return st.Mode&unix.S_IFMT == unix.S_IFREG && st.Mode&07777 == 0600 && st.Nlink == 1
}

func privateFile(dir *os.File, name string, flags int) (*os.File, error) {
	// Only a newly created inode is chmodded. Existing unsafe files refuse.
	created := false
	var fd int
	var err error
	if flags&unix.O_CREAT != 0 {
		fd, err = unix.Openat(int(dir.Fd()), name, flags|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
		created = err == nil
		if err == unix.EEXIST && flags&unix.O_EXCL == 0 {
			fd, err = unix.Openat(int(dir.Fd()), name, (flags&^unix.O_CREAT)|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		}
	} else {
		fd, err = unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	}
	if err != nil {
		return nil, errRefused
	}
	if created && unix.Fchmod(fd, 0600) != nil {
		unix.Close(fd)
		return nil, errRefused
	}
	if !secureStat(fd, false) {
		unix.Close(fd)
		return nil, errRefused
	}
	return os.NewFile(uintptr(fd), name), nil
}

// withRegistry serializes both readers and writers with a nonblocking lock.
// It never follows a registry/lock symlink or repairs unsafe ownership/modes.
func withRegistry(home string, create bool, fn func(*os.File, registry) error) error {
	return registryLocked(home, create, false, fn)
}

func registryLocked(home string, create, shared bool, fn func(*os.File, registry) error) error {
	dir, err := openDirectory(home, create)
	if err != nil {
		return err
	}
	defer dir.Close()
	flags := unix.O_RDWR
	if create {
		flags |= unix.O_CREAT
	}
	lock, err := privateFile(dir, ".stations.lock", flags)
	if err != nil {
		return err
	}
	defer lock.Close()
	mode := unix.LOCK_EX
	if shared {
		mode = unix.LOCK_SH
	}
	if unix.Flock(int(lock.Fd()), mode|unix.LOCK_NB) != nil {
		return errRefused
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	r := registry{}
	var st unix.Stat_t
	err = unix.Fstatat(int(dir.Fd()), "stations.json", &st, unix.AT_SYMLINK_NOFOLLOW)
	if err == unix.ENOENT && create {
		return fn(dir, r)
	}
	if err != nil {
		return errRefused
	}
	f, err := privateFile(dir, "stations.json", unix.O_RDONLY)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRegistryBytes+1))
	f.Close()
	if err != nil || len(data) > maxRegistryBytes {
		return errRefused
	}
	r, err = decodeRegistry(data)
	if err != nil {
		return err
	}
	return fn(dir, r)
}

func decodeRegistry(data []byte) (registry, error) {
	entries, err := object(data, nil)
	if err != nil || len(entries) > maxStations {
		return nil, errRefused
	}
	r := registry{}
	for station, raw := range entries {
		if !localName.MatchString(station) {
			return nil, errRefused
		}
		fields, err := object(raw, map[string]bool{"identity": true, "uid": true, "secret": true})
		if err != nil || len(fields) != 3 {
			return nil, errRefused
		}
		var e Entry
		var secret string
		if json.Unmarshal(fields["identity"], &e.Identity) != nil || ValidateIdentity(e.Identity) != nil {
			return nil, errRefused
		}
		if bytes.Equal(bytes.TrimSpace(fields["uid"]), []byte("null")) || json.Unmarshal(fields["uid"], &e.UID) != nil {
			return nil, errRefused
		}
		if json.Unmarshal(fields["secret"], &secret) != nil {
			return nil, errRefused
		}
		e.Secret, err = base64.StdEncoding.Strict().DecodeString(secret)
		if err != nil || len(e.Secret) != StationKeyLen || base64.StdEncoding.EncodeToString(e.Secret) != secret {
			return nil, errRefused
		}
		r[station] = e
	}
	return r, nil
}

func saveRegistry(dir *os.File, r registry) error {
	data, err := json.Marshal(r)
	if err != nil || len(data) > maxRegistryBytes {
		return errRefused
	}
	// Random exclusive temporary name, always under the held directory handle.
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return errRefused
	}
	name := ".stations-" + base64.RawURLEncoding.EncodeToString(random[:])
	f, err := privateFile(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(int(dir.Fd()), name, 0)
	if err = writeAll(f, append(data, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errRefused
	}
	if unix.Renameat(int(dir.Fd()), name, int(dir.Fd()), "stations.json") != nil || dir.Sync() != nil {
		return errRefused
	}
	return nil
}

// Register creates a station binding, or rotates an existing one only when
// explicitly requested. Only its caller may deliberately
// send the returned secret into an operator-controlled enrollment pipe.
func Register(home, station, identity string, uid uint32, rotate bool) ([]byte, error) {
	if !localName.MatchString(station) || ValidateIdentity(identity) != nil {
		return nil, errRefused
	}
	var secret [StationKeyLen]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, errRefused
	}
	err := withRegistry(home, true, func(dir *os.File, r registry) error {
		_, exists := r[station]
		if exists && !rotate {
			return errRefused
		}
		if !exists && len(r) >= maxStations {
			return errRefused
		}
		r[station] = Entry{identity, uid, secret[:]}
		return saveRegistry(dir, r)
	})
	if err != nil {
		return nil, err
	}
	return secret[:], nil
}

// Unregister removes the binding, including its verifier material.
func Unregister(home, station string) error {
	if !localName.MatchString(station) {
		return errRefused
	}
	return withRegistry(home, false, func(dir *os.File, r registry) error {
		if _, exists := r[station]; !exists {
			return errRefused
		}
		delete(r, station)
		return saveRegistry(dir, r)
	})
}

func lookup(home, station string) (Entry, bool) {
	var entry Entry
	var found bool
	err := registryLocked(home, false, true, func(_ *os.File, r registry) error { entry, found = r[station]; return nil })
	return entry, found && err == nil
}
