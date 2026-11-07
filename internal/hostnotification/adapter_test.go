package hostnotification

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type adapterFixture struct {
	paths      config.Paths
	a          *Adapter
	attachment config.HostAttachment
	peer       *net.UnixConn
	now        atomic.Int64
	clockHook  func()
}

func (f *adapterFixture) clock() time.Time {
	if f.clockHook != nil {
		f.clockHook()
	}
	return time.Unix(0, f.now.Load()).UTC()
}
func withAdapter(t *testing.T, run func(*adapterFixture)) {
	t.Helper()
	f := &adapterFixture{paths: config.Paths{Base: t.TempDir()}}
	f.now.Store(time.Now().UTC().UnixNano())
	scope := config.HostScope{Agent: "member", Member: "member", Connection: "connection", Configuration: "config", Session: "source-session", NativeThread: "thread", Generation: "generation", Consumer: "consumer"}
	f.attachment = config.HostAttachment{Reference: "admitted-attachment", Scope: scope, Lease: config.Lease{Holder: scope.Session, Host: "host", PID: 42, Expires: f.clock().Add(10 * time.Minute)}}
	if err := config.SaveState(f.paths, scope.Agent, config.State{Lease: &f.attachment.Lease, NextBatch: "unread-retained"}); err != nil {
		t.Fatal(err)
	}
	err := config.WithHostNotificationEnrollment(f.paths, scope.Agent, limit(), f.attachment, scope, f.clock, func(receipt string) error {
		stream, peer := socketPair(t, syscall.SOCK_STREAM)
		f.peer = peer
		binding := fixtureRequest("trigger").Binding
		binding.EnrollmentRef = receipt
		client, err := New(stream, binding, 2000)
		if err != nil {
			return err
		}
		defer client.Close()
		f.a, err = NewAdapter(f.paths, scope.Agent, limit(), f.attachment, scope, client, f.clock)
		if err != nil {
			return err
		}
		run(f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func limit() time.Time { return time.Now().Add(5 * time.Second) }
func (f *adapterFixture) load(t *testing.T) config.State {
	t.Helper()
	s, err := config.LoadState(f.paths, "member")
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func (f *adapterFixture) update(t *testing.T, fn func(*config.State) error) {
	t.Helper()
	if err := f.a.update(context.Background(), limit(), fn); err != nil {
		t.Fatal(err)
	}
}

// Fixture peers exercise the actual STREAM and on-disk Source state only;
// replies are contract fixtures, not a running Host engine or native runtime.
func (f *adapterFixture) serve(steps ...func(Request) (Response, error)) <-chan error {
	done := make(chan error, 1)
	go func() {
		defer f.peer.Close()
		_ = f.peer.SetDeadline(limit())
		reader := bufio.NewReader(f.peer)
		for _, step := range steps {
			frame, err := reader.ReadBytes('\n')
			if err != nil {
				done <- err
				return
			}
			var request Request
			if err := json.Unmarshal(frame, &request); err != nil {
				done <- err
				return
			}
			response, err := step(request)
			if err != nil {
				done <- err
				return
			}
			if response.Contract == "" {
				done <- nil
				return
			}
			data, err := json.Marshal(response)
			if err == nil {
				_, err = f.peer.Write(append(data, '\n'))
			}
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	return done
}
func joined(t *testing.T, done <-chan error) {
	t.Helper()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func noRequest(t *testing.T, peer *net.UnixConn) {
	t.Helper()
	_ = peer.SetReadDeadline(time.Now().Add(25 * time.Millisecond))
	var b [1]byte
	n, err := peer.Read(b[:])
	if n != 0 || err == nil {
		t.Fatalf("unexpected Host I/O: %d %v", n, err)
	}
	_ = peer.SetReadDeadline(time.Time{})
}

// Proves a peer receives only the work reference AFTER both successful state
// writes, and acceptance completes spacing without receipting pending mail.
func TestAdapterPersistBeforeTrigger(t *testing.T) {
	withAdapter(t, func(f *adapterFixture) {
		notice := []byte("fixed Source doorbell")
		digest := sha256.Sum256(notice)
		done := f.serve(func(r Request) (Response, error) {
			s, err := config.LoadState(f.paths, "member")
			if err != nil {
				return Response{}, err
			}
			entries := s.HostEnrollment.Attempts.Entries
			if len(entries) != 1 || entries[0].State != "unknown" || entries[0].RequestID != r.RequestID || "a2a-"+entries[0].Key.Token != r.NoticeID || entries[0].Key.NoticeSHA256 != hex.EncodeToString(digest[:]) || r.Method != "trigger" || s.Notification.Reservation == nil {
				return Response{}, fmt.Errorf("trigger arrived before durable UNKNOWN: %+v", entries)
			}
			frame, _ := json.Marshal(r)
			if bytes.Contains(frame, notice) {
				return Response{}, errors.New("notice crossed wire")
			}
			return acceptedResponse(r), nil
		})
		key, response, err := f.a.Notify(context.Background(), notice, limit(), time.Time{})
		joined(t, done)
		if err != nil || response.Result != "accepted" || key.Token == "" {
			t.Fatalf("positive trigger: %+v %v", response, err)
		}
		s := f.load(t)
		if s.HostEnrollment.Attempts.Entries[0].State != "accepted" || s.Notification.Reservation != nil || s.NextBatch != "unread-retained" {
			t.Fatal("accepted state/mailbox changed incorrectly")
		}
		bad := &Client{binding: f.a.client.binding}
		bad.binding.Execution = "other-execution"
		if _, err := NewAdapter(f.paths, "member", limit(), f.attachment, f.attachment.Scope, bad, f.clock); !errors.Is(err, config.ErrHostEnrollment) {
			t.Fatalf("retained full binding replaced: %v", err)
		}
		if _, _, err := f.a.Notify(context.Background(), notice, limit(), time.Time{}); !errors.Is(err, config.ErrNotificationSpacing) {
			t.Fatalf("spacing bypass: %v", err)
		}
	})
}

// A nonzero legacy timestamp suppresses only the first spacing interval. The
// migration is saved before refusal, with no wire I/O or attempt; reloading
// and retrying cannot move that interval forward. The zero-legacy neighbor
// rings immediately, and both cases admit exactly one trigger.
func TestAdapterLegacyMigrationSpacing(t *testing.T) {
	for _, legacyEvidence := range []bool{false, true} {
		t.Run(fmt.Sprint(legacyEvidence), func(t *testing.T) {
			withAdapter(t, func(f *adapterFixture) {
				now := f.clock()
				var legacy time.Time
				if legacyEvidence {
					legacy = now.Add(-time.Minute)
					before := f.load(t)
					for retry := 0; retry < 2; retry++ {
						key, r, err := f.a.Notify(context.Background(), []byte("notice"), limit(), legacy)
						if !errors.Is(err, config.ErrNotificationSpacing) || key != (config.NotificationAttemptKey{}) || r.Contract != "" {
							t.Fatalf("migration refusal: key=%+v response=%+v err=%v", key, r, err)
						}
						noRequest(t, f.peer)
						got := f.load(t)
						if got.Notification.Version != 1 || !got.Notification.NotBefore.Equal(now.Add(config.NotificationEvery)) || got.Notification.Reservation != nil {
							t.Fatal("suppression failed to persist the one-time migration")
						}
						got.Notification = before.Notification
						if !reflect.DeepEqual(got, before) {
							t.Fatal("suppression changed custody, attempts or unrelated state")
						}
					}
					f.now.Add(int64(config.NotificationEvery))
				}
				calls := 0
				done := f.serve(func(r Request) (Response, error) {
					calls++
					if r.Method != "trigger" {
						return Response{}, errors.New("expected one trigger")
					}
					return acceptedResponse(r), nil
				})
				key, r, err := f.a.Notify(context.Background(), []byte("notice"), limit(), legacy)
				joined(t, done)
				if err != nil || r.Result != "accepted" || key.Token == "" || calls != 1 {
					t.Fatalf("post-spacing trigger: calls=%d response=%+v err=%v", calls, r, err)
				}
				if _, _, err := f.a.Notify(context.Background(), []byte("notice"), limit(), legacy); !errors.Is(err, config.ErrNotificationSpacing) {
					t.Fatalf("second trigger was not suppressed: %v", err)
				}
				got := f.load(t)
				if len(got.HostEnrollment.Attempts.Entries) != 1 || got.HostEnrollment.Attempts.Entries[0].State != "accepted" || got.Notification.Reservation != nil {
					t.Fatal("migration did not lead to exactly one completed attempt")
				}
			})
		})
	}
}

// Persistence failure takes precedence over spacing, and cannot grant I/O.
// The successful migration/write path is the neighboring test above.
func TestAdapterLegacyMigrationWriteRefusal(t *testing.T) {
	withAdapter(t, func(f *adapterFixture) {
		before := f.load(t)
		if err := os.Chmod(filepath.Join(f.paths.Base, "state"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.a.Notify(context.Background(), []byte("notice"), limit(), f.clock().Add(-time.Minute)); !errors.Is(err, config.ErrHostStateScope) {
			t.Fatalf("failed migration save reported as spacing: %v", err)
		}
		noRequest(t, f.peer)
		if !reflect.DeepEqual(before, f.load(t)) {
			t.Fatal("failed migration save changed state")
		}
	})
}

// Proves only an original conclusive refusal releases its reservation and a
// nearby valid successor can trigger, with a different source and transport ID.
func TestAdapterOriginalRefusal(t *testing.T) {
	withAdapter(t, func(f *adapterFixture) {
		var original Request
		done := f.serve(func(r Request) (Response, error) {
			original = r
			return Response{Contract: Contract, RequestID: r.RequestID, Method: r.Method, Result: "refused", Reason: "input-not-ready"}, nil
		}, func(r Request) (Response, error) {
			if r.NoticeID == original.NoticeID || r.RequestID == original.RequestID {
				return Response{}, errors.New("original identity reused")
			}
			return acceptedResponse(r), nil
		})
		key, r, err := f.a.Notify(context.Background(), []byte("notice"), limit(), time.Time{})
		if err != nil || r.Result != "refused" {
			t.Fatalf("refusal: %v", err)
		}
		s := f.load(t)
		if s.Notification.Reservation != nil || s.HostEnrollment.Attempts.Entries[0].State != "not-applied" {
			t.Fatal("original refusal did not cancel")
		}
		if err := f.a.AbandonPrepared(context.Background(), key, limit()); !errors.Is(err, config.ErrNotificationAttempt) {
			t.Fatalf("terminal abandonment: %v", err)
		}
		_, r, err = f.a.Notify(context.Background(), []byte("next"), limit(), time.Time{})
		joined(t, done)
		if err != nil || r.Result != "accepted" {
			t.Fatalf("successor refused: %v", err)
		}
	})
}

// Proves a lost reply remains UNKNOWN on disk after cooldown and reload, and
// neither another Notify nor explicit abandonment can replay or cancel it.
func TestAdapterLostReply(t *testing.T) {
	withAdapter(t, func(f *adapterFixture) {
		done := f.serve(func(r Request) (Response, error) { return Response{}, nil })
		key, _, err := f.a.Notify(context.Background(), []byte("notice"), limit(), time.Time{})
		joined(t, done)
		if !errors.Is(err, ErrUnresolved) {
			t.Fatalf("lost reply: %v", err)
		}
		before := f.load(t)
		f.now.Add(int64(time.Minute))
		if _, _, err := f.a.Notify(context.Background(), []byte("new notice"), limit(), time.Time{}); !errors.Is(err, config.ErrNotificationAttempt) {
			t.Fatalf("UNKNOWN replay: %v", err)
		}
		if err := f.a.AbandonPrepared(context.Background(), key, limit()); !errors.Is(err, config.ErrNotificationAttempt) {
			t.Fatalf("UNKNOWN abandoned: %v", err)
		}
		if _, err := f.a.Reconcile(context.Background(), key, limit()); !errors.Is(err, ErrUnresolved) {
			t.Fatalf("dead endpoint: %v", err)
		}
		after := f.load(t)
		if !reflect.DeepEqual(before, after) || after.HostEnrollment.Attempts.Entries[0].State != "unknown" || after.Notification.Reservation == nil {
			t.Fatal("lost response fence lost across reload")
		}
	})
}

// Proves lookup refuses to infer nonapplication from missing/refused/unresolved
// observations, but an exact later observation resolves after original expiry.
func TestAdapterLookupAfterExpiry(t *testing.T) {
	withAdapter(t, func(f *adapterFixture) {
		var original Request
		done := f.serve(func(r Request) (Response, error) {
			original = r
			return Response{Contract: Contract, RequestID: r.RequestID, Method: r.Method, Result: "unresolved", Reason: "attempt-unresolved"}, nil
		}, func(r Request) (Response, error) {
			if r.Method != "lookup_attempt" || r.RequestID == original.RequestID || r.NoticeID != original.NoticeID {
				return Response{}, errors.New("lookup retriggered or changed key")
			}
			return Response{Contract: Contract, RequestID: r.RequestID, Method: r.Method, Result: "refused", Reason: "invalid-attempt"}, nil
		}, func(r Request) (Response, error) {
			if r.Method != "lookup_attempt" {
				return Response{}, errors.New("expired lookup triggered")
			}
			response := acceptedResponse(r)
			response.Result = "existing-attempt"
			response.Attempt.NativeStage = "native-completed"
			return response, nil
		})
		key, _, err := f.a.Notify(context.Background(), []byte("notice"), limit(), time.Time{})
		if !errors.Is(err, ErrUnresolved) {
			t.Fatal(err)
		}
		before := f.load(t)
		wrong := key
		wrong.NoticeSHA256 = strings.Repeat("b", 64)
		if _, err := f.a.Reconcile(context.Background(), wrong, limit()); !errors.Is(err, config.ErrNotificationAttempt) {
			t.Fatalf("copied token accepted: %v", err)
		}
		f.now.Add(int64(11 * time.Minute))
		if _, _, err := f.a.Notify(context.Background(), []byte("next"), limit(), time.Time{}); !errors.Is(err, config.ErrHostEnrollment) {
			t.Fatalf("expired new trigger: %v", err)
		}
		if _, err := NewAdapter(f.paths, "member", limit(), f.attachment, f.attachment.Scope, f.a.client, f.clock); !errors.Is(err, config.ErrHostEnrollment) {
			t.Fatalf("expired construction: %v", err)
		}
		if _, err := f.a.Reconcile(context.Background(), key, limit()); !errors.Is(err, ErrUnresolved) {
			t.Fatalf("lookup refusal: %v", err)
		}
		if !reflect.DeepEqual(before, f.load(t)) {
			t.Fatal("lookup refusal canceled UNKNOWN")
		}
		if _, err := f.a.Reconcile(context.Background(), key, limit()); err != nil {
			t.Fatal(err)
		}
		joined(t, done)
		s := f.load(t)
		if s.Notification.Reservation != nil || s.HostEnrollment.Attempts.Entries[0].NativeStage != "native-completed" {
			t.Fatal("exact expired observation not persisted")
		}
	})
}

// Proves an older accepted observation can advance its stage without completing
// a different newer UNKNOWN reservation or changing pending mailbox state.
func TestAdapterLateObservation(t *testing.T) {
	withAdapter(t, func(f *adapterFixture) {
		done := f.serve(func(r Request) (Response, error) { return acceptedResponse(r), nil }, func(r Request) (Response, error) {
			return Response{Contract: Contract, RequestID: r.RequestID, Method: r.Method, Result: "unresolved", Reason: "attempt-unresolved"}, nil
		}, func(r Request) (Response, error) {
			v := acceptedResponse(r)
			v.Result = "existing-attempt"
			v.Attempt.NativeStage = "native-completed"
			return v, nil
		})
		old, _, err := f.a.Notify(context.Background(), []byte("old"), limit(), time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		f.now.Add(int64(time.Minute))
		current, _, err := f.a.Notify(context.Background(), []byte("new"), limit(), time.Time{})
		if !errors.Is(err, ErrUnresolved) {
			t.Fatal(err)
		}
		before := f.load(t)
		if _, err := f.a.Reconcile(context.Background(), old, limit()); err != nil {
			t.Fatal(err)
		}
		joined(t, done)
		after := f.load(t)
		if !reflect.DeepEqual(before.Notification, after.Notification) || after.Notification.Reservation.Token != current.Token || after.HostEnrollment.Attempts.Entries[1].State != "unknown" || after.NextBatch != before.NextBatch {
			t.Fatal("late observation completed another reservation")
		}
	})
}

// Proves real failed first/second state writes send no request. The directory
// mode fault is confined to the fixture and restored; a failed second write
// retains prepared, which alone is explicitly abandonable before any I/O.
func TestAdapterFailedPersistence(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			withAdapter(t, func(f *adapterFixture) {
				calls := 0
				f.clockHook = func() {
					calls++
					if calls == failAt {
						if err := os.Chmod(filepath.Join(f.paths.Base, "state"), 0500); err != nil {
							t.Error(err)
						}
					}
				}
				key, _, err := f.a.Notify(context.Background(), []byte("notice"), limit(), time.Time{})
				f.clockHook = nil
				if restoreErr := os.Chmod(filepath.Join(f.paths.Base, "state"), 0700); restoreErr != nil {
					t.Fatal(restoreErr)
				}
				if !errors.Is(err, config.ErrHostStateScope) {
					t.Fatalf("failed write admitted I/O: %v", err)
				}
				noRequest(t, f.peer)
				s := f.load(t)
				if failAt == 1 {
					if len(s.HostEnrollment.Attempts.Entries) != 0 || s.Notification.Reservation != nil {
						t.Fatal("failed first write partially persisted")
					}
				} else {
					if s.HostEnrollment.Attempts.Entries[0].State != "prepared" {
						t.Fatal("failed Begin discarded prepared")
					}
					if err := f.a.AbandonPrepared(context.Background(), key, limit()); err != nil {
						t.Fatal(err)
					}
					if f.load(t).Notification.Reservation != nil {
						t.Fatal("pre-I/O abandonment failed")
					}
				}
			})
		})
	}
}

// Proves an accepted response whose Source reconciliation cannot persist is
// returned with a failure, while the durable original UNKNOWN remains fenced.
func TestAdapterPostResponseWriteFailure(t *testing.T) {
	withAdapter(t, func(f *adapterFixture) {
		done := f.serve(func(r Request) (Response, error) {
			err := os.Chmod(filepath.Join(f.paths.Base, "state"), 0500)
			return acceptedResponse(r), err
		})
		_, r, err := f.a.Notify(context.Background(), []byte("notice"), limit(), time.Time{})
		joined(t, done)
		if restoreErr := os.Chmod(filepath.Join(f.paths.Base, "state"), 0700); restoreErr != nil {
			t.Fatal(restoreErr)
		}
		if !errors.Is(err, config.ErrHostStateScope) || r.Result != "accepted" {
			t.Fatalf("unpersisted acceptance misreported: %v %+v", err, r)
		}
		s := f.load(t)
		if s.HostEnrollment.Attempts.Entries[0].State != "unknown" || s.Notification.Reservation == nil {
			t.Fatal("unpersisted acceptance unfenced")
		}
	})
}

// Proves two concurrent Source callers share one reservation and only one
// actual trigger reaches the peer; the losing call cannot replace UNKNOWN.
func TestAdapterConcurrentNotify(t *testing.T) {
	withAdapter(t, func(f *adapterFixture) {
		done := f.serve(func(r Request) (Response, error) {
			return Response{Contract: Contract, RequestID: r.RequestID, Method: r.Method, Result: "unresolved", Reason: "attempt-unresolved"}, nil
		})
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, _, err := f.a.Notify(context.Background(), []byte("notice"), limit(), time.Time{})
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		joined(t, done)
		counts := map[error]int{}
		for err := range errs {
			counts[err]++
		}
		if counts[ErrUnresolved] != 1 || counts[config.ErrNotificationAttempt] != 1 || len(f.load(t).HostEnrollment.Attempts.Entries) != 1 {
			t.Fatalf("concurrent reservation: %+v", counts)
		}
	})
}

// Proves concurrent observers cannot pipeline this endpoint or turn lookup
// into a trigger. The winning exact observation alone completes UNKNOWN.
func TestAdapterConcurrentObservers(t *testing.T) {
	withAdapter(t, func(f *adapterFixture) {
		entered, release := make(chan struct{}), make(chan struct{})
		done := f.serve(func(r Request) (Response, error) {
			return Response{Contract: Contract, RequestID: r.RequestID, Method: r.Method, Result: "unresolved", Reason: "attempt-unresolved"}, nil
		}, func(r Request) (Response, error) {
			if r.Method != "lookup_attempt" {
				return Response{}, errors.New("observer triggered")
			}
			close(entered)
			<-release
			v := acceptedResponse(r)
			v.Result = "existing-attempt"
			return v, nil
		})
		key, _, err := f.a.Notify(context.Background(), []byte("notice"), limit(), time.Time{})
		if !errors.Is(err, ErrUnresolved) {
			t.Fatal(err)
		}
		first := make(chan error, 1)
		go func() { _, err := f.a.Reconcile(context.Background(), key, limit()); first <- err }()
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("first observer did not reach peer")
		}
		_, err = f.a.Reconcile(context.Background(), key, limit())
		close(release)
		if !errors.Is(err, ErrBusy) {
			t.Fatalf("observer pipelined: %v", err)
		}
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		joined(t, done)
		s := f.load(t)
		if len(s.HostEnrollment.Attempts.Entries) != 1 || s.HostEnrollment.Attempts.Entries[0].State != "accepted" || s.Notification.Reservation != nil {
			t.Fatal("observer outcome corrupted")
		}
	})
}

// Proves wrong bindings, invalid notice/deadline/context and filesystem errors
// refuse locally with fixed errors, beside the live fixture used by positives.
func TestAdapterLocalRefusals(t *testing.T) {
	withAdapter(t, func(f *adapterFixture) {
		bad := &Client{binding: f.a.client.binding}
		bad.binding.EnrollmentRef = "wrong-receipt"
		if _, err := NewAdapter(f.paths, "member", limit(), f.attachment, f.attachment.Scope, bad, f.clock); !errors.Is(err, config.ErrHostEnrollment) {
			t.Fatalf("wrong binding admitted: %v", err)
		}
		for _, notice := range [][]byte{nil, {0xff}, bytes.Repeat([]byte{'x'}, MaxFrame+1)} {
			if _, _, err := f.a.Notify(context.Background(), notice, limit(), time.Time{}); !errors.Is(err, ErrProtocol) {
				t.Fatal(err)
			}
		}
		if _, _, err := f.a.Notify(nil, []byte("notice"), limit(), time.Time{}); !errors.Is(err, ErrProtocol) {
			t.Fatal(err)
		}
		if _, _, err := f.a.Notify(context.Background(), []byte("notice"), time.Time{}, time.Time{}); !errors.Is(err, config.ErrLockTimeout) {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := f.a.Notify(ctx, []byte("notice"), limit(), time.Time{}); !errors.Is(err, ErrUnresolved) {
			t.Fatal(err)
		}
		noRequest(t, f.peer)
		if err := os.WriteFile(f.paths.StateFile("member"), []byte("invalid private fixture JSON"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.a.Notify(context.Background(), []byte("notice"), limit(), time.Time{}); err != ErrState {
			t.Fatalf("filesystem detail exposed: %v", err)
		}
		noRequest(t, f.peer)
	})
	if stateError(config.ErrHostStateUncertain) != config.ErrHostStateUncertain || stateError(fmt.Errorf("private-path: %w", config.ErrNotificationSpacing)) != config.ErrNotificationSpacing {
		t.Fatal("public error category lost")
	}
}

// Proves a state-lock wait uses the earlier context deadline, without touching
// the peer or reserving an attempt. The held lock is this fixture's own file.
func TestAdapterStateLockDeadline(t *testing.T) {
	withAdapter(t, func(f *adapterFixture) {
		lock, err := os.OpenFile(filepath.Join(f.paths.Base, "state", "member.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, _, err = f.a.Notify(ctx, []byte("notice"), limit(), time.Time{})
		if !errors.Is(err, config.ErrLockTimeout) || time.Since(start) > 500*time.Millisecond {
			t.Fatalf("context lock deadline ignored: %v %v", err, time.Since(start))
		}
		noRequest(t, f.peer)
		if len(f.load(t).HostEnrollment.Attempts.Entries) != 0 {
			t.Fatal("locked state changed")
		}
	})
}

// Proves expiry observed immediately after the durable Begin leaves UNKNOWN
// and no Host I/O, rather than treating successful persistence as admission.
func TestAdapterExpiryAfterPersistence(t *testing.T) {
	withAdapter(t, func(f *adapterFixture) {
		calls := 0
		f.clockHook = func() {
			calls++
			if calls == 3 {
				f.now.Store(f.attachment.Lease.Expires.UnixNano())
			}
		}
		_, _, err := f.a.Notify(context.Background(), []byte("notice"), limit(), time.Time{})
		f.clockHook = nil
		if !errors.Is(err, config.ErrHostEnrollment) {
			t.Fatalf("post-write expiry admitted: %v", err)
		}
		noRequest(t, f.peer)
		s := f.load(t)
		if s.HostEnrollment.Attempts.Entries[0].State != "unknown" || s.Notification.Reservation == nil {
			t.Fatal("post-write expiry canceled UNKNOWN")
		}
	})
}

// Proves a missing namespace is refused without creating it, and constructor
// arguments cannot manufacture an enrollment. Existing fixture is the positive.
func TestAdapterConstructorRefusals(t *testing.T) {
	withAdapter(t, func(f *adapterFixture) {
		missing := config.Paths{Base: filepath.Join(t.TempDir(), "absent")}
		if _, err := NewAdapter(missing, "member", limit(), f.attachment, f.attachment.Scope, f.a.client, f.clock); !errors.Is(err, config.ErrHostStateScope) {
			t.Fatalf("missing namespace: %v", err)
		}
		if _, err := os.Stat(missing.Base); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("constructor provisioned missing namespace")
		}
		if _, err := NewAdapter(f.paths, "member", limit(), f.attachment, f.attachment.Scope, nil, f.clock); !errors.Is(err, config.ErrHostEnrollment) {
			t.Fatal(err)
		}
		if _, err := NewAdapter(f.paths, "member", limit(), f.attachment, f.attachment.Scope, f.a.client, nil); !errors.Is(err, config.ErrHostEnrollment) {
			t.Fatal(err)
		}
		if _, err := NewAdapter(f.paths, "foreign", limit(), f.attachment, f.attachment.Scope, f.a.client, f.clock); !errors.Is(err, config.ErrHostEnrollment) {
			t.Fatal(err)
		}
		noRequest(t, f.peer)
	})
}
