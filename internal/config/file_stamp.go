package config

import "os"

// FileStamp identifies one observed version of a path without reading it:
// presence, file identity, size, modification time and mode. Equal stamps are
// a wake hint only. They never stand in for a read, a guard or a lock.
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
	dev, ino, ok := fileIdentity(info)
	if !ok {
		return FileStamp{}
	}
	return FileStamp{Known: true, Present: true, Dev: dev, Ino: ino, Size: info.Size(), ModNS: info.ModTime().UnixNano(), Mode: info.Mode()}
}
