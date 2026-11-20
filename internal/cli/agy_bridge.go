package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

const agyQualifiedModel = "gemini-4-neon-xhigh"

func agyUserFrame(notice string) any {
	return map[string]any{"event": "user", "message": map[string]string{"content": notice}}
}

// An ordinary launcher owns the child lifetime and the only writer to its stdin.
// It submits the initial admitted turn, then hands the paired anonymous pipes
// to this adapter. No file path, terminal, process lookup or attach fallback.
func (a *App) agyBridge(ctx context.Context, sid, model string) error {
	if a.as == "" || a.Getenv("A2A_AGENT") != a.as || a.Getenv(EnvSessionID) != sid || !agyUUID.MatchString(sid) || model != agyQualifiedModel {
		return errors.New("agy bridge requires the selected conversation and qualified model")
	}
	if _, err := a.agyIdentity(sid); err != nil {
		return err
	}
	input, ok := a.Stdin.(*os.File)
	output, outOK := a.Stdout.(*os.File)
	if !ok || !outOK || !agyPipe(input) || !agyPipe(output) {
		return errors.New("agy bridge requires dedicated provider pipes")
	}
	reader, restoreInput, err := agyBridgePipe(input)
	if err != nil {
		return err
	}
	defer restoreInput()
	writer, restoreOutput, err := agyBridgePipe(output)
	if err != nil {
		return err
	}
	defer restoreOutput()
	a.Stdout = writer
	defer func() { a.Stdout = output }()
	p, err := a.pathsFor(a.as)
	if err != nil {
		return err
	}
	return a.withRuntimeLock(p, a.as, "consumer", time.Now().Add(100*time.Millisecond), func() error {
		if _, err := a.agyAdmission(sid, true); err != nil {
			return fmt.Errorf("agy bridge startup custody: %w", err)
		}
		return a.runAgyBridge(ctx, reader, sid, model, 5*time.Second)
	})
}
func agyPipe(f *os.File) bool {
	info, err := f.Stat()
	// Both anonymous pipes and FIFOs have this type. Path custody and exclusive
	// descriptor handoff belong to the calling launcher.
	return err == nil && info.Mode()&os.ModeNamedPipe != 0
}

type agyObservation struct {
	Event        string
	Conversation string `json:"conversation_id"`
	Init         struct{ Model string }
	Result       struct {
		Conversation string `json:"conversation_id"`
		Status       string
	}
}

// Native field names are case-sensitive. Ignore unrelated payload values;
// reject ambiguous duplicates before interpreting any control observation.
func (f *agyObservation) decode(data []byte) error {
	invalid := errors.New("invalid agy frame")
	var root map[string]json.RawMessage
	if uniqueHookObject(data) != nil || json.Unmarshal(data, &root) != nil || json.Unmarshal(root["event"], &f.Event) != nil || f.Event == "" {
		return invalid
	}
	if raw, ok := root["conversation_id"]; ok && (json.Unmarshal(raw, &f.Conversation) != nil || f.Conversation == "") {
		return invalid
	}
	switch f.Event {
	case "error":
		return errors.New("agy provider reported an error")
	case "init":
		var init map[string]json.RawMessage
		if json.Unmarshal(root["init"], &init) != nil || json.Unmarshal(init["model"], &f.Init.Model) != nil {
			return invalid
		}
	case "result":
		var result map[string]json.RawMessage
		if json.Unmarshal(root["result"], &result) != nil || json.Unmarshal(result["conversation_id"], &f.Result.Conversation) != nil || json.Unmarshal(result["status"], &f.Result.Status) != nil {
			return invalid
		}
	}
	return nil
}

// The reader observes the stream throughout Matrix I/O. The same mutex
// linearizes invalidation and a single nonblocking, pipe-atomic write. A
// frame already observed while idle can never become the next turn's result.
type agyGate struct {
	mu                        sync.Mutex
	initialized, idle, closed bool
	err                       error
	ctx                       context.Context
	cancel                    context.CancelFunc
	write                     func([]byte) (int, error)
}

