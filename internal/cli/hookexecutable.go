package cli

import "path/filepath"

type hookExecutableChoice struct {
	binary, installed       string
	binarySet, installedSet bool
}

func (c hookExecutableChoice) validate() error {
	if c.binarySet && c.installedSet {
		return usagef("choose only one of --executable and --installed-command")
	}
	if c.binarySet && !filepath.IsAbs(c.binary) || c.installedSet && !filepath.IsAbs(c.installed) {
		return usagef("executable selection requires a non-empty absolute path")
	}
	return nil
}

type hookExecutableSelection struct {
	path    string
	aliases []string // Observed spellings for companion owner-DM rules only.
	check   func() error
}

func (a *App) selectHookExecutable(c hookExecutableChoice) (hookExecutableSelection, error) {
	if a.hookSelection != nil {
		return a.hookSelection(c)
	}
	return selectHookExecutable(c)
}
