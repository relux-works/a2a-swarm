//go:build linux || darwin

package daemonenroll

import (
	"os"
	"path/filepath"

	"github.com/relux-works/a2a-swarm/internal/daemon"
)

// Enrolled is one pinned, verified generation. The caller must clear Key after use.
type Enrolled struct {
	Socket, Identity, Station string
	Key                       []byte
}

// Read distinguishes absence from any invalid or partial enrollment. Reader
// rule (b) refuses aliases; it never adopts a newly canonicalized namespace.
func Read(dir string) (Enrolled, bool, error) {
	var e Enrolled
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return e, false, nil
	}
	if filepath.Base(dir) != ".a2a" || !canonicalReaderPath(dir) {
		return e, true, errFiles
	}
	chain, err := openParent(filepath.Dir(dir))
	if err != nil {
		return e, true, errFiles
	}
	defer chain.close()
	parent := chain.fds[len(chain.fds)-1]
	f, err := openDir(parent, ".a2a")
	if err != nil {
		return e, true, errFiles
	}
	defer f.Close()
	data, err := snapshot(f, nil)
	if err != nil {
		return e, true, err
	}
	if !chain.valid() || !sameEntry(parent, ".a2a", f) {
		clear(data[2])
		return e, true, errFiles
	}
	e.Socket, _ = daemon.ParsePointerFile(data[0])
	e.Identity, _ = daemon.ParseIdentityFile(data[1])
	e.Station, _ = daemon.ParseIdentityFile(data[3])
	e.Key = data[2]
	return e, true, nil
}
