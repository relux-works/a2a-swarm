package tmuxwake

import (
	"context"
	"errors"
	"os"
	"testing"
)

// fakeMuseSender is the delivery seam double: scripted liveness and send
// outcomes, every call recorded. It never shells the Muse CLI or tmux.
type fakeMuseSender struct {
	live    bool
	liveErr error
	sendErr error
	sends   []struct{ sessionID, body string }
	lives   int
}

func (f *fakeMuseSender) SessionLive(_ context.Context, sessionID string) (bool, error) {
	f.lives++
	if !ValidMuseSessionID(sessionID) {
		return false, ErrMuseSessionDead
	}
	return f.live, f.liveErr
}

func (f *fakeMuseSender) SendSessionMessage(_ context.Context, sessionID, body string) error {
	f.sends = append(f.sends, struct{ sessionID, body string }{sessionID, body})
	return f.sendErr
}

func museBound(t *testing.T, sender MuseSender) BoundTerminal {
	t.Helper()
	birth, err := Birth(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	target := Target{Runtime: "muse", OwnerPID: os.Getpid(), OwnerBirth: birth}
	return BoundTerminal{Target: target, Generation: "muse-gen", SessionID: "muse-session-1", Muse: sender}
}

func TestPinMuse(t *testing.T) {
	live := &fakeMuseSender{live: true}
	target, err := PinMuse(context.Background(), "muse-session-1", live)
	if err != nil || target.Runtime != "muse" || target.Pane != "" || target.Socket != "" || live.lives != 1 {
		t.Fatalf("live pin: target=%+v err=%v lives=%d", target, err, live.lives)
	}
	if _, err := PinMuse(context.Background(), "muse-session-1", &fakeMuseSender{live: false}); !errors.Is(err, ErrMuseSessionDead) {
		t.Fatalf("dead pin err = %v, want ErrMuseSessionDead", err)
	}
	if _, err := PinMuse(context.Background(), "bad id!", live); err == nil {
		t.Fatal("malformed session id pinned")
	}
	if _, err := PinMuse(context.Background(), "muse-session-1", &fakeMuseSender{liveErr: errors.New("boom")}); !errors.Is(err, ErrMuseUnavailable) {
		t.Fatalf("unavailable pin err = %v, want ErrMuseUnavailable", err)
	}
	// Pin accepts the muse runtime too; a malformed id refuses before any CLI.
	if _, err := Pin(context.Background(), "", "bad id!", "muse"); err == nil {
		t.Fatal("Pin pinned a malformed muse session id")
	}
	if _, err := Pin(context.Background(), "", "", "other"); err == nil {
		t.Fatal("Pin accepted an unknown runtime")
	}
}

func TestMuseReadinessVocabulary(t *testing.T) {
	bound := museBound(t, &fakeMuseSender{live: true})
	r := bound.AssessReadiness(context.Background())
	if !r.Eligible() || r.Runtime.Reason != RuntimeNone || r.Key.Route != MuseSessionRoute || !r.Key.valid() {
		t.Fatalf("live assessment: %+v", r)
	}
	dead := museBound(t, &fakeMuseSender{live: false})
	r = dead.AssessReadiness(context.Background())
	if r.Eligible() || r.Runtime.Reason != RuntimeSessionDead || r.Runtime.State != RuntimeRefused || r.Key != (ReadinessKey{}) {
		t.Fatalf("dead assessment: %+v", r)
	}
	down := museBound(t, &fakeMuseSender{liveErr: errors.New("boom")})
	r = down.AssessReadiness(context.Background())
	if r.Eligible() || r.Runtime.Reason != RuntimeSessionUnavailable {
		t.Fatalf("unavailable assessment: %+v", r)
	}
	for _, reason := range []RuntimeReason{RuntimeSessionDead, RuntimeSessionUnavailable} {
		if !ValidRuntimeReason(reason) {
			t.Fatalf("reason %q not in the closed vocabulary", reason)
		}
	}
}

func TestMuseSendSeam(t *testing.T) {
	fenced := false
	before := func() { fenced = true }

	t.Run("success", func(t *testing.T) {
		fake := &fakeMuseSender{live: true}
		bound := museBound(t, fake)
		key := bound.AssessReadiness(context.Background()).Key
		result, err := bound.Send(context.Background(), 3, key, before, nil)
		if err != nil || !result.Valid() || result.Disposition != SendSubmitted || result.Outcome != SendWakeSubmitted {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if !fenced || len(fake.sends) != 1 || fake.sends[0].sessionID != "muse-session-1" || fake.sends[0].body != Notice(3) {
			t.Fatalf("fenced=%t sends=%+v", fenced, fake.sends)
		}
	})

	t.Run("sender_unverified", func(t *testing.T) {
		fake := &fakeMuseSender{live: true, sendErr: ErrMuseSenderUnverified}
		bound := museBound(t, fake)
		key := bound.AssessReadiness(context.Background()).Key
		result, err := bound.Send(context.Background(), 1, key, before, nil)
		if err != nil || !result.Valid() || result.Disposition != SendRefused || result.Reason != ReasonSenderUnverified || result.ReasonAt.IsZero() {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if SafeSendReason(result.Reason) != ReasonSenderUnverified {
			t.Fatalf("reason %q escaped the closed vocabulary", result.Reason)
		}
	})

	t.Run("dead at send", func(t *testing.T) {
		fake := &fakeMuseSender{live: true, sendErr: ErrMuseSessionDead}
		bound := museBound(t, fake)
		key := bound.AssessReadiness(context.Background()).Key
		result, err := bound.Send(context.Background(), 1, key, before, nil)
		if err != nil || result.Disposition != SendRefused || result.Reason != ReasonSessionDead {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})

	t.Run("dead before send sends nothing", func(t *testing.T) {
		fake := &fakeMuseSender{live: true}
		bound := museBound(t, fake)
		key := bound.AssessReadiness(context.Background()).Key
		fake.live = false
		result, err := bound.Send(context.Background(), 1, key, before, nil)
		if err != nil || result.Disposition != SendRefused || result.Reason != ReasonSessionDead || len(fake.sends) != 0 {
			t.Fatalf("result=%+v err=%v sends=%+v", result, err, fake.sends)
		}
	})

	t.Run("uncertain delivery", func(t *testing.T) {
		fake := &fakeMuseSender{live: true, sendErr: ErrMuseUnavailable}
		bound := museBound(t, fake)
		key := bound.AssessReadiness(context.Background()).Key
		result, err := bound.Send(context.Background(), 1, key, before, nil)
		if err == nil || !result.Valid() || result.Disposition != SendUncertain || result.Outcome != SendDeliveryUncertain {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})

	t.Run("stale key refused without delivery", func(t *testing.T) {
		fake := &fakeMuseSender{live: true}
		bound := museBound(t, fake)
		result, err := bound.Send(context.Background(), 1, ReadinessKey{}, before, nil)
		if err != nil || result.Disposition != SendRefused || len(fake.sends) != 0 {
			t.Fatalf("result=%+v err=%v sends=%+v", result, err, fake.sends)
		}
	})

	t.Run("resume never delivers", func(t *testing.T) {
		fake := &fakeMuseSender{live: true}
		bound := museBound(t, fake)
		key := bound.AssessReadiness(context.Background()).Key
		result, err := bound.ResumeNotice(context.Background(), 1, Notice(1), key)
		if err != nil || !result.Valid() || result.Disposition != SendRefused || len(fake.sends) != 0 {
			t.Fatalf("result=%+v err=%v sends=%+v", result, err, fake.sends)
		}
	})

	t.Run("classifier", func(t *testing.T) {
		for out, want := range map[string]error{
			`{"error":"sender_unverified"}`:                                 ErrMuseSenderUnverified,
			"external_agent_ingress_closed":                                 ErrMuseIngressClosed,
			`{"status":"target_gone","error_code":"target_not_found"}`:     ErrMuseSessionDead,
			`{"ok":true}`:                                                   nil,
		} {
			if got := classifyMuseSend(out); got != want {
				t.Fatalf("classifyMuseSend(%q) = %v, want %v", out, got, want)
			}
		}
	})
}

func TestParseMuseSessions(t *testing.T) {
	// The document shape printed by `muse sessions --json`.
	doc := []byte(`{"sessions":[{"sessionId":"00000000-0000-4000-8000-000000000001","name":"x","workspaceLabel":"w","openedBy":"tui","binaryVersion":null,"attachable":false},{"sessionId":"muse-session-1","name":"y","workspaceLabel":"w","openedBy":"exec","binaryVersion":null,"attachable":false}]}`)
	if live, err := parseMuseSessions(doc, "muse-session-1"); err != nil || !live {
		t.Fatalf("member: live=%t err=%v", live, err)
	}
	if live, err := parseMuseSessions(doc, "muse-session-9"); err != nil || live {
		t.Fatalf("non-member: live=%t err=%v", live, err)
	}
	if _, err := parseMuseSessions([]byte("not json"), "muse-session-1"); !errors.Is(err, ErrMuseUnavailable) {
		t.Fatalf("malformed err = %v, want ErrMuseUnavailable", err)
	}
}

func TestMuseTargetInspectNeedsNoPane(t *testing.T) {
	bound := museBound(t, &fakeMuseSender{live: true})
	if _, _, _, err := bound.Target.Inspect(context.Background()); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	gone := bound.Target
	gone.OwnerBirth = "someone else"
	if _, _, _, err := gone.Inspect(context.Background()); !errors.Is(err, ErrTargetGone) {
		t.Fatalf("replaced owner inspect err = %v, want ErrTargetGone", err)
	}
}
