package hostnotification

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/relux-works/a2a-swarm/internal/config"
)

const dockContract = "source-host-dock/1"
const dockFrameLimit = 8192

var errDockProtocol = errors.New("counterpart fixture control refused")
var errDockArtifact = errors.New("counterpart fixture artifact refused")
var errDockProcess = errors.New("counterpart fixture process failed")

type dockSetup struct {
	Contract      string                         `json:"contract"`
	Kind          string                         `json:"kind"`
	Case          string                         `json:"case"`
	Binding       config.HostNotificationBinding `json:"binding"`
	CallTimeoutMS int                            `json:"call_timeout_ms"`
}
type dockReady struct {
	Contract string                         `json:"contract"`
	Kind     string                         `json:"kind"`
	Binding  config.HostNotificationBinding `json:"binding"`
}
type dockResult struct {
	Contract      string                         `json:"contract"`
	Kind          string                         `json:"kind"`
	Case          string                         `json:"case"`
	Binding       config.HostNotificationBinding `json:"binding"`
	IntentCount   int                            `json:"intent_count"`
	NoticeID      string                         `json:"notice_id"`
	Sequence      string                         `json:"sequence"`
	NativeStage   string                         `json:"native_stage"`
	WakeCount     int                            `json:"wake_count"`
	BindingActive bool                           `json:"binding_active"`
	InputFenced   bool                           `json:"input_fenced"`
}

