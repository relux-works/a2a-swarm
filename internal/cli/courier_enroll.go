package cli

import (
	"context"
	"fmt"

	"github.com/relux-works/a2a-swarm/internal/courierenroll"
)

func cmdCourierEnroll(_ context.Context, a *App, args []string) error {
	fs := newFlagSet("courier enroll")
	dir := fs.String("dir", "", "station home enrollment directory")
	from := fs.String("from", "", "read enrollment from stdin (-)")
	rotate := fs.Bool("rotate", false, "replace an existing complete enrollment")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *dir == "" || *from != "-" {
		return usagef("usage: courier enroll --dir HOME/.a2a --from - [--rotate]")
	}
	if err := courierenroll.Install(*dir, a.Stdin, *rotate); err != nil {
		return err
	}
	_, err := fmt.Fprintln(a.Stdout, "Enrollment installed.")
	return err
}
