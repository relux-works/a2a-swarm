package cli

import "github.com/relux-works/a2a-swarm/internal/config"

// monitorSink is an inert attachment seam for the later closed notice encoder.
// Raw watch/terminal/native paths have neither a constructor nor a live handle
// for it. No byte/event/error input is accepted here; later wiring must supply
// only the accepted closed projection. This leaf registers no notice command.
type monitorSink struct {
	generation *config.MonitorGeneration
	id         string
	emit       func() error
}

func admitMonitorSink(g *config.MonitorGeneration, id string, emit func() error) (*monitorSink, error) {
	if emit == nil {
		return nil, config.ErrMonitorGeneration
	}
	if err := g.CheckRenewal(id); err != nil {
		return nil, err
	}
	return &monitorSink{generation: g, id: id, emit: emit}, nil
}

func (s *monitorSink) announce() error {
	if s == nil {
		return config.ErrMonitorGeneration
	}
	return s.generation.Announce(s.id, s.emit)
}