// This is a fixture artifact check, not trust discovery. The runner must first
// accept its counterpart's exact source/test/binary handoff. No helper is found
// on PATH, guessed from a checkout or admitted by its own declaration.
func openDockArtifact(path, digest string) (*os.File, error) {
	if !filepath.IsAbs(path) || len(digest) != 64 || strings.ToLower(digest) != digest {
		return nil, errDockArtifact
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return nil, errDockArtifact
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Size() < 1 || info.Size() > 64<<20 {
		return nil, errDockArtifact
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errDockArtifact
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0111 == 0 || opened.Size() != info.Size() {
		f.Close()
		return nil, errDockArtifact
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, (64<<20)+1))
	if err != nil || n != info.Size() || hex.EncodeToString(h.Sum(nil)) != digest {
		f.Close()
		return nil, errDockArtifact
	}
	return f, nil
}

func checkDockArtifact(path, digest string) error {
	f, err := openDockArtifact(path, digest)
	if err != nil {
		return err
	}
	return f.Close()
}

// Linux executes the hashed descriptor at fd 5. Darwin cannot exec /dev/fd/N,
// so execute a bounded, digest-checked copy from that same open descriptor in a
// fresh private directory. Never reopen the supplied pathname. The supplier
// must keep its artifact immutable; this is not isolation from same-UID peers.
// The caller owns cleanup until the child has been reaped, including failures.
func dockCommand(ctx context.Context, artifact, wire, control *os.File, digest string, args ...string) (*exec.Cmd, func(), error) {
	if runtime.GOOS == "linux" {
		cmd := exec.CommandContext(ctx, "/dev/fd/5", args...)
		cmd.ExtraFiles = []*os.File{wire, control, artifact}
		return cmd, func() {}, nil
	}
	if runtime.GOOS != "darwin" {
		return nil, nil, errDockArtifact
	}
	path, cleanup, err := copyDockArtifact(artifact, digest)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.ExtraFiles = []*os.File{wire, control}
	return cmd, cleanup, nil
}

func copyDockArtifact(artifact *os.File, digest string) (string, func(), error) {
	info, err := artifact.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Size() < 1 || info.Size() > 64<<20 {
		return "", nil, errDockArtifact
	}
	dir, err := os.MkdirTemp("", "a2a-dock-")
	if err != nil {
		return "", nil, errDockArtifact
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	output, err := os.CreateTemp(dir, "artifact-")
	if err != nil {
		cleanup()
		return "", nil, errDockArtifact
	}
	h := sha256.New()
	// ReadAt starts at zero even though admission left the descriptor at EOF.
	n, copyErr := io.Copy(io.MultiWriter(output, h), io.NewSectionReader(artifact, 0, (64<<20)+1))
	valid := copyErr == nil && n == info.Size() && n <= 64<<20 && hex.EncodeToString(h.Sum(nil)) == digest
	var modeErr error
	if valid {
		modeErr = output.Chmod(0o700)
	}
	closeErr := output.Close()
	if !valid || modeErr != nil || closeErr != nil {
		cleanup()
		return "", nil, errDockArtifact
	}
	return output.Name(), cleanup, nil
}

func TestDockPinnedArtifactHelper(t *testing.T) {
	if os.Getenv("A2A_TEST_DOCK_PINNED_HELPER") != "1" {
		return
	}
	_, _ = os.Stdout.WriteString("descriptor-original\n")
	os.Exit(0)
}

// A real inert child still executes the hashed binary after its pathname is
// replaced. This exercises the same fd layout as the optional docking runner.
func TestDockArtifactPinnedExecution(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	path := filepath.Join(t.TempDir(), "fixture.test")
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(output, h), input)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copy fixture: %v %v", copyErr, closeErr)
	}
	digest := hex.EncodeToString(h.Sum(nil))
	artifact, err := openDockArtifact(path, digest)
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Close()
	if err := os.Rename(path, path+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'replacement\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	placeholder, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer placeholder.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd, cleanup, err := dockCommand(ctx, artifact, placeholder, placeholder, digest, "-test.run=^TestDockPinnedArtifactHelper$")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if runtime.GOOS == "darwin" && (strings.HasPrefix(cmd.Path, "/dev/fd/") || len(cmd.ExtraFiles) != 2) {
		t.Fatal("Darwin must execute the private copy with only the protocol endpoints")
	}
	if runtime.GOOS == "linux" && (cmd.Path != "/dev/fd/5" || len(cmd.ExtraFiles) != 3) {
		t.Fatal("Linux must execute the admitted descriptor")
	}
	cmd.Env = []string{"A2A_TEST_DOCK_PINNED_HELPER=1"}
	got, err := cmd.CombinedOutput()
	if err != nil || string(got) != "descriptor-original\n" {
		t.Fatalf("pinned artifact execution on %s: %q %v", runtime.GOOS, got, err)
	}
	cleanup()
	if runtime.GOOS == "darwin" {
		if _, err := os.Lstat(filepath.Dir(cmd.Path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("private execution directory retained")
		}
	}
}

// Exercise copying on both hosted platforms, including in-place mutation
// refusal. Path substitution must still copy the admitted original descriptor.
func TestDockArtifactCopyAdmission(t *testing.T) {
	digest := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	path := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(path, []byte("abc"), 0o700); err != nil {
		t.Fatal(err)
	}
	artifact, err := openDockArtifact(path, digest)
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Close()
	if err := os.Rename(path, path+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o700); err != nil {
		t.Fatal(err)
	}
	copyPath, cleanup, err := copyDockArtifact(artifact, digest)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	data, err := os.ReadFile(copyPath)
	if err != nil || string(data) != "abc" {
		t.Fatal("copy did not retain admitted bytes")
	}
	for _, p := range []string{copyPath, filepath.Dir(copyPath)} {
		info, err := os.Lstat(p)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatal("copy namespace is not private 0700")
		}
	}
	cleanup()
	if _, err := os.Lstat(filepath.Dir(copyPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("copy namespace retained")
	}
	t.Setenv("TMPDIR", t.TempDir())
	refuse := func() {
		t.Helper()
		p, clean, err := copyDockArtifact(artifact, digest)
		if clean != nil {
			defer clean()
		}
		if err != errDockArtifact || p != "" {
			t.Fatal("changed or invalid artifact copied")
		}
		entries, err := os.ReadDir(os.TempDir())
		if err != nil || len(entries) != 0 {
			t.Fatal("refused copy retained temporary files")
		}
	}
	if err := os.WriteFile(path+"-original", []byte("abd"), 0o700); err != nil {
		t.Fatal(err)
	}
	refuse()
	if err := os.Truncate(path+"-original", (64<<20)+1); err != nil {
		t.Fatal(err)
	}
	refuse()
	if err := artifact.Close(); err != nil {
		t.Fatal(err)
	}
	refuse()
}

// Control messages are fixture metadata, never raw stores or peer payloads.
func decodeDockControl(data []byte, value any, keys ...string) error {
	if len(data) < 3 || len(data) > dockFrameLimit || data[len(data)-1] != '\n' || data[len(data)-2] != '}' || !utf8.Valid(data) || bytes.Contains(data[:len(data)-1], []byte{'\n'}) {
		return errDockProtocol
	}
	var tree map[string]any
	if config.DecodeConnectionJSON(data, value) != nil || json.Unmarshal(data, &tree) != nil || !boundedJSON(tree, 1) || !exact(tree, keys...) || !bindingShape(tree["binding"]) {
		return errDockProtocol
	}
	return nil
}
func decodeDockReady(data []byte, binding config.HostNotificationBinding) error {
	var ready dockReady
	if decodeDockControl(data, &ready, "contract", "kind", "binding") != nil || ready.Contract != dockContract || ready.Kind != "ready" || ready.Binding != binding {
		return errDockProtocol
	}
	return nil
}
func decodeDockResult(data []byte, binding config.HostNotificationBinding, mode string) (dockResult, error) {
	var r dockResult
	if decodeDockControl(data, &r, "contract", "kind", "case", "binding", "intent_count", "notice_id", "sequence", "native_stage", "wake_count", "binding_active", "input_fenced") != nil || r.Contract != dockContract || r.Kind != "result" || r.Binding != binding || r.Case != mode || !oneOf(mode, "accepted-lookup", "input-not-ready") || r.IntentCount < 0 || r.IntentCount > 32 || r.WakeCount < 0 || r.WakeCount > 32 {
		return dockResult{}, errDockProtocol
	}
	if r.IntentCount == 0 {
		if r.NoticeID != "" || r.Sequence != "" || r.NativeStage != "" {
			return dockResult{}, errDockProtocol
		}
	} else {
		seq, err := strconv.ParseUint(r.Sequence, 10, 64)
		if err != nil || seq == 0 || strconv.FormatUint(seq, 10) != r.Sequence || !noticeID(r.NoticeID) || !oneOf(r.NativeStage, "not-evidenced", "write-unresolved", "native-accepted", "native-completed") {
			return dockResult{}, errDockProtocol
		}
	}
	return r, nil
}
func dockDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(2 * time.Second)
	if end, ok := ctx.Deadline(); ok {
		deadline = minTime(deadline, end)
	}
	return deadline
}
func readDockControl(ctx context.Context, conn *net.UnixConn) ([]byte, error) {
	if conn.SetReadDeadline(dockDeadline(ctx)) != nil {
		return nil, errDockProtocol
	}
	var one [1]byte
	data := make([]byte, 0, dockFrameLimit)
	for len(data) < dockFrameLimit {
		if _, err := io.ReadFull(conn, one[:]); err != nil {
			return nil, errDockProtocol
		}
		data = append(data, one[0])
		if one[0] == '\n' {
			return data, nil
		}
	}
	return nil, errDockProtocol
}
func writeDockSetup(ctx context.Context, conn *net.UnixConn, binding config.HostNotificationBinding, mode string) error {
	data, err := json.Marshal(dockSetup{Contract: dockContract, Kind: "setup", Case: mode, Binding: binding, CallTimeoutMS: 2000})
	if err != nil || len(data)+1 > dockFrameLimit || conn.SetWriteDeadline(dockDeadline(ctx)) != nil {
		return errDockProtocol
	}
	data = append(data, '\n')
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil || n < 1 {
			return errDockProtocol
		}
		data = data[n:]
	}
	return nil
}

