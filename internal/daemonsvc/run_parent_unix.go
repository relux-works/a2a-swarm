//go:build darwin || linux

package daemonsvc

import (
	"os"

	"golang.org/x/sys/unix"
)

const (
	darwinRunParent                  = "/private/var/run"
	linuxRunParent                   = "/run"
	systemRootUID             uint32 = 0
	wheelGID                  uint32 = 0
	daemonGID                 uint32 = 1
	anyReadOnlyGID            uint32 = ^uint32(0)
	runParentReadOnly         uint32 = 0755
	runParentSystemGroupWrite uint32 = 0775
)

type runParentPolicy struct {
	platform, path string
	gids           [2]uint32
	modes          [2]uint32
}

// Closed platform table. Source's macOS 27.2/26B123 observation is
// root:daemon 0775, no ACL. Linux retains the previous uid0/0755 rule;
// its group ID is immaterial because group write is forbidden there.
var runParentPolicies = [...]runParentPolicy{
	{"darwin", darwinRunParent, [2]uint32{wheelGID, daemonGID}, [2]uint32{runParentReadOnly, runParentSystemGroupWrite}},
	{"linux", linuxRunParent, [2]uint32{anyReadOnlyGID, anyReadOnlyGID}, [2]uint32{runParentReadOnly, runParentReadOnly}},
}

func runParentPolicyFor(platform string) (runParentPolicy, bool) {
	for _, p := range runParentPolicies {
		if p.platform == platform {
			return p, true
		}
	}
	return runParentPolicy{}, false
}

// Only prepare uses this exception. The public entry point fixes the path to
// the table's parent; private fixtures substitute an owned physical directory.
func (d disk) runParent(path string, p runParentPolicy) (*os.File, error) {
	f, err := d.directory(path)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	err = d.stat(int(f.Fd()), &st)
	mode := uint32(st.Mode) & 07777
	groupOK := p.gids[0] == anyReadOnlyGID || st.Gid == p.gids[0] || st.Gid == p.gids[1]
	if err != nil || st.Uid != systemRootUID || !groupOK || mode&0002 != 0 || (mode != p.modes[0] && mode != p.modes[1]) || !d.aclSafe(int(f.Fd())) || d.rootAncestors(path) != nil {
		f.Close()
		return nil, ErrRefused
	}
	return f, nil
}
