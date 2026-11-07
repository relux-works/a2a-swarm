package voicetranscribe

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var errUnsafe = errors.New("unsafe local ASR artifact")

// Walk with held directory descriptors. No symlink component is accepted. A
// writable ancestor is accepted only with sticky protection; the leaf TMPDIR
// must be owned by euid and exactly 0700. Same-UID interference is out of scope.
func openDirectory(path string, private bool) (*os.Root, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 4096 || strings.ContainsRune(path, 0) {
		return nil, errUnsafe
	}
	root, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	if path == string(filepath.Separator) {
		parts = nil
	}
	for _, part := range parts {
		before, err := root.Lstat(part)
		if err != nil {
			root.Close()
			return nil, err
		}
		if !safeDirectory(before, false) {
			root.Close()
			return nil, errUnsafe
		}
		next, err := root.OpenRoot(part)
		if err != nil {
			root.Close()
			return nil, err
		}
		opened, err := next.Stat(".")
		root.Close()
		if err != nil || !os.SameFile(before, opened) {
			next.Close()
			return nil, errUnsafe
		}
		root = next
	}
	st, err := root.Stat(".")
	if err != nil || !safeDirectory(st, private) {
		root.Close()
		return nil, errUnsafe
	}
	return root, nil
}

func makeOperation(root *os.Root) (*os.Root, string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, "", err
	}
	name := "a2a-stt-" + hex.EncodeToString(nonce[:])
	if err := root.Mkdir(name, 0700); err != nil {
		return nil, "", err
	}
	op, err := root.OpenRoot(name)
	if err != nil {
		root.RemoveAll(name)
		return nil, "", err
	}
	st, err := op.Stat(".")
	if err != nil || !safeDirectory(st, true) {
		op.Close()
		root.RemoveAll(name)
		return nil, "", errUnsafe
	}
	return op, name, nil
}

func writeInput(op *os.Root, audio []byte) error {
	f, err := op.OpenFile("input.wav", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	n, writeErr := f.Write(audio)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil || n != len(audio) {
		return errUnsafe
	}
	return nil
}

func sameFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && a.Mode().IsRegular() && b.Mode().IsRegular() &&
		os.SameFile(a, b) && a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime() == b.ModTime()
}

// Copy from an opened regular, no-follow, non-blocking descriptor. Exact size
// and SHA authenticate the copied bytes; pathname/descriptor checks also catch
// replacement during the copy. Nothing executes from the source installation.
func snapshotFile(ctx context.Context, source, op *os.Root, a Artifact) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if a.Bytes <= 0 || a.Bytes > 96<<20 || len(a.SHA256) != 64 {
		return errUnsafe
	}
	parts := strings.Split(a.Path, "/")
	if len(parts) != 2 || (parts[0] != "bin" && parts[0] != "lib" && parts[0] != "model") ||
		parts[1] == "" || parts[1] == "." || parts[1] == ".." || strings.ContainsAny(parts[1], "\\\x00") {
		return errUnsafe
	}
	dirInfo, err := source.Lstat(parts[0])
	if err != nil {
		return err
	}
	if !safeDirectory(dirInfo, false) {
		return errUnsafe
	}
	dir, err := source.OpenRoot(parts[0])
	if err != nil {
		return err
	}
	defer dir.Close()
	st, err := dir.Stat(".")
	if err != nil || !os.SameFile(dirInfo, st) {
		return errUnsafe
	}
	before, err := dir.Lstat(parts[1])
	if err != nil {
		return err
	}
	if !safeArtifact(before) || before.Size() != a.Bytes {
		return errUnsafe
	}
	in, err := openReadOnly(dir, parts[1])
	if err != nil {
		return err
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil || !sameFile(before, opened) {
		return errUnsafe
	}
	out, err := op.OpenFile(a.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	h := sha256.New()
	buf := make([]byte, 64<<10)
	var n int64
	var copyErr error
	for {
		if ctx.Err() != nil {
			copyErr = ctx.Err()
			break
		}
		read, readErr := in.Read(buf)
		n += int64(read)
		if n > a.Bytes {
			copyErr = errUnsafe
			break
		}
		if read > 0 {
			if _, err := io.MultiWriter(h, out).Write(buf[:read]); err != nil {
				copyErr = err
				break
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			copyErr = readErr
			break
		}
	}
	mode := os.FileMode(0400)
	if parts[0] == "bin" {
		mode = 0500
	}
	modeErr := out.Chmod(mode)
	closeErr := out.Close()
	after, statErr := in.Stat()
	pathAfter, pathErr := dir.Lstat(parts[1])
	if copyErr != nil || modeErr != nil || closeErr != nil || statErr != nil || pathErr != nil ||
		n != a.Bytes || hex.EncodeToString(h.Sum(nil)) != a.SHA256 ||
		!sameFile(opened, after) || !sameFile(after, pathAfter) {
		return errUnsafe
	}
	return nil
}
