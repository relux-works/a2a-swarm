package hostnotification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"time"
	"unicode/utf8"

	"github.com/relux-works/a2a-swarm/internal/config"
)

var ErrState = errors.New("host notification state unavailable")

// Adapter combines the original shared Source reservation with one admitted
// Host stream. Its owner must construct and use it entirely inside the same
// WithHostNotificationEnrollment callback, retaining sole-consumer custody.
// It does not acquire that custody, authenticate a caller or attach an endpoint.
type Adapter struct {
	paths      config.Paths
	name       string
	attachment config.HostAttachment
	actual     config.HostScope
	client     *Client
	clock      func() time.Time
}

// NewAdapter checks the actual persisted enrollment and complete stream binding.
// Construction requires live admission; retained observations on this original
// adapter may finish after expiry. No implicit successor or reattachment exists.
func NewAdapter(p config.Paths, name string, deadline time.Time, attachment config.HostAttachment, actual config.HostScope, client *Client, clock func() time.Time) (*Adapter, error) {
	if clock == nil || client == nil || name != actual.Agent {
		return nil, config.ErrHostEnrollment
	}
	a := &Adapter{paths: p, name: name, attachment: attachment, actual: actual, client: client, clock: clock}
	err := a.control(context.Background(), deadline, func(_ time.Time) error {
		s, err := config.LoadState(p, name)
		if err != nil {
			return err
		}
		if err := s.CheckHostEnrollment(attachment, actual, clock()); err != nil {
			return err
		}
		if !a.matches(s) {
			return config.ErrHostEnrollment
		}
		return nil
	})
	if err != nil {
		return nil, stateError(err)
	}
	return a, nil
}

func (a *Adapter) matches(s config.State) bool {
	e := s.HostEnrollment
	if e == nil || e.Version != 2 || e.Validate() != nil || e.Attachment.Reference != a.attachment.Reference || e.Attachment.Scope != a.actual {
		return false
	}
	x, y := e.Attachment.Lease, a.attachment.Lease
	if x.Holder != y.Holder || x.Host != y.Host || x.PID != y.PID || !x.Expires.Equal(y.Expires) {
		return false
	}
	b := a.client.binding
	for _, entry := range e.Attempts.Entries {
		if entry.Key.Binding != b {
			return false
		}
	}
	return b.EnrollmentRef == e.ReceiptRef && b.NativeThread == a.actual.NativeThread && b.MemberRef == a.actual.Member && b.ConnectionRef == a.actual.Connection && b.ConfigurationRef == a.actual.Configuration && b.ConsumerGeneration == a.actual.Generation
}

func (a *Adapter) retained(s config.State, key config.NotificationAttemptKey) bool {
	if !a.matches(s) || key.Binding != a.client.binding || key.Session != a.actual.Session || key.Generation != a.actual.Generation {
		return false
	}
	for _, entry := range s.HostEnrollment.Attempts.Entries {
		if entry.Key == key && (entry.State == "unknown" || entry.State == "accepted") {
			return true
		}
	}
	return false
}

// Notify hashes the trusted fixed Source notice locally; neither its bytes nor
// peer text crosses this work-reference wire. A returned key with an error is
// correlation only, never a persistence, delivery or nonapplication receipt.
func (a *Adapter) Notify(ctx context.Context, notice []byte, deadline, legacy time.Time) (config.NotificationAttemptKey, Response, error) {
	var key config.NotificationAttemptKey
	if len(notice) == 0 || len(notice) > MaxFrame || !utf8.Valid(notice) {
		return key, Response{}, ErrProtocol
	}
	digest := sha256.Sum256(notice)
	var requestID string
	err := a.control(ctx, deadline, func(deadline time.Time) error {
		_, err := config.UpdateStateBy(a.paths, a.name, deadline, func(s *config.State) error {
			if !a.matches(*s) {
				return config.ErrHostEnrollment
			}
			var err error
			key, err = s.PrepareHostNotification(a.attachment, a.actual, a.client.binding, hex.EncodeToString(digest[:]), a.clock(), legacy)
			return err
		})
		if err != nil {
			return err
		}
		requestID, err = NewRequestID()
		if err != nil {
			return err
		}
		// A separate successful write is required: prepared is the durable
		// pre-I/O abandonment boundary; UNKNOWN fences the original trigger.
		if err := ready(ctx, deadline); err != nil {
			return err
		}
		_, err = config.UpdateStateBy(a.paths, a.name, deadline, func(s *config.State) error {
			return s.BeginHostNotification(a.attachment, a.actual, key, requestID, a.clock())
		})
		return err
	})
	if err != nil {
		return key, Response{}, stateError(err)
	}
	// Persistence can cross expiry. Retain UNKNOWN and send nothing in that case.
	if now := a.clock(); now.IsZero() || !a.attachment.Lease.Expires.After(now) {
		return key, Response{}, config.ErrHostEnrollment
	}
	if err := ready(ctx, deadline); err != nil {
		return key, Response{}, err
	}
	callCtx, cancel := context.WithDeadline(ctx, minTime(deadline, a.attachment.Lease.Expires))
	defer cancel()
	r, err := a.client.Call(callCtx, requestID, "trigger", "a2a-"+key.Token)
	if err != nil {
		return key, Response{}, err
	}
	if r.Result == "unresolved" {
		return key, r, ErrUnresolved
	}
	err = a.update(ctx, deadline, func(s *config.State) error {
		if !a.retained(*s, key) {
			return config.ErrNotificationAttempt
		}
		if r.Result == "refused" {
			return s.RefuseHostNotification(key, requestID, r.Reason)
		}
		return s.AcceptHostNotification(key, r.Attempt.Sequence, r.Attempt.NativeStage, a.clock())
	})
	return key, r, err
}

