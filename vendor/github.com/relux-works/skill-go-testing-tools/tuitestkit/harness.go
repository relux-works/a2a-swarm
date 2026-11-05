package tuitestkit

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

// Send sends messages to a bubbletea model sequentially and returns the final model.
// The generic type parameter preserves the concrete model type, avoiding manual
// type assertions in test code.
//
// Panics if the model's Update method returns a type different from M, which
// indicates a broken Update implementation.
//
// If no messages are provided, the model is returned unchanged.
// Init() is NOT called — callers handle initialization separately.
//
// Example:
//
//	m := myModel{count: 0}
//	m = tuitestkit.Send(m, tuitestkit.Key("up"), tuitestkit.Key("enter"))
//	// m is still myModel, no type assertion needed
func Send[M tea.Model](model M, msgs ...tea.Msg) M {
	if len(msgs) == 0 {
		return model
	}

	for _, msg := range msgs {
		updated, _ := model.Update(msg)
		concrete, ok := updated.(M)
		if !ok {
			panic(fmt.Sprintf(
				"tuitestkit.Send: Update returned %T, expected %T — broken Update implementation",
				updated, model,
			))
		}
		model = concrete
	}

	return model
}

// SendAndCollect sends messages to a bubbletea model sequentially, collects all
// non-nil Cmds returned by Update, and returns the final model along with the
// collected Cmds.
//
// Like Send, this preserves the concrete model type via generics and panics if
// Update returns an unexpected type.
//
// Nil cmds are not included in the returned slice.
// Init() is NOT called — callers handle initialization separately.
//
// Example:
//
//	m := myModel{}
//	m, cmds := tuitestkit.SendAndCollect(m, tuitestkit.Key("q"))
//	msgs := tuitestkit.ExecCmds(cmds...)
func SendAndCollect[M tea.Model](model M, msgs ...tea.Msg) (M, []tea.Cmd) {
	var cmds []tea.Cmd

	if len(msgs) == 0 {
		return model, cmds
	}

	for _, msg := range msgs {
		updated, cmd := model.Update(msg)
		concrete, ok := updated.(M)
		if !ok {
			panic(fmt.Sprintf(
				"tuitestkit.SendAndCollect: Update returned %T, expected %T — broken Update implementation",
				updated, model,
			))
		}
		model = concrete
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	return model, cmds
}

// ExecCmds executes tea.Cmd functions synchronously and collects the resulting
// messages. Nil cmds in the input are skipped. If a Cmd returns a tea.BatchMsg
// (which is []tea.Cmd), those cmds are recursively executed and their messages
// collected.
//
// This is useful for testing command pipelines without running the bubbletea
// event loop.
//
// Example:
//
//	m, cmds := tuitestkit.SendAndCollect(m, tuitestkit.Key("enter"))
//	msgs := tuitestkit.ExecCmds(cmds...)
//	m = tuitestkit.Send(m, msgs...)
func ExecCmds(cmds ...tea.Cmd) []tea.Msg {
	var msgs []tea.Msg

	for _, cmd := range cmds {
		if cmd == nil {
			continue
		}
		msg := cmd()
		if msg == nil {
			continue
		}
		// If the message is a BatchMsg, recursively execute its cmds
		if batch, ok := msg.(tea.BatchMsg); ok {
			msgs = append(msgs, ExecCmds(batch...)...)
		} else {
			msgs = append(msgs, msg)
		}
	}

	return msgs
}
