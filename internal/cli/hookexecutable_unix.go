//go:build darwin || linux

package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Selection is an explicit custody choice, not a trust inference from a path.
// Reuse the native-image validation used by terminal executable refresh.
func selectHookExecutable(c hookExecutableChoice) (hookExecutableSelection, error) {
	if err := c.validate(); err != nil {
		return hookExecutableSelection{}, err
	}
	p := c.binary
	if c.installedSet {
		p = c.installed
	} else if !c.binarySet {
		var err error
		p, err = os.Executable()
		if err != nil {
			return hookExecutableSelection{}, fmt.Errorf("resolve running executable: %w", err)
		}
	}
	p = filepath.Clean(p)
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return hookExecutableSelection{}, fmt.Errorf("hook executable is missing or unresolved; run a complete installed image and select its absolute path")
	}
	var linkStamp terminalExecutable
	custodyPaths := []string{p, resolved}
	if c.installedSet {
		st, err := os.Lstat(p)
		if err != nil || st.Mode()&os.ModeSymlink == 0 {
			return hookExecutableSelection{}, fmt.Errorf("--installed-command requires an explicitly selected installed symlink")
		}
		uid := st.Sys().(*syscall.Stat_t).Uid
		if uid != 0 && uid != uint32(os.Getuid()) {
			return hookExecutableSelection{}, fmt.Errorf("installed command has foreign ownership")
		}
		linkStamp = executableMetadata(st)
		// A single link to the canonical image keeps every selected piece of
		// custody inspectable; do not adopt an uninspected chain of aliases.
		target, err := os.Readlink(p)
		if err != nil {
			return hookExecutableSelection{}, fmt.Errorf("installed command target is unavailable")
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		// Resolve directory aliases (/var -> /private/var on macOS), but
		// never resolve a second link in the target's final component.
		parent, err := filepath.EvalSymlinks(filepath.Dir(target))
		direct := filepath.Join(parent, filepath.Base(target))
		if err != nil || direct != resolved {
			return hookExecutableSelection{}, fmt.Errorf("installed command must link directly to the canonical image; symlink chains are not supported")
		}
		info, err := os.Lstat(direct)
		if err != nil || !info.Mode().IsRegular() {
			return hookExecutableSelection{}, fmt.Errorf("installed command must link directly to a regular image; symlink chains are not supported")
		}
		custodyPaths = append(custodyPaths, target)
	}
	if err := hookExecutableParents(custodyPaths...); err != nil {
		return hookExecutableSelection{}, err
	}
	stamp, err := observeTerminalExecutable(resolved)
	if err != nil {
		return hookExecutableSelection{}, fmt.Errorf("hook executable must be a complete own-UID native executable with owner execute permission and no group/world write or set-ID bits")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	loaded, err := bindingImage(ctx, os.Getpid())
	if err != nil || loaded != stamp.Image {
		return hookExecutableSelection{}, fmt.Errorf("hook executable does not match the running image; invoke the selected binary before installing")
	}
	command := resolved
	if c.installedSet {
		command = p
	}
	aliases := []string{p, resolved}
	// This only broadens companion denials; it never selects an executable.
	if len(os.Args) > 0 && filepath.IsAbs(os.Args[0]) {
		if launch, err := filepath.EvalSymlinks(os.Args[0]); err == nil && launch == resolved {
			aliases = append(aliases, os.Args[0])
		}
	}
	check := func() error {
		current, err := filepath.EvalSymlinks(p)
		if err != nil || current != resolved {
			return fmt.Errorf("hook executable target changed; refusing to write configuration")
		}
		if c.installedSet {
			st, err := os.Lstat(p)
			if err != nil || executableMetadata(st) != linkStamp {
				return fmt.Errorf("installed command changed; refusing to write configuration")
			}
			target := custodyPaths[len(custodyPaths)-1]
			parent, err := filepath.EvalSymlinks(filepath.Dir(target))
			if err != nil || filepath.Join(parent, filepath.Base(target)) != resolved {
				return fmt.Errorf("installed command no longer links directly to the image; refusing to write configuration")
			}
		}
		if err := hookExecutableParents(custodyPaths...); err != nil {
			return err
		}
		currentStamp, err := observeTerminalExecutable(resolved)
		if err != nil || currentStamp != stamp {
			return fmt.Errorf("hook executable image changed; refusing to write configuration")
		}
		return nil
	}
	return hookExecutableSelection{path: command, aliases: aliases, check: check}, nil
}

func hookExecutableParents(paths ...string) error {
	for _, path := range paths {
		parent, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			return fmt.Errorf("hook executable directory is unavailable")
		}
		if parent != filepath.Dir(path) {
			if err := hookExecutableParents(filepath.Join(parent, filepath.Base(path))); err != nil {
				return err
			}
		}
		for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
			if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil || !os.IsNotExist(err) {
				return fmt.Errorf("hook executable is in a mutable source checkout or its custody cannot be checked; install a separate image first (no checkout override)")
			}
			info, err := os.Stat(dir)
			if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
				return fmt.Errorf("hook executable directory is writable by other users or unavailable")
			}
			uid := info.Sys().(*syscall.Stat_t).Uid
			if uid != 0 && uid != uint32(os.Getuid()) {
				return fmt.Errorf("hook executable directory has foreign ownership")
			}
			if dir == filepath.Dir(dir) {
				break
			}
		}
	}
	return nil
}
