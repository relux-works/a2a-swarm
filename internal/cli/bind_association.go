package cli

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/relux-works/a2a-swarm/internal/association"
	"github.com/relux-works/a2a-swarm/internal/config"
)

// associationContext deliberately bypasses selectedConnection/pathsFor, whose
// legacy conflict check reads matrix.env. Only public routing metadata and the
// already selected own name are used; no credential metadata is opened.
func (a *App) associationContext() (association.Context, error) {
	bad := errors.New("association context unavailable")
	name, e := a.identity()
	if e != nil || a.human {
		return association.Context{}, bad
	}
	p, e := config.ResolvePaths(a.Getenv)
	if e != nil || !filepath.IsAbs(p.Base) {
		return association.Context{}, bad
	}
	p.Base = filepath.Clean(p.Base)
	out := association.Context{Name: name, Connection: "legacy", Base: p.Base, Root: p.Base}
	b, e := association.ReadMetadata(p.ConnectionsFile(), 1<<20)
	if errors.Is(e, os.ErrNotExist) {
		if a.profile != "" || a.connectionFingerprint != "" {
			return out, bad
		}
		return out, nil
	}
	if e != nil {
		return out, bad
	}
	var r config.ConnectionRegistry
	if config.DecodeConnectionJSON(b, &r) != nil || r.Validate() != nil {
		return out, bad
	}
	label := a.profile
	if label == "" {
		label = r.Current
	}
	c, ok := r.Profiles[label]
	if !ok {
		return out, bad
	}
	binding, e := c.Binding(name, false)
	if e != nil || a.connectionFingerprint != "" && binding.Fingerprint() != a.connectionFingerprint {
		return out, bad
	}
	// Paths validates connection directory custody without reading identity files.
	resolved, e := c.Paths(p, name, false)
	if e != nil {
		return out, bad
	}
	out.Connection = c.ID
	out.Base = resolved.Base
	out.Selection = binding.Fingerprint()
	return out, nil
}

func (a *App) bindAssociation(args []string) int {
	// Exactly the admitted inherited descriptor and JSON mode; no file path or
	// fallback transport. os.NewFile owns descriptor 3 and Run closes it on refusal.
	valid := len(args) == 2 && ((args[0] == "--private-intent-fd=3" && args[1] == "--json") || (args[1] == "--private-intent-fd=3" && args[0] == "--json"))
	var input *os.File
	for _, arg := range args {
		if arg == "--private-intent-fd=3" {
			input = os.NewFile(3, "association-intent")
			break
		}
	}
	return association.Run(association.Options{Context: a.context(), Input: input, Output: a.Stdout, Now: a.Now, Resolve: a.associationContext, InvalidArgs: !valid})
}
