package cli

import (
	"context"
	"fmt"

	"github.com/relux-works/a2a-swarm/internal/daemonenroll"
)

func cmdDaemonEnroll(_ context.Context, a *App, args []string) error {
	fs := newFlagSet("daemon enroll")
	dir := fs.String("dir", "", "station home enrollment directory")
	from := fs.String("from", "", "read enrollment from stdin (-)")
	rotate := fs.Bool("rotate", false, "replace an existing complete enrollment")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *dir == "" || *from != "-" {
		return usagef("usage: daemon enroll --dir HOME/.a2a --from - [--rotate]")
	}
	if err := daemonenroll.Install(*dir, a.Stdin, *rotate); err != nil {
		return err
	}
	_, err := fmt.Fprintln(a.Stdout, "Enrollment installed.")
	return err
}
