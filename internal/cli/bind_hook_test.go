package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// A synthetic runtime executable owns a Python terminal fixture and invokes
// real public hooks below. It never loads a provider, token or live pane.
func TestAutoBindingRuntimeChild(t *testing.T) {
	if os.Getenv("A2A_AUTO_FIXTURE") != "1" {
		return
	}
	cmd := exec.Command("python3", os.Getenv("AUTO_SCRIPT"), os.Getenv("AUTO_DIR"), os.Getenv("AUTO_EXE"))
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// Real SessionStart under each runtime ancestor binds without agent action.
// Repeated startup/resume/compact/clear preserve generation/PID; prompt/Stop
// recover a killed consumer or lost binding. A briefly live prior consumer
// drains after owner death; a stuck one refuses within the hook budget. Opposite wait
// admission refuses, explicit unbind remains stopped, and no mailbox receipts
// are invented by binding. An unsupported parent is covered by the ancestry controls.
func TestHookAutomaticBindingLifecycle(t *testing.T) {
	testHookAutomaticBindingLifecycle(t, "")
}

// The real prompt hook replaces the dead consumer. Both acknowledged and
// server-applied/truncated-ACK incidents retain their pinned message and txn
// on Claude and Codex, without granting the new generation Enter authority.
// A real SessionStart with a different loaded session retires the old episode.
func TestHookStrandedReportConsumerRecovery(t *testing.T) {
	for _, mode := range []string{"acknowledged", "lost-ack", "new-session"} {
		t.Run(mode, func(t *testing.T) { testHookAutomaticBindingLifecycle(t, mode) })
	}
}

func testHookAutomaticBindingLifecycle(t *testing.T, incidentMode string) {
	if testing.Short() {
		t.Skip("private terminal integration")
	}
	for _, tool := range []string{"tmux", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	dir, err := os.MkdirTemp("/tmp", "ah-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	exe := filepath.Join(dir, "a2a")
	if raw, err := exec.Command("go", "build", "-o", exe, "../../cmd/a2a").CombinedOutput(); err != nil {
		t.Fatalf("CLI build: %v %s", err, raw)
	}
	self, _ := os.Executable()
	runtimeBytes, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	for _, runtime := range []string{"claude", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			e, _ := world(t)

			e.extra[EnvSessionID] = "auto-fixture"
			e.as("alice", "wait", "--timeout", "1ms").ok(t)
			home := filepath.Join(dir, runtime)
			os.Mkdir(home, 0700)
			runtimeExe := filepath.Join(home, runtime)
			if err := os.WriteFile(runtimeExe, runtimeBytes, 0700); err != nil {
				t.Fatal(err)
			}
			script := bindFixture
			script = strings.Replace(script, "import os,sys,tty,json,select,subprocess", "import os,sys,tty,json,select,subprocess,time", 1)
			script = strings.Replace(script, "text='';submitted=[]", "text='';submitted=[];session='auto-fixture'", 1)
			start := strings.Index(script, "        args=[exe,")
			end := strings.Index(script[start:], "        with open(root+'/result.next'") + start
			script = script[:start] + `        if request in (13,17): session='auto-fixture-clear'+str(request)
        event={7:'prompt',8:'stop',9:'prompt',10:'stop',11:'prompt',12:'stop',14:'prompt'}.get(request,'session-start')
        args=[exe,'--as','alice','hook',event,'--runtime','` + runtime + `']
        source=['startup','resume','compact','clear'][min(request-1,3)]
        current='auto-fixture' if request==14 else session
        inherited='auto-fixture' if request==13 else current
        hookenv={**os.environ,'A2A_SESSION_ID':inherited}
        if '` + runtime + `'=='codex': hookenv['CODEX_THREAD_ID']=inherited
        started=time.monotonic()
        try:
            p=subprocess.run(args,input=json.dumps({'session_id':current,'source':source}),env=hookenv,capture_output=True,text=True,timeout=8)
            timedout=False
        except subprocess.TimeoutExpired as error:
            def decoded(value): return value.decode(errors='replace') if isinstance(value,bytes) else (value or '')
            p=subprocess.CompletedProcess(args,124,decoded(error.stdout),'fixture hook subprocess timed out; '+decoded(error.stderr))
            timedout=True
        with open(root+'/hook-observation.next','w') as f: json.dump({'request':request,'seconds':time.monotonic()-started,'timeout':timedout,'exit':p.returncode,'stderr':p.stderr},f)
        os.replace(root+'/hook-observation.next',root+'/hook-observation.json')
` + script[end:]
			// Build the Claude draw directly: Go raw strings retain Python escapes.
			if runtime == "claude" {
				a := strings.Index(script, "def draw():")
				b := strings.Index(script[a:], "draw()\nwhile") + a
				script = script[:a] + `def draw():
    os.write(1,('\x1b[2J\x1b[19;1H'+'─'*100+'\x1b[20;1H❯ '+text+'\x1b[21;1H'+'─'*100+'\x1b[22;1H  bypass permissions on\x1b[20;'+str(3+len(text))+'H').encode());save()
` + script[b:]
			}
			scriptPath := filepath.Join(home, "fixture.py")
			if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
				t.Fatal(err)
			}
			socket := filepath.Join(home, "socket")
			cmd := exec.Command("tmux", "-S", socket, "-f", "/dev/null", "new-session", "-d", "-x", "120", "-y", "24", runtimeExe, "-test.run=^TestAutoBindingRuntimeChild$")
			thread := ""
			if runtime == "codex" {
				thread = "auto-fixture"
			}
			env := append(os.Environ(), "A2A_CONFIG_DIR="+e.dir, "A2A_AGENT=alice", "A2A_SESSION_ID=auto-fixture", "CODEX_THREAD_ID="+thread, "A2A_SECRET_STORE=file", "CLAUDE_ENV_FILE=", "CODEX_ENV_FILE=", "A2A_AUTO_FIXTURE=1", "AUTO_SCRIPT="+scriptPath, "AUTO_DIR="+home, "AUTO_EXE="+exe)
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("tmux: %v %s", err, out)
			}
			raw, err := exec.Command("tmux", "-S", socket, "display-message", "-p", "#{pid}").Output()
			if err != nil {
				t.Fatal(err)
			}
			serverPID, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
			// Retain this private synthetic pane on failure so its traceback
			// explains an absent result; no live/provider pane is captured.
			if out, err := exec.Command("tmux", "-S", socket, "set-option", "-w", "remain-on-exit", "on").CombinedOutput(); err != nil {
				t.Fatalf("fixture diagnostics: %v %s", err, out)
			}
			t.Cleanup(func() {
				st, _ := config.LoadState(e.paths(), "alice")
				if st.Terminal != nil {
					birth, _ := tmuxwake.Birth(context.Background(), st.Terminal.DaemonPID)
					if birth != "" && birth == st.Terminal.DaemonBirth {
						p, _ := os.FindProcess(st.Terminal.DaemonPID)
						p.Signal(syscall.SIGTERM)
						// The owned daemon persists its handled exit. Join that
						// process before testing.TempDir removes its config;
						// signalling alone races the final state rename/write.
						until := time.Now().Add(5 * time.Second)
						for !processGone(st.Terminal.DaemonPID) && time.Now().Before(until) {
							time.Sleep(10 * time.Millisecond)
						}
						if !processGone(st.Terminal.DaemonPID) {
							t.Errorf("owned fixture daemon cleanup did not finish: pid=%d", st.Terminal.DaemonPID)
						}
					}
				}
				p, _ := os.FindProcess(serverPID)
				p.Signal(syscall.SIGTERM)
			})
			request := func(id int) {
				t.Helper()
				data, _ := json.Marshal(id)
				if err := writeBindingFixtureCommand(home, data); err != nil {
					t.Fatal(err)
				}
				until := time.Now().Add(12 * time.Second)
				for time.Now().Before(until) {
					data, err := os.ReadFile(filepath.Join(home, "result.json"))
					var v struct {
						Code     int
						Out, Err string
						Request  int
					}
					if err == nil && json.Unmarshal(data, &v) == nil && v.Request == id {
						want := ""
						if id == 5 || id == 17 {
							want = "explicitly stopped"
						}
						if id == 11 {
							want = "prior bound consumer still stopping"
						}
						if id == 14 {
							want = "session already bound"
						}
						if v.Code != 0 || want == "" && v.Err != "" || want != "" && !strings.Contains(v.Err, want) {
							observation, _ := os.ReadFile(filepath.Join(home, "hook-observation.json"))
							t.Fatalf("hook %d: code%d %s %s; observation: %s", id, v.Code, v.Out, v.Err, observation)
						}
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
				// This server contains only the synthetic fixture. Preserve its
				// exception/output when a missing result would hide the cause.
				pane, _ := exec.Command("tmux", "-S", socket, "capture-pane", "-p").Output()
				observation, _ := os.ReadFile(filepath.Join(home, "hook-observation.json"))
				t.Fatalf("hook %d did not return; observation: %s; private fixture output: %s", id, observation, pane)
			}
			// Both previous owner and remaining consumer are task-owned OS
			// processes; no signal is sent by the product's automatic bind.
			gone := exec.Command("sleep", "0.3")
			if err := gone.Start(); err != nil {
				t.Fatal(err)
			}
			goneBirth, err := tmuxwake.Birth(context.Background(), gone.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			gone.Wait()
			priorDone := map[int]chan struct{}{}
			prior := func(duration string) *exec.Cmd {
				t.Helper()
				p := exec.Command("sleep", duration)
				if err := p.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan struct{})
				priorDone[p.Process.Pid] = done
				go func() { p.Wait(); close(done) }()
				t.Cleanup(func() { p.Process.Kill(); <-done })
				birth, err := tmuxwake.Birth(context.Background(), p.Process.Pid)
				if err != nil {
					t.Fatal(err)
				}
				_, err = config.UpdateState(e.paths(), "alice", func(st *config.State) error {
					st.Terminal = &config.TerminalBinding{Generation: "prior-fixture", SessionID: "auto-fixture", Host: machineLabel(), OwnerPID: gone.Process.Pid, OwnerBirth: goneBirth, DaemonPID: p.Process.Pid, DaemonBirth: birth, Interval: time.Second}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				return p
			}
			prior("0.5")
			request(1)
			st := awaitBinding(t, e.paths(), func(st config.State) bool { return st.Terminal != nil && st.Terminal.Outcome == "armed: no new mail" })
			generation, pid := st.Terminal.Generation, st.Terminal.DaemonPID
			for _, n := range []int{2, 3, 4} {
				request(n)
				b := e.state("alice").Terminal
				if b.Generation != generation || b.DaemonPID != pid {
					t.Fatal("SessionStart replaced generation")
				}
			}
			// A killed own fixture consumer is not a live binding: prompt
			// must recover it without requiring an agent to run bind/unbind.
			oldPID := pid
			dead, _ := os.FindProcess(oldPID)
			dead.Signal(syscall.SIGTERM)
			until := time.Now().Add(5 * time.Second)
			for !processGone(oldPID) && time.Now().Before(until) {
				time.Sleep(10 * time.Millisecond)
			}
			if !processGone(oldPID) {
				t.Fatal("owned fixture consumer did not exit")
			}
			if incidentMode != "" {
				// Prepare the persisted, already
				// reported stranded episode after stopping this fixture's daemon.
				// The real prompt hook must recover its process without duplicating
				// the incident for the unchanged session, target and suspicion.
				systemRoom := e.ownReportRoom("alice")
				if out, err := exec.Command("tmux", "-S", socket, "send-keys", "-t", "%0", "-l", tmuxwake.Notice(1)).CombinedOutput(); err != nil {
					t.Fatalf("synthetic notice: %v %s", err, out)
				}
				since := time.Now().Add(-11 * time.Minute).UTC().Format(time.RFC3339Nano)
				_, err = config.UpdateState(e.paths(), "alice", func(st *config.State) error {
					yes := true
					st.Terminal.Stopped = false
					st.Terminal.Typed = true
					st.Terminal.Notice = tmuxwake.Notice(1)
					st.Terminal.NoticeCount = 1
					// Keep the real binding policy valid while bounding this retry control.
					st.Terminal.RetryMax = st.Terminal.Interval
					st.Terminal.OwnNotice = config.OwnNoticeObservation{PossiblyOrphaned: &yes, Since: since, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Reason: orphanReason}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				reviewApp := &App{as: "alice", Getenv: func(k string) string {
					if k == config.EnvConfigDir {
						return e.dir
					}
					return e.extra[k]
				}, Now: time.Now, Stdout: io.Discard, Stderr: io.Discard, HTTP: e.hs.srv.Client()}
				reviewSession, err := reviewApp.session()
				if err != nil {
					t.Fatal(err)
				}
				var lostACK atomic.Bool
				if incidentMode == "lost-ack" {
					// Apply through the real fake homeserver over HTTP, then
					// truncate its successful response. The production reporter
					// must persist its actual ambiguous failure; no state rewrite
					// manufactures a pre-ACK journal.
					proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/send/m.room.message/") {
							rec := httptest.NewRecorder()
							e.hs.srv.Config.Handler.ServeHTTP(rec, r)
							if rec.Code != http.StatusOK {
								t.Errorf("setup: incident apply returned %d", rec.Code)
								w.WriteHeader(rec.Code)
								return
							}
							lostACK.Store(true)
							w.Header().Set("Content-Length", "100")
							w.WriteHeader(http.StatusOK)
							_, _ = w.Write([]byte(`{"event_id":"`))
							return
						}
						e.hs.srv.Config.Handler.ServeHTTP(w, r)
					}))
					defer proxy.Close()
					reviewSession.client.BaseURL = proxy.URL
				}
				reportErr := reviewApp.reportStrandedTerminalNotice(context.Background(), reviewSession, generation)
				beforeReport := e.state("alice").Terminal.NoticeReport
				if beforeReport == nil || len(e.projectionsFrom(systemRoom, "@alice:test.local")) != 1 {
					t.Fatal("first incident was not server-applied")
				}
				if incidentMode == "lost-ack" {
					if !lostACK.Load() || reportErr == nil || reportErr.Error() != "unavailable: system incident report failed" || beforeReport.Sent || beforeReport.RetryAt.IsZero() {
						t.Fatal("truncated response did not persist the intended ambiguous report state", reportErr, beforeReport)
					}
				} else if reportErr != nil || !beforeReport.Sent {
					t.Fatal("acknowledged positive control did not persist ACK", reportErr)
				}

				if incidentMode == "new-session" {
					request(13) // actual SessionStart clear/new ID on the same pane
					next := awaitBinding(t, e.paths(), func(st config.State) bool {
						return st.Terminal != nil && st.Terminal.DaemonPID != oldPID && st.Terminal.SessionID == "auto-fixture-clear13" && st.Terminal.OwnNotice.PossiblyOrphaned != nil
					}).Terminal
					if next.Generation == generation || next.NoticeReport != nil || next.OwnNotice.Since == "" || next.OwnNotice.Since == beforeReport.Since || len(e.projectionsFrom(systemRoom, "@alice:test.local")) != 1 {
						t.Fatal("new loaded session inherited the prior incident episode/report")
					}
					return
				}

				request(7)
				recovered := awaitBinding(t, e.paths(), func(st config.State) bool {
					return st.Terminal != nil && st.Terminal.DaemonPID != oldPID && st.Terminal.NoticeReport != nil && st.Terminal.NoticeReport.Sent
				})
				if got := len(e.projectionsFrom(systemRoom, "@alice:test.local")); got != 1 || recovered.Terminal.NoticeReport.Message.Txn != beforeReport.Message.Txn {
					t.Fatalf("same-session consumer restart duplicated incident: messages=%d oldTxn=%s newTxn=%s oldSince=%s newSince=%s oldGeneration=%s newGeneration=%s", got, beforeReport.Message.Txn, recovered.Terminal.NoticeReport.Message.Txn, beforeReport.Since, recovered.Terminal.NoticeReport.Since, generation, recovered.Terminal.Generation)
				}
				if recovered.Terminal.Generation == generation || recovered.Terminal.DaemonPID == oldPID ||
					recovered.Terminal.NoticeReport.Since != beforeReport.Since ||
					recovered.Terminal.NoticeReport.Message.Room != beforeReport.Message.Room ||
					!reflect.DeepEqual(recovered.Terminal.NoticeReport.Message.Content, beforeReport.Message.Content) ||
					recovered.Terminal.NotificationToken != "" {
					t.Fatal("recovery changed incident or inherited submission authority")
				}
				return

			}
			request(7)
			recovered := awaitBinding(t, e.paths(), func(st config.State) bool {
				return st.Terminal != nil && st.Terminal.DaemonPID != oldPID && st.Terminal.Outcome == "armed: no new mail"
			})
			generation, pid = recovered.Terminal.Generation, recovered.Terminal.DaemonPID
			_, err = config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Terminal = nil; return nil })
			if err != nil {
				t.Fatal(err)
			}
			until = time.Now().Add(5 * time.Second)
			for !processGone(pid) && time.Now().Before(until) {
				time.Sleep(10 * time.Millisecond)
			}
			if !processGone(pid) {
				t.Fatal("vanished binding left consumer alive")
			}
			request(8)
			recovered = awaitBinding(t, e.paths(), func(st config.State) bool { return st.Terminal != nil && st.Terminal.Outcome == "armed: no new mail" })
			pid = recovered.Terminal.DaemonPID
			dead, _ = os.FindProcess(pid)
			dead.Signal(syscall.SIGTERM)
			until = time.Now().Add(5 * time.Second)
			for !processGone(pid) && time.Now().Before(until) {
				time.Sleep(10 * time.Millisecond)
			}
			if !processGone(pid) {
				t.Fatal("owned replacement consumer did not exit")
			}
			stuck := prior("30")
			request(11)
			if b := e.state("alice").Terminal; b.Generation != "prior-fixture" || b.DaemonPID != stuck.Process.Pid || processGone(stuck.Process.Pid) {
				t.Fatal("bounded refusal changed or signalled prior consumer")
			}
			stuck.Process.Kill()
			<-priorDone[stuck.Process.Pid]
			request(12)
			recovered = awaitBinding(t, e.paths(), func(st config.State) bool { return st.Terminal != nil && st.Terminal.Outcome == "armed: no new mail" })
			generation, pid = recovered.Terminal.Generation, recovered.Terminal.DaemonPID

			// Claude clear uses a NEW ID with the same runtime/pane. Only
			// SessionStart transfers the lease; old commands/hooks cannot.
			request(13)
			b := e.state("alice").Terminal
			if b.SessionID != "auto-fixture-clear13" || b.Generation != generation || b.DaemonPID != pid || e.state("alice").Lease.Holder != b.SessionID {
				t.Fatal("clear lost binding or lease handover")
			}
			e.extra[EnvSessionID] = "auto-fixture"
			e.as("alice", "inbox").failsWith(t, ExitError, "is held by auto-fixture-clear13")
			e.as("alice", "unbind").failsWith(t, ExitError, "another session")
			request(14)
			if e.state("alice").Terminal.SessionID != "auto-fixture-clear13" {
				t.Fatal("old prompt took back clear session")
			}
			e.extra[EnvSessionID] = "auto-fixture-clear13"
			e.as("alice", "inbox").ok(t)
			e.extra[EnvSessionID] = "auto-fixture-clear13"
			// A live automatic binding refuses a second consumer with the
			// binding diagnosis, without advising removal of its own consumer.
			refused := e.as("alice", "wait", "--timeout", "1ms").failsWith(t, ExitError, "same identity has a live binding")
			for _, want := range []string{"a2a bind status", "a2a inbox"} {
				if !strings.Contains(refused.stderr, want) {
					t.Fatalf("bound refusal lacks %q: %s", want, refused.stderr)
				}
			}
			for _, forbidden := range []string{"live wait/watch", "stop own waiter", "unbind", "kill"} {
				if strings.Contains(refused.stderr, forbidden) {
					t.Fatalf("bound refusal gives misleading %q advice: %s", forbidden, refused.stderr)
				}
			}
			stop := exec.Command(exe, "--as", "alice", "unbind")
			stop.Env = append(env, "A2A_SESSION_ID=auto-fixture-clear13", "CODEX_THREAD_ID=")
			if out, err := stop.CombinedOutput(); err != nil {
				t.Fatalf("unbind: %v %s", err, out)
			}
			request(5)
			request(17)
			request(9)
			request(10)
			b = e.state("alice").Terminal
			if !b.Stopped || b.Generation != generation || b.DaemonPID != pid {
				t.Fatal("hook revived explicit unbind")
			}
			if b.SessionID != "auto-fixture-clear17" || e.state("alice").Lease.Holder != b.SessionID {
				t.Fatal("unbind-then-clear lost stopped session handover")
			}
			e.extra[EnvSessionID] = "auto-fixture-clear17"
			e.as("alice", "wait", "--timeout", "1ms").ok(t)
		})
	}
}

// Retirement and cancellation have distinct fixed reasons, with no network or
// terminal effects. This includes loss of the field by a legacy schema writer.
func TestBindingExitReasons(t *testing.T) {
	for _, c := range []struct {
		name string
		b    *config.TerminalBinding
		want string
	}{{"lost", nil, "binding vanished"}, {"replacement", &config.TerminalBinding{Generation: "other"}, "generation replaced"}, {"stopped", &config.TerminalBinding{Generation: "own", Stopped: true}, "stopped"}} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.agent("alice")
			_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Terminal = c.b; return nil })
			if err != nil {
				t.Fatal(err)
			}
			a := NewApp()
			a.as = "alice"
			a.Getenv = func(k string) string {
				if k == config.EnvConfigDir {
					return e.dir
				}
				return ""
			}
			a.Stdout = io.Discard
			a.Stderr = io.Discard
			s, err := a.session()
			if err != nil {
				t.Fatal(err)
			}
			reason, err := a.runTerminal(context.Background(), s, "own", config.TerminalBinding{})
			if err != nil || reason != c.want {
				t.Fatalf("got %q %v", reason, err)
			}
		})
	}
}
