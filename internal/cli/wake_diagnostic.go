package cli

import (
	"time"

	"github.com/relux-works/a2a-swarm/internal/codexwake"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/wakediag"
)

// Called only with an actual Wake error, in the existing outcome transaction.
// A changed binding/attempt may keep its existing outcome behavior but must not
// receive another scope's historical diagnostic.
func recordNativeWakeDiagnostic(st *config.State, identity string, expected codexwake.Binding, attempt string, kind wakediag.Kind, at time.Time) {
	if st.Runtime == nil || st.Runtime.Identity != identity || st.Runtime.Retired || st.Wake.Attempt == nil || st.Wake.Attempt.ID != attempt || st.HooksPaused || manualWakeGate(*st) != "" {
		return
	}
	current := *st.Runtime
	current.WakeDiagnostic, expected.WakeDiagnostic = nil, nil
	if current != expected {
		return
	}
	st.Runtime.WakeDiagnostic = wakediag.Retain(st.Runtime.WakeDiagnostic, kind, at)
}

func nativeWakeDiagnostic(b *codexwake.Binding, identity string) *wakediag.Record {
	if b == nil || b.Identity != identity {
		return nil
	}
	return wakediag.Project(b.WakeDiagnostic, wakediag.NativeDefiniteRefusal, wakediag.NativeTransportUncertain)
}

func (a *App) printWakeDiagnostic(r *wakediag.Record) {
	if r == nil {
		return
	}
	explanation := ""
	switch r.Kind {
	case wakediag.TerminalLayoutRefused:
		explanation = "terminal readiness refused an unrecognized layout before Send with pending mail"
	case wakediag.NativeDefiniteRefusal:
		explanation = "native Wake explicitly refused acceptance"
	case wakediag.NativeTransportUncertain:
		explanation = "native Wake transport result was uncertain; acceptance is unknown"
	}
	a.printf("wake_diagnostic=%s at=%s; retained observation: %s\n", r.Kind, r.At.Format(time.RFC3339Nano), explanation)
}
