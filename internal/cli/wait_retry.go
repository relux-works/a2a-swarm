package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"syscall"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// waitRetry retries a delivery operation in this waiter, without starting a
// goroutine or changing its cursor. Each successful operation resets backoff.
func waitRetry[T any](ctx context.Context, s *session, deadline time.Time, attempt func() (T, error)) (T, error) {
	var zero T
	delay := 5 * time.Second
	retrying := false
	for {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if retrying && !deadline.IsZero() && !s.app.Now().Before(deadline) {
			return zero, errWaitTimeout
		}
		if err := s.app.acquireLease(s, "", time.Time{}); err != nil {
			return zero, err
		}
		value, err := attempt()
		if err == nil {
			return value, nil
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		if !waitTemporary(err) {
			return zero, err
		}
		pause := delay
		if !deadline.IsZero() {
			remaining := deadline.Sub(s.app.Now())
			if remaining <= 0 {
				return zero, errWaitTimeout
			}
			pause = min(pause, remaining)
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-timer.C:
		}
		retrying = true
		if delay == 5*time.Second {
			delay = 30 * time.Second
		} else {
			delay = min(2*delay, 5*time.Minute)
		}
	}
}

var errWaitTimeout = errors.New("wait: timeout")

func (s *session) waitSync(ctx context.Context, since string, poll time.Duration, filter string) (matrix.SyncResponse, error) {
	resp, err := s.client.Sync(ctx, since, poll, filter)
	if err == nil && resp.NextBatch == "" {
		err = errors.New("wait: sync response has no next_batch")
	}
	return resp, err
}

// Only known temporary failures qualify. A URL wrapper alone is not evidence
// of a transport outage: invalid URLs, redirects and TLS configuration also use
// it. Malformed successful responses and local state errors remain refusals.
func waitTemporary(err error) bool {
	if errors.Is(err, matrix.ErrAuth) || errors.Is(err, context.Canceled) {
		return false
	}
	var me *matrix.Error
	if errors.As(err, &me) {
		switch me.ErrCode {
		case "M_UNKNOWN_TOKEN", "M_MISSING_TOKEN", "M_FORBIDDEN", "M_BAD_JSON", "M_NOT_JSON", "M_INVALID_PARAM", "M_UNRECOGNIZED":
			return false
		}
		switch me.Status {
		case 429, 500, 502, 503, 504:
			return true
		}
		return false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) && dns.IsNotFound {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && (ne.Timeout() || ne.Temporary()) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
			errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) ||
			errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH)
	}
	var u *url.Error
	return errors.As(err, &u) && (errors.Is(u.Err, io.EOF) || errors.Is(u.Err, io.ErrUnexpectedEOF))
}
