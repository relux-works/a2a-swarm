package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

var ErrHostStateScope = errors.New("host state namespace unavailable")
var ErrHostStateCapacity = errors.New("host state size limit exceeded")
var ErrHostStatePersistence = errors.New("host state persistence failed")
var ErrHostStateUncertain = fmt.Errorf("%w: outcome uncertain", ErrHostStatePersistence)

// This is the source State budget, distinct from Host's own journal/wire budget.
const hostStateMaxBytes = 16 << 20

type hostStateOps struct {
	sync       func(*os.File) error
	checkpoint func(string) error
}

func writeHostStateAtomic(p Paths, name string, data []byte) error {
	return writeHostStateWith(p, name, data, hostStateOps{})
}

// The admitted namespace already exists. Pin its directories before mutation;
// all temporary/target operations use that opened state root. No second journal
// or receipt is written. Callers must hold the existing state serialization and
// must not begin an effect unless this entire write returns success.
func writeHostStateWith(p Paths, name string, data []byte, ops hostStateOps) error {
	if ValidateName(name) != nil || p.Base == "" || len(data) == 0 {
		return ErrHostStateScope
	}
	if len(data) > hostStateMaxBytes {
		return ErrHostStateCapacity
	}
	if ops.sync == nil {
		ops.sync = (*os.File).Sync
	}
	base, err := os.OpenRoot(p.Base)
	if err != nil {
		return ErrHostStateScope
	}
	defer base.Close()
	if err := base.Mkdir("state", 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return ErrHostStateScope
	}
	info, err := base.Lstat("state")
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return ErrHostStateScope
	}
	root, err := base.OpenRoot("state")
	if err != nil {
		return ErrHostStateScope
	}
	defer root.Close()
	target := name + ".json"
	if info, err := root.Lstat(target); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != SecretFileMode {
			return ErrHostStateScope
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return ErrHostStateScope
	}
	renamed := false
	failure := func() error {
		if renamed {
			return ErrHostStateUncertain
		}
		return ErrHostStatePersistence
	}
	checkpoint := func(at string) error {
		if ops.checkpoint != nil && ops.checkpoint(at) != nil {
			return failure()
		}
		return nil
	}
	if err := checkpoint("directory-opened"); err != nil {
		return err
	}
	// Persist the state-directory entry in the admitted configuration root too.
	parent, err := base.Open(".")
	if err != nil {
		return failure()
	}
	syncErr, closeErr := ops.sync(parent), parent.Close()
	if syncErr != nil || closeErr != nil {
		return failure()
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return failure()
	}
	temporary := "." + name + ".host-" + hex.EncodeToString(nonce[:]) + ".tmp"
	f, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, SecretFileMode)
	if err != nil {
		return failure()
	}
	defer func() {
		f.Close()
		if !renamed {
			_ = root.Remove(temporary)
		}
	}()
	if err := f.Chmod(SecretFileMode); err != nil {
		return failure()
	}
	if err := checkpoint("temp-created"); err != nil {
		return err
	}
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return failure()
	}
	if err := checkpoint("file-written"); err != nil {
		return err
	}
	if err := ops.sync(f); err != nil {
		return failure()
	}
	if err := checkpoint("file-synced"); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return failure()
	}
	if err := checkpoint("file-closed"); err != nil {
		return err
	}
	if err := root.Rename(temporary, target); err != nil {
		return failure()
	}
	renamed = true
	if err := checkpoint("renamed"); err != nil {
		return err
	}
	directory, err := root.Open(".")
	if err != nil {
		return failure()
	}
	syncErr, closeErr = ops.sync(directory), directory.Close()
	if syncErr != nil || closeErr != nil {
		return failure()
	}
	return checkpoint("directory-synced")
}