type dockOutput struct {
	bytes int
	kept  []byte
}

func (w *dockOutput) Write(p []byte) (int, error) {
	w.bytes += len(p)
	remaining := dockFrameLimit - len(w.kept)
	if remaining > len(p) {
		remaining = len(p)
	}
	if remaining > 0 {
		w.kept = append(w.kept, p[:remaining]...)
	}
	return len(p), nil
}

// This explicit opt-in runs only an owner-supplied, pinned inert test binary.
// Without it, the joint engine/client check is SKIPPED/UNQUALIFIED. Ordinary
// unit/race tests and their green result do not stand in for this docking test.
func TestHostCounterpartDock(t *testing.T) {
	path, hasPath := os.LookupEnv("A2A_TEST_HOST_HELPER")
	digest, hasDigest := os.LookupEnv("A2A_TEST_HOST_HELPER_SHA256")
	if !hasPath && !hasDigest {
		t.Skip("counterpart artifact not supplied; joint docking unqualified")
	}
	if !hasPath || !hasDigest || checkDockArtifact(path, digest) != nil {
		t.Fatal(errDockArtifact)
	}
	for _, mode := range []string{"accepted-lookup", "input-not-ready"} {
		t.Run(mode, func(t *testing.T) {
			withAdapter(t, func(f *adapterFixture) {
				if err := runDockCase(t, f, path, digest, mode); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func runDockCase(t *testing.T, f *adapterFixture, path, digest, mode string) error {
	t.Helper()
	artifact, err := openDockArtifact(path, digest)
	if err != nil {
		return err
	}
	defer artifact.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	control, childControl := socketPair(t, syscall.SOCK_STREAM)
	wireFile, err := f.peer.File()
	if err != nil {
		return errDockProcess
	}
	defer wireFile.Close()
	controlFile, err := childControl.File()
	if err != nil {
		return errDockProcess
	}
	defer controlFile.Close()
	cmd, cleanup, err := dockCommand(ctx, artifact, wireFile, controlFile, digest, "-test.run=^TestSourceNotificationDockHelper$", "-test.count=1", "-test.timeout=20s")
	if err != nil {
		return err
	}
	defer cleanup()
	cmd.Env = []string{"HOST_NOTIFICATION_DOCK_HELPER=1"}
	cmd.Dir = t.TempDir()
	stdout, stderr := &dockOutput{}, &dockOutput{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 2 * time.Second
	if cmd.Start() != nil {
		return errDockProcess
	}
	pid := cmd.Process.Pid
	t.Logf("started inert counterpart helper_pid=%d helper_sha256=%s", pid, digest)
	waited := false
	defer func() {
		_ = f.a.client.Close()
		_ = control.Close()
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Logf("closed failed fixture helper_pid=%d reaped=true", pid)
		}
	}()
	// Parent drops every duplicate of the child's endpoints immediately.
	_ = wireFile.Close()
	_ = controlFile.Close()
	_ = f.peer.Close()
	_ = childControl.Close()
	binding := f.a.client.binding
	setupCtx, endSetup := context.WithDeadline(ctx, dockDeadline(ctx))
	defer endSetup()
	if writeDockSetup(setupCtx, control, binding, mode) != nil {
		return errDockProtocol
	}
	frame, err := readDockControl(setupCtx, control)
	if err != nil || decodeDockReady(frame, binding) != nil {
		return errDockProtocol
	}
	endSetup()
	id, err := NewRequestID()
	if err != nil {
		return err
	}
	view, err := f.a.client.Call(ctx, id, "observe_binding", "")
	gate := "open"
	if mode == "input-not-ready" {
		gate = "input-not-ready"
	}
	if err != nil || view.View == nil || view.View.Gate != gate {
		return errDockProtocol
	}
	// The real adapter calls its clock after both state writes and before Call.
	// Read the actual file at that point, independently of the Host's report.
	clockCalls, probed := 0, false
	var probeErr error
	f.clockHook = func() {
		clockCalls++
		if clockCalls != 3 {
			return
		}
		s, err := config.LoadState(f.paths, "member")
		if err != nil || s.HostEnrollment == nil || len(s.HostEnrollment.Attempts.Entries) != 1 {
			probeErr = errDockProtocol
			return
		}
		entry := s.HostEnrollment.Attempts.Entries[0]
		if entry.State != "unknown" || entry.RequestID == "" || entry.Key.Binding != binding || s.Notification.Reservation == nil || s.Notification.Reservation.Token != entry.Key.Token {
			probeErr = errDockProtocol
			return
		}
		probed = true
	}
	key, response, err := f.a.Notify(ctx, []byte("fixed Source fixture notice"), dockDeadline(ctx), time.Time{})
	f.clockHook = nil
	if err != nil || probeErr != nil || !probed {
		return errDockProtocol
	}
	if mode == "accepted-lookup" {
		if response.Result != "accepted" || response.Attempt == nil {
			return errDockProtocol
		}
		lookup, err := f.a.Reconcile(ctx, key, dockDeadline(ctx))
		if err != nil || lookup.Result != "existing-attempt" || lookup.Attempt == nil || lookup.Attempt.Sequence != response.Attempt.Sequence {
			return errDockProtocol
		}
		response = lookup
	} else if response.Result != "refused" || response.Reason != "input-not-ready" {
		return errDockProtocol
	}
	s, err := config.LoadState(f.paths, "member")
	if err != nil || s.Notification.Reservation != nil || s.NextBatch != "unread-retained" || s.HostEnrollment == nil || s.HostEnrollment.ReceiptRef != binding.EnrollmentRef || len(s.HostEnrollment.Attempts.Entries) != 1 {
		return errDockProtocol
	}
	entry := s.HostEnrollment.Attempts.Entries[0]
	if entry.Key != key || mode == "accepted-lookup" && entry.State != "accepted" || mode == "input-not-ready" && entry.State != "not-applied" {
		return errDockProtocol
	}
	_ = f.a.client.Close()
	finalCtx, endFinal := context.WithDeadline(ctx, dockDeadline(ctx))
	defer endFinal()
	frame, err = readDockControl(finalCtx, control)
	if err != nil {
		return err
	}
	result, err := decodeDockResult(frame, binding, mode)
	if err != nil || result.BindingActive || !result.InputFenced {
		return errDockProtocol
	}
	if mode == "accepted-lookup" {
		if result.IntentCount != 1 || result.WakeCount != 1 || result.NoticeID != "a2a-"+key.Token || result.Sequence != entry.Sequence || result.NativeStage != entry.NativeStage {
			return errDockProtocol
		}
	} else if result.IntentCount != 0 || result.WakeCount != 0 {
		return errDockProtocol
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	select {
	case err = <-finished:
	case <-finalCtx.Done():
		_ = cmd.Process.Kill()
		_ = <-finished
		waited = true
		return errDockProcess
	}
	waited = true
	if err != nil || stdout.bytes > dockFrameLimit || stderr.bytes > dockFrameLimit {
		return errDockProcess
	}
	t.Logf("case=%s helper_pid=%d helper_sha256=%s reaped=true durable_unknown_before_trigger=true intents=%d wakes=%d source_state=%s", mode, pid, digest, result.IntentCount, result.WakeCount, entry.State)
	return nil
}

// Proves an exact bounded artifact hash beside wrong hash, relative path and
// symlink rejection. It validates inert fixture bytes and never executes them.
func TestDockArtifactAdmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(path, []byte("abc"), 0700); err != nil {
		t.Fatal(err)
	}
	digest := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if err := checkDockArtifact(path, digest); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, "relative-artifact", path + "-missing"} {
		if err := checkDockArtifact(p, strings.Repeat("a", 64)); !errors.Is(err, errDockArtifact) {
			t.Fatal("unadmitted artifact accepted")
		}
	}
	link := path + "-link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if checkDockArtifact(link, digest) != errDockArtifact {
		t.Fatal("symlink admitted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if checkDockArtifact(path, digest) != errDockArtifact {
		t.Fatal("nonexecutable admitted")
	}
}

// Proves closed, correlated fixture grammar refuses malformed/unknown fields
// beside a valid ready/result. These are parser tests, not Host docking passes.
func TestDockControlAdmission(t *testing.T) {
	b := fixtureRequest("trigger").Binding
	ready := wireFrame(t, dockReady{Contract: dockContract, Kind: "ready", Binding: b})
	if err := decodeDockReady(ready, b); err != nil {
		t.Fatal(err)
	}
	wrong := b
	wrong.ConsumerGeneration = "other"
	if decodeDockReady(ready, wrong) != errDockProtocol {
		t.Fatal("changed ready binding accepted")
	}
	for _, bad := range [][]byte{
		append([]byte{0xef, 0xbb, 0xbf}, ready...), append(bytes.Repeat([]byte(" "), dockFrameLimit), ready...),
		bytes.Replace(ready, []byte(`"kind":"ready"`), []byte(`"kind":null`), 1),
		bytes.Replace(ready, []byte(`"kind":"ready"`), []byte(`"kind":"ready","future":false`), 1),
		bytes.Replace(ready, []byte(`"kind":"ready"`), []byte(`"kind":"ready","kind":"ready"`), 1),
		bytes.Replace(ready, []byte(`"kind":`), []byte(`"Kind":`), 1), ready[:len(ready)-1],
	} {
		if decodeDockReady(bad, b) != errDockProtocol {
			t.Fatal("invalid fixture ready accepted")
		}
	}
	r := dockResult{Contract: dockContract, Kind: "result", Case: "accepted-lookup", Binding: b, IntentCount: 1, NoticeID: "a2a-" + strings.Repeat("b", 32), Sequence: "1", NativeStage: "not-evidenced", WakeCount: 1, InputFenced: true}
	if _, err := decodeDockResult(wireFrame(t, r), b, r.Case); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*dockResult){func(x *dockResult) { x.IntentCount = -1 }, func(x *dockResult) { x.WakeCount = 33 }, func(x *dockResult) { x.Sequence = "01" }, func(x *dockResult) { x.Case = "lost-reply" }, func(x *dockResult) { x.NativeStage = "model-completed" }} {
		bad := r
		mutate(&bad)
		if _, err := decodeDockResult(wireFrame(t, bad), b, r.Case); err != errDockProtocol {
			t.Fatal("invalid fixture result accepted")
		}
	}
	r.Case = "input-not-ready"
	r.IntentCount = 0
	r.NoticeID = ""
	r.Sequence = ""
	r.NativeStage = ""
	r.WakeCount = 0
	if _, err := decodeDockResult(wireFrame(t, r), b, r.Case); err != nil {
		t.Fatal(err)
	}
}

// Proves a noisy helper cannot grow diagnostic retention beyond the declared
// byte bound; its full count still prevents a successful fixture result.
func TestDockOutputBound(t *testing.T) {
	w := &dockOutput{}
	data := bytes.Repeat([]byte{'x'}, dockFrameLimit+17)
	if n, err := w.Write(data); err != nil || n != len(data) || len(w.kept) != dockFrameLimit || w.bytes != len(data) {
		t.Fatal("fixture output bound lost")
	}
}
