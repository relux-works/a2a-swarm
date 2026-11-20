package config

import "os"

// FileStamp identifies one observed version of a path without reading it:
// presence, device/inode identity, size, nanosecond modification time and mode.
// Consumers may reuse decoded data for an equal stamp; equality never replaces
// authority checks, writer-seal verification or locking.
type FileStamp struct {
	Known   bool // false: the platform cannot identify files; never equal
	Present bool
	Dev     uint64
	Ino     uint64
	Size    int64
	ModNS   int64
	Mode    os.FileMode
}

// Equal reports whether two stamps name the same observed version. An unknown
// stamp equals nothing, so callers treat it as a change.
func (s FileStamp) Equal(o FileStamp) bool {
	return s.Known && o.Known && s == o
}

// StampFile observes path without following a final symlink. A missing path is
// a known, absent stamp; any other error is unknown.
func StampFile(path string) FileStamp {
	return stampFile(path, os.Lstat)
}

// StampTarget observes the file path resolves to, following symlinks.
func StampTarget(path string) FileStamp {
	return stampFile(path, os.Stat)
}

func stampFile(path string, stat func(string) (os.FileInfo, error)) FileStamp {
	info, err := stat(path)
	if os.IsNotExist(err) {
		return FileStamp{Known: true}
	}
	if err != nil {
		return FileStamp{}
	}
	return stampInfo(info)
}

func stampInfo(info os.FileInfo) FileStamp {
	dev, ino, ok := fileIdentity(info)
	if !ok {
		return FileStamp{}
	}
	return FileStamp{Known: true, Present: true, Dev: dev, Ino: ino, Size: info.Size(), ModNS: info.ModTime().UnixNano(), Mode: info.Mode()}
}

// fileChangeTimeNS returns a timestamp value and whether its source is
// qualified as a namespace version for durability elision. Current platform
// stat timestamps are retained only as observations; none is qualified yet.
var fileChangeTimeNS = platformFileChangeTimeNS

// directoryStamp records the identity and change time of the directory that
// owns a State filename. File bytes and inode identity cannot prove that the
// directory entry has remained durable across an unobserved rename-away and
// rename-back. A timestamp value alone is not a qualified namespace version,
// so unqualified observations leave this unknown and disable the zero-sync
// own-write shortcut.
type directoryStamp struct {
	known    bool
	file     FileStamp
	changeNS int64
}

func (s directoryStamp) Equal(other directoryStamp) bool {
	return s.known && other.known && s.file.Equal(other.file) && s.changeNS == other.changeNS
}

func stampStateDirectory(path string) directoryStamp {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return directoryStamp{}
	}
	file := stampInfo(info)
	changeNS, ok := fileChangeTimeNS(info)
	if !file.Known || !ok {
		return directoryStamp{}
	}
	return directoryStamp{known: true, file: file, changeNS: changeNS}
}
