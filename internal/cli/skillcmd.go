package cli

import (
	"context"
	"io/fs"
	"os"

	"github.com/relux-works/a2a-swarm/internal/skill"
)

// cmdSkill: a2a skill install [--force] | a2a skill print
func cmdSkill(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 {
		return usagef("usage: a2a skill install [--force] | a2a skill print")
	}
	switch args[0] {
	case "print":
		if len(args) != 1 {
			return usagef("usage: a2a skill print")
		}
		data, err := fs.ReadFile(skill.Files(), "SKILL.md")
		if err != nil {
			return err
		}
		a.printf("%s", data)
		return nil
	case "install":
		fs := newFlagSet("skill install")
		force := fs.Bool("force", false, "replace a foreign a2a skill")
		pos, err := parseInterspersed(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 0 {
			return usagef("usage: a2a skill install [--force]")
		}
		home := a.Getenv("HOME")
		if home == "" {
			if home, err = os.UserHomeDir(); err != nil {
				return err
			}
		}
		r, err := skill.Install(home, *force)
		if err != nil {
			return err
		}
		a.printf("installed %s\n", r.Copied)
		for _, l := range r.Linked {
			a.printf("linked %s\n", l)
		}
		return nil
	}
	return usagef("usage: a2a skill install [--force] | a2a skill print")
}
