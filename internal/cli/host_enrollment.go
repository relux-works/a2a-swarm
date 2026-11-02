package cli

import "github.com/relux-works/a2a-swarm/internal/config"

// The persisted admitted record selects Host. Environment labels and socket
// metadata never select it. Even an expired attachment keeps legacy fallback
// closed until explicit source-owned reconciliation/handover exists.
func (a *App) hostBackendSelected() (bool, error) {
	name, err := a.identity()
	if err != nil {
		return false, err
	}
	p, err := a.pathsFor(name)
	if err != nil {
		return false, err
	}
	st, err := config.LoadState(p, name)
	if err != nil {
		return false, err
	}
	return st.HostEnrollment != nil, nil
}
