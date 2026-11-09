package config

import (
	"os"
	"path/filepath"
	"time"
)

func postAttemptPath(p Paths, name string, human bool) string {
	class := "agent"
	if human {
		class = "human"
	}
	return filepath.Join(p.Base, "state", name+".post-"+class+"-attempt.lock")
}

// AcquirePostAttempt excludes migration for the full prepare/reserve/send/ACK
// lifetime, including remote I/O. Journal flocks remain short lived. The stable
// inode is never removed. A reserved UNKNOWN can move only after this release.
func AcquirePostAttempt(p Paths, name string, human bool, deadline time.Time) (func(), error) {
	if ValidateName(name) != nil {
		return nil, ErrPostOperation
	}
	if err := os.MkdirAll(filepath.Join(p.Base, "state"), 0700); err != nil {
		return nil, err
	}
	release, err := lockFileBy(postAttemptPath(p, name, human), deadline)
	if err != nil {
		return nil, err
	}
	if err := CheckIdentityMigration(p, name); err != nil {
		release()
		return nil, err
	}
	return release, nil
}
