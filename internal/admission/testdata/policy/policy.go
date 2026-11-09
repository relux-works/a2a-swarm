// Package policy is linked only by tests, exercising the fork's init seam.
package policy

import (
	"context"
	"errors"
	"sync"

	"github.com/relux-works/a2a-swarm/internal/admission"
)

const Name = "fixture-425"
const NostrName = "fixture-425-nostr"
const Leak = "DISTINCTIVE_PRIVATE_BODY_SENDER_425"

type Fixture struct {
	Mu                        sync.Mutex
	In                        admission.Decision
	Out                       admission.Decision
	Err                       bool
	InCalls, OutCalls, Closes int
	Config                    admission.Config
	Requests                  []admission.Outbound
}

var fixtures sync.Map

func Get(dir string) *Fixture {
	f, _ := fixtures.LoadOrStore(dir, &Fixture{In: admission.Decision{Verdict: admission.Live, Reason: "allowed"}, Out: admission.Decision{Verdict: admission.Live, Reason: "allowed"}})
	return f.(*Fixture)
}
func init() {
	factory := func(c admission.Config) (admission.Checker, error) {
		f := Get(c.StateDir)
		f.Mu.Lock()
		f.Config = c
		f.Mu.Unlock()
		return f, nil
	}
	admission.Register(Name, factory)
	admission.RegisterNostrOnly(NostrName, factory)
	admission.Register("fixture-425-error", func(admission.Config) (admission.Checker, error) { return nil, errors.New(Leak) })
}
func (f *Fixture) Inbound(_ context.Context, in admission.Inbound) (admission.Decision, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.InCalls++
	if f.Err {
		return f.In, errors.New(Leak)
	}
	return f.In, nil
}
func (f *Fixture) Outbound(_ context.Context, out admission.Outbound) (admission.Decision, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.OutCalls++
	f.Requests = append(f.Requests, out)
	if f.Err {
		return f.Out, errors.New(Leak)
	}
	return f.Out, nil
}
func (f *Fixture) Close() error { f.Mu.Lock(); defer f.Mu.Unlock(); f.Closes++; return nil }