func (g *agyGate) finishLocked(err error) {
	if g.closed {
		return
	}
	g.closed = true
	g.err = err
	if g.cancel != nil {
		g.cancel()
	}
}

func (g *agyGate) observe(f agyObservation, sid, model string) (ready bool, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case !g.initialized:
		if f.Event != "init" || f.Conversation != sid || f.Init.Model != model {
			err = errors.New("agy init binding mismatch")
		} else {
			g.initialized = true
		}
	case g.idle:
		err = errors.New("unexpected activity after agy idle result")
	case f.Event == "init" || f.Conversation != "" && f.Conversation != sid:
		err = errors.New("agy stream binding changed")
	case f.Event == "result":
		if f.Result.Conversation != sid || f.Result.Status != "SUCCESS" {
			err = errors.New("agy result did not confirm a successful turn in this conversation")
		} else {
			g.idle = true
			ready = true
		}
	}
	if err != nil {
		g.finishLocked(err)
	}
	return
}

func (g *agyGate) finish(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err == nil && (!g.initialized || !g.idle) {
		err = errors.New("agy stream closed before an idle result")
	}
	g.finishLocked(err)
}

func (g *agyGate) poll(ctx context.Context) (context.Context, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.idle {
		return nil, false
	}
	g.ctx, g.cancel = context.WithCancel(ctx)
	return g.ctx, true
}

func (g *agyGate) endPoll() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cancel != nil {
		g.cancel()
		g.cancel = nil
		g.ctx = nil
	}
}

func (g *agyGate) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || !g.idle || g.ctx == nil || g.ctx.Err() != nil {
		return 0, errors.New("agy idle observation invalidated")
	}
	if len(p) > 512 {
		return 0, errors.New("agy notice exceeds atomic pipe budget")
	}
	n, err := g.write(p)
	if n > 0 {
		g.idle = false
	}
	return n, err
}

func (a *App) runAgyBridge(ctx context.Context, in io.ReadCloser, sid, model string, interval time.Duration) error {
	output := a.Stdout
	gate := &agyGate{write: func(p []byte) (int, error) {
		f, ok := output.(*os.File)
		if !ok {
			return 0, errors.New("agy output is not a pipe")
		}
		if native, ok := in.(*os.File); ok {
			pending, err := agyPipePending(native)
			if err != nil {
				return 0, err
			}
			if pending {
				return 0, errors.New("agy provider activity pending before write")
			}
		}
		return agyPipeWrite(f, p)
	}}
	a.Stdout = gate
	defer func() { a.Stdout = output }()
	done := make(chan struct{})
	initialized := make(chan struct{})
	readyStatus := make(chan struct{}, 1)
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 8192), 2<<20)
		first := true
		for scanner.Scan() {
			var f agyObservation
			if err := f.decode(scanner.Bytes()); err != nil {
				gate.finish(err)
				return
			}
			ready, err := gate.observe(f, sid, model)
			if err != nil {
				return
			}
			if first {
				close(initialized)
				first = false
			}
			if ready {
				select {
				case readyStatus <- struct{}{}:
				default:
				}
			}
		}
		if scanner.Err() != nil {
			gate.finish(errors.New("failed to read agy stream"))
		} else {
			gate.finish(nil)
		}
	}()
	defer func() { in.Close(); <-done }()
	first := time.NewTimer(15 * time.Second)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-first.C:
		return errors.New("missing agy init")
	case <-done:
		return gate.err
	case <-initialized:
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return gate.err
		case <-readyStatus:
			fmt.Fprintln(a.Stderr, "agy bridge: target idle")
		case <-ticker.C:
			poll, ok := gate.poll(ctx)
			if !ok {
				continue
			}
			sent, err := a.agyNotice(poll, sid, hookEvent{"notice", "PreInvocation"}, true)
			gate.endPoll()
			if err != nil {
				select {
				case <-done:
					if gate.err != nil {
						return errors.Join(gate.err, err)
					}
				default:
				}
				return fmt.Errorf("agy idle poll: %w", err)
			}
			if sent {
				fmt.Fprintln(a.Stderr, "agy bridge: notification submitted")
			}
		}
	}
}
