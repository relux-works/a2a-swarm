package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Keep the persisted fixture's clock while timers advance in fake time. All
// HTTP in the bubble goes straight to the fake homeserver, without sockets.
func noticeFakeTime(t *testing.T, e *env, a *App, run func(*testing.T)) {
	t.Helper()
	at := a.Now()
	a.Now = func() time.Time { return at }
	e.now = a.Now
	e.configure = func(app *App) {
		app.HTTP = &http.Client{Transport: noticeHandlerTransport(e.hs.srv.Config.Handler)}
	}
	parent := e.t
	synctest.Test(t, func(t *testing.T) {
		e.t = t
		defer func() { e.t = parent }()
		run(t)
	})
}

// Prepare real fixture I/O before entering the fake-time test callback.
func noticeDeadlineTest(fakeTime bool, run func(*testing.T, *env, *App, *session)) func(*testing.T) {
	return func(t *testing.T) {
		t.Helper()
		e, a, s := noticeDeadlineFixture(t)
		if fakeTime {
			noticeFakeTime(t, e, a, func(t *testing.T) { run(t, e, a, s) })
		} else {
			run(t, e, a, s)
		}
	}
}

func noticeHandlerTransport(h http.Handler) noticeWriterTransport {
	return func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		resp := rec.Result()
		resp.Request = r
		return resp, nil
	}
}

// Deliver the first bytes, then withhold the rest until the request expires.
// Unlike a listener, this body is durably blocked inside the synctest bubble.
func noticePartialResponse(r *http.Request) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Request:    r,
		Body: io.NopCloser(io.MultiReader(strings.NewReader(`{"event_id":"`),
			noticeDeadlineBody{r.Context()})),
	}
}

type noticeDeadlineBody struct{ ctx context.Context }

func (b noticeDeadlineBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

// Expiry is a real refusal, even though valid controls no longer race real time.
func TestNoticeExpiredPublicationDeadline(t *testing.T) {
	isolateBindTest(t)
	e, a, s := noticeDeadlineFixture(t)
	noticeFakeTime(t, e, a, func(t *testing.T) {
		before := e.state("alice")
		probe := &noticeContextTransport{next: noticeHandlerTransport(e.hs.srv.Config.Handler)}
		s.client.HTTP = &http.Client{Transport: probe}
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		time.Sleep(150 * time.Millisecond)
		if err := s.publishTerminalNotice(ctx, "deadline"); err == nil || len(probe.contexts) != 0 || !reflect.DeepEqual(before.Liveness, e.state("alice").Liveness) {
			t.Fatal("expired publication deadline made HTTP or acknowledged a write")
		}
		a.publishTerminalNotice(context.Background(), s, "deadline")
		if len(probe.contexts) != 2 || e.state("alice").Terminal.NoticePublication != "published" {
			t.Fatal("valid publication control refused")
		}
	})
}