// Reconcile only observes this original retained key. Lookup refusal, absence,
// malformed reply or lost transport never releases UNKNOWN or sends a trigger.
func (a *Adapter) Reconcile(ctx context.Context, key config.NotificationAttemptKey, deadline time.Time) (Response, error) {
	err := a.control(ctx, deadline, func(deadline time.Time) error {
		s, err := config.LoadState(a.paths, a.name)
		if err != nil {
			return err
		}
		if !a.retained(s, key) {
			return config.ErrNotificationAttempt
		}
		return nil
	})
	if err != nil {
		return Response{}, stateError(err)
	}
	requestID, err := NewRequestID()
	if err != nil {
		return Response{}, err
	}
	if err := ready(ctx, deadline); err != nil {
		return Response{}, err
	}
	callCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	r, err := a.client.Call(callCtx, requestID, "lookup_attempt", "a2a-"+key.Token)
	if err != nil {
		return Response{}, err
	}
	if r.Result != "existing-attempt" {
		return r, ErrUnresolved
	}
	err = a.update(ctx, deadline, func(s *config.State) error {
		if !a.retained(*s, key) {
			return config.ErrNotificationAttempt
		}
		return s.AcceptHostNotification(key, r.Attempt.Sequence, r.Attempt.NativeStage, a.clock())
	})
	return r, err
}

// AbandonPrepared is explicit pre-I/O cancellation. It can never abandon an
// UNKNOWN attempt, including after lease or notification cooldown expiry.
func (a *Adapter) AbandonPrepared(ctx context.Context, key config.NotificationAttemptKey, deadline time.Time) error {
	return a.update(ctx, deadline, func(s *config.State) error {
		if !a.matches(*s) || key.Binding != a.client.binding {
			return config.ErrNotificationAttempt
		}
		return s.AbandonHostNotification(key)
	})
}

func ready(ctx context.Context, deadline time.Time) error {
	if ctx == nil {
		return ErrProtocol
	}
	if ctx.Err() != nil {
		return ErrUnresolved
	}
	if deadline.IsZero() || !deadline.After(time.Now()) {
		return config.ErrLockTimeout
	}
	return nil
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func (a *Adapter) control(ctx context.Context, deadline time.Time, run func(time.Time) error) error {
	if err := ready(ctx, deadline); err != nil {
		return err
	}
	// Existing lock helpers may create their directory. This adapter may only
	// use an already provisioned namespace, including after a failed restart.
	if a.paths.Base == "" {
		return config.ErrHostStateScope
	}
	root, err := os.OpenRoot(a.paths.Base)
	if err != nil {
		return config.ErrHostStateScope
	}
	if err := root.Close(); err != nil {
		return config.ErrHostStateScope
	}
	if at, ok := ctx.Deadline(); ok {
		deadline = minTime(deadline, at)
	}
	return config.WithRuntimeLock(a.paths, a.name, "control", deadline, func() error {
		if err := ready(ctx, deadline); err != nil {
			return err
		}
		return run(deadline)
	})
}

func (a *Adapter) update(ctx context.Context, deadline time.Time, change func(*config.State) error) error {
	return stateError(a.control(ctx, deadline, func(deadline time.Time) error {
		_, err := config.UpdateStateBy(a.paths, a.name, deadline, change)
		return err
	}))
}

// Preserve only public fixed categories, never a wrapped filesystem path or
// decoder payload. The post-rename uncertainty category remains distinguishable.
func stateError(err error) error {
	if err == nil {
		return nil
	}
	for _, category := range []error{config.ErrHostStateUncertain, config.ErrHostStatePersistence, config.ErrHostStateCapacity, config.ErrHostStateScope, config.ErrHostEnrollment, config.ErrNotificationAttemptCapacity, config.ErrNotificationAttempt, config.ErrNotificationSpacing, config.ErrNotificationStale, config.ErrLockTimeout, ErrProtocol, ErrUnresolved, ErrBusy} {
		if errors.Is(err, category) {
			return category
		}
	}
	return ErrState
}
