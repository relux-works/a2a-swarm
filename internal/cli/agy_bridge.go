package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
	if _, err := a.identity(); err != nil {
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
	return a.runAgyBridge(ctx, reader, sid, model, 5*time.Second)
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
	if raw, ok := root["conversation_id"]; ok && json.Unmarshal(raw, &f.Conversation) != nil {
		return invalid
	}
	switch f.Event {
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

func (a *App) runAgyBridge(ctx context.Context, in io.ReadCloser, sid, model string, interval time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	frames := make(chan agyObservation)
	end := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 8192), 2<<20)
		for scanner.Scan() {
			var frame agyObservation
			if err := frame.decode(scanner.Bytes()); err != nil {
				end <- errors.New("invalid agy frame")
				return
			}
			select {
			case frames <- frame:
			case <-ctx.Done():
				return
			}
		}
		if scanner.Err() != nil {
			end <- errors.New("failed to read agy stream")
		} else {
			end <- nil
		}
	}()
	defer func() { cancel(); in.Close(); <-done }()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	initialized, idle := false, false
	first := time.NewTimer(15 * time.Second)
	defer first.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-first.C:
			return errors.New("missing agy init")
		case err := <-end:
			if err != nil {
				return err
			}
			if !initialized || !idle {
				return errors.New("agy stream closed before an idle result")
			}
			return nil
		case f := <-frames:
			if !initialized {
				if f.Event != "init" || f.Conversation != sid || f.Init.Model != model {
					return errors.New("agy init binding mismatch")
				}
				initialized = true
				first.Stop()
				continue
			}
			if idle {
				return errors.New("unexpected activity after agy idle result")
			}
			if f.Event == "init" || (f.Conversation != "" && f.Conversation != sid) {
				return errors.New("agy stream binding changed")
			}
			if f.Event == "result" {
				if f.Result.Conversation != sid || f.Result.Status != "SUCCESS" {
					return errors.New("agy result did not confirm a successful turn in this conversation")
				}
				idle = true
				fmt.Fprintln(a.Stderr, "agy bridge: target idle")
			} else {
				idle = false
			}
		case <-ticker.C:
			if idle {
				sent, err := a.agyNotice(ctx, sid, hookEvent{"notice", "PreInvocation"}, true)
				if err != nil {
					return fmt.Errorf("agy idle poll: %w", err)
				}
				if sent {
					fmt.Fprintln(a.Stderr, "agy bridge: notification submitted")
					idle = false
				}
			}
		}
	}
}
