//go:build n1_fixture && darwin

package cli

import (
	"context"
	"os"
	"os/exec"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/n1fixture"
)

func n1Rows(ctx context.Context) ([]bindingProcess, error) {
	rows, e := n1fixture.Enumerate()
	if e != nil {
		return nil, e
	}
	out := make([]bindingProcess, 0, len(rows))
	for _, r := range rows {
		out = append(out, bindingProcess{PID: r.PID, PPID: r.PPID, UID: r.UID, Name: r.Name})
	}
	return out, nil
}
func n1AfterLoad(ctx context.Context, st config.State) error {
	if os.Getenv("N1_ROLE") != "replacement" {
		return nil
	}
	if e := n1fixture.Write("loaded.json", st.Terminal); e != nil {
		return e
	}
	return n1fixture.Latch(ctx, "replacement-load")
}
func n1BeforeReport(ctx context.Context, generation string) error {
	role := os.Getenv("N1_ROLE")
	if role == "prior-consumer" {
		return n1fixture.Latch(ctx, "prior-report")
	}
	if role == "new-consumer" {
		return n1fixture.Latch(ctx, "new-report")
	}
	return nil
}
func n1AfterReport(ctx context.Context, generation string) error {
	role := os.Getenv("N1_ROLE")
	if role == "prior-consumer" || role == "new-consumer" {
		return n1fixture.Latch(ctx, role+"-reported")
	}
	return nil
}

// The explicit test supervisor owns the same child Start/Wait as bindTerminal.
// The wrapper changes only test-binary dispatch, env instrumentation and receipts.
func n1ConsumerCommand(exe string, args []string, generation string) *exec.Cmd {
	cmd := exec.Command(exe, append([]string{"-test.run=^TestN1Role$", "--"}, args...)...)
	return cmd
}
func n1ConsumerEnv(env []string, generation string) []string {
	role := "new-consumer"
	if os.Getenv("N1_ROLE") == "initial" {
		role = "prior-consumer"
	}
	return append(env, "N1_DIR="+n1fixture.Dir(), "N1_NONCE="+n1fixture.Nonce(), "N1_ROLE="+role, "N1_GENERATION="+generation)
}
func n1Started(cmd *exec.Cmd, generation string) error {
	// Child.Start is replaced as a whole by n1fixture.Start: it registers Wait and
	// failure cleanup before returning, including failed identity sealing.
	nonce := ""
	for _, v := range cmd.Env {
		if len(v) > 9 && v[:9] == "N1_NONCE=" {
			nonce = v[9:]
		}
	}
	_, e := n1fixture.Start(cmd, nonce, generation, "consumer")
	return e
}
func n1BoundedContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 60*time.Second)
}
