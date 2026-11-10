package tmuxwake

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const fixtureMachineID = "0123456789abcdef0123456789abcdef"
const fixtureLinuxDomain = "linux:" + fixtureMachineID + ":pid:[123456]"

func linuxIdentityFixture() (map[string]any, registryIdentity) {
	m := registryFixture("3.0.0", "idle")
	m["pidDomain"] = fixtureLinuxDomain
	m["procStart"] = "987654"
	id := registryIdentity{PID: 1234, Domain: fixtureLinuxDomain, StartTicks: "987654", SessionID: "11111111-1111-4111-8111-111111111111", Pane: "%1"}
	return m, id
}

// Every refusal starts from an otherwise-valid Linux record and independent
// expected identity. A wrong machine, namespace, reused PID or platform cannot
// qualify by agreeing only with itself; PID/session/pane and malformed neighbors
// also require the intended closed reason, not an incidental parser failure.
func TestLinuxRegistryIdentityFences(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any, *registryIdentity)
		want RuntimeReason
	}{
		{"wrong-machine-id", func(m map[string]any, _ *registryIdentity) {
			m["pidDomain"] = "linux:" + strings.Repeat("a", 32) + ":pid:[123456]"
		}, RuntimeIdentityMismatch},
		{"wrong-pid-namespace", func(m map[string]any, _ *registryIdentity) {
			m["pidDomain"] = "linux:" + fixtureMachineID + ":pid:[654321]"
		}, RuntimeIdentityMismatch},
		{"reused-pid-start-ticks", func(m map[string]any, _ *registryIdentity) { m["procStart"] = "987655" }, RuntimeIdentityMismatch},
		{"mixed-darwin-linux-domain", func(m map[string]any, _ *registryIdentity) { m["pidDomain"] = "darwin" }, RuntimeIdentityMismatch},
		{"darwin-birth-with-linux-domain", func(m map[string]any, _ *registryIdentity) { m["procStart"] = "Mon Oct 26 12:00:00 2026" }, RuntimeIdentityMismatch},
		{"wrong-pid", func(m map[string]any, _ *registryIdentity) { m["pid"] = 1235 }, RuntimeIdentityMismatch},
		{"wrong-session", func(m map[string]any, _ *registryIdentity) { m["sessionId"] = "different" }, RuntimeIdentityMismatch},
		{"wrong-pane", func(m map[string]any, _ *registryIdentity) { m["tmux"] = "synthetic:@0.%2" }, RuntimeIdentityMismatch},
		{"empty-start", func(m map[string]any, _ *registryIdentity) { m["procStart"] = "" }, RuntimeIdentityMismatch},
		{"signed-start", func(m map[string]any, _ *registryIdentity) { m["procStart"] = "+987654" }, RuntimeIdentityMismatch},
		{"leading-zero-start", func(m map[string]any, _ *registryIdentity) { m["procStart"] = "0987654" }, RuntimeIdentityMismatch},
		{"missing-start", func(m map[string]any, _ *registryIdentity) { delete(m, "procStart") }, RuntimeSchemaUnqualified},
		{"missing-domain", func(m map[string]any, _ *registryIdentity) { delete(m, "pidDomain") }, RuntimeSchemaUnqualified},
		{"missing-local-domain", func(_ map[string]any, id *registryIdentity) { id.Domain = "" }, RuntimeIdentityMismatch},
		{"missing-local-start", func(_ map[string]any, id *registryIdentity) { id.StartTicks = "" }, RuntimeIdentityMismatch},
		{"malformed-local-domain", func(m map[string]any, id *registryIdentity) {
			id.Domain = "linux:unknown:pid:[123456]"
			m["pidDomain"] = id.Domain
		}, RuntimeIdentityMismatch},
		{"malformed-local-start", func(m map[string]any, id *registryIdentity) { id.StartTicks = "-1"; m["procStart"] = id.StartTicks }, RuntimeIdentityMismatch},
		{"unsupported-local-platform", func(m map[string]any, id *registryIdentity) { id.Domain = "unsupported"; m["pidDomain"] = id.Domain }, RuntimeIdentityMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, id := linuxIdentityFixture()
			check := func(want RuntimeReason) {
				t.Helper()
				raw, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				before := bytes.Clone(raw)
				got := decodeRegistry(raw, id)
				if got.Reason != want || got.State != RuntimeStateFor(want) || !bytes.Equal(before, raw) {
					t.Fatalf("identity reason=%s state=%s want=%s; input unchanged=%t", got.Reason, got.State, want, bytes.Equal(before, raw))
				}
			}
			check(RuntimeNone)
			tc.edit(m, &id)
			check(tc.want)
		})
	}
}

// Darwin stays an explicit UTC ps-birth/domain contract on every runner OS;
// adding Linux support must not reinterpret dates as ticks or accept mixing.
// Complete records take the production JSON path, including numeric field types.
func TestDarwinRegistryIdentityFences(t *testing.T) {
	for _, mode := range []string{"wrong-birth", "mixed-linux-domain", "ticks-as-birth", "missing-local-birth"} {
		t.Run(mode, func(t *testing.T) {
			m := registryFixture("3.0.0", "idle")
			id := registryIdentity{PID: 1234, Domain: "darwin", Birth: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC), SessionID: "11111111-1111-4111-8111-111111111111", Pane: "%1"}
			check := func(wantReason RuntimeReason, wantState RuntimeState) {
				t.Helper()
				raw, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				got := decodeRegistry(raw, id)
				if got.Reason != wantReason || got.State != wantState {
					t.Fatalf("Darwin reason=%s state=%s want=%s/%s", got.Reason, got.State, wantReason, wantState)
				}
			}
			check(RuntimeNone, RuntimeReady)
			switch mode {
			case "wrong-birth":
				m["procStart"] = "Sun Oct 25 12:00:00 2026"
			case "mixed-linux-domain":
				m["pidDomain"] = fixtureLinuxDomain
			case "ticks-as-birth":
				m["procStart"] = "987654"
			case "missing-local-birth":
				id.Birth = time.Time{}
			}
			check(RuntimeIdentityMismatch, RuntimeRefused)
		})
	}
}

func procStatFixture(pid int, comm, ticks string) []byte {
	fields := make([]string, 50) // state (3) through exit_code (52).
	for i := range fields {
		fields[i] = "0"
	}
	fields[0], fields[1] = "S", "1"
	fields[18], fields[19], fields[20] = "314", ticks, "2718"
	return []byte(strconv.Itoa(pid) + " (" + comm + ") " + strings.Join(fields, " ") + "\n")
}

// The documented field number is independent of the parser's offset: distinct
// neighbors catch either off-by-one. Parentheses/newlines in comm are valid;
// truncated, mismatched-PID, malformed or oversized input cannot supply ticks.
func TestRegistryProcStatStart(t *testing.T) {
	valid := procStatFixture(1234, "name ) 99 (\nspace", "987654")
	if got, err := procStatStart(valid, 1234); err != nil || got != "987654" {
		t.Fatal("comm delimiter/field-22 control", err)
	}
	for _, raw := range [][]byte{
		nil,
		procStatFixture(1235, "name", "987654"),
		[]byte("1234 (unfinished"),
		bytes.Replace(valid, []byte(") S "), []byte(") ? "), 1),
		bytes.Replace(valid, []byte(") S "), []byte(")S "), 1),
		[]byte("1234 (name) S 1 2 3"),
		[]byte("1234 (name) " + strings.Join(strings.Fields(string(procStatFixture(1234, "name", "987654")))[2:22], " ")),
		procStatFixture(1234, "name", "-1"),
		procStatFixture(1234, "name", "+987654"),
		procStatFixture(1234, "name", "0987654"),
		procStatFixture(1234, "name", "18446744073709551616"),
		append(bytes.Clone(valid), []byte("garbage")...),
		bytes.Repeat([]byte{'x'}, maxProcStatBytes+1),
	} {
		if got, err := procStatStart(raw, 1234); err == nil || got != "" || err.Error() != "registry identity unavailable" {
			t.Fatal("malformed stat supplied identity")
		}
	}
}

// File collection is bounded and refuses missing, malformed, symlink/FIFO and
// nonregular inputs. Each negative first collects the complete valid isolated
// platform fixture; no real process, machine identifier or registry is read.
// A private read seam supplies permission errors or changes start ticks on the
// second stat read, proving refusal without racing a real process replacement.
func TestLinuxRegistryProcessFiles(t *testing.T) {
	for _, mode := range []string{"missing-stat", "truncated-stat", "oversized-stat", "symlink-stat", "fifo-stat", "missing-machine", "malformed-machine", "oversized-machine", "directory-machine", "missing-namespace", "malformed-namespace", "oversized-namespace", "unreadable-stat", "unreadable-machine", "replaced-during-read", "gone-after-namespace"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			proc := filepath.Join(root, "proc")
			dir := filepath.Join(proc, "1234")
			ns := filepath.Join(dir, "ns", "pid")
			stat := filepath.Join(dir, "stat")
			machine := filepath.Join(root, "machine-id")
			if err := os.MkdirAll(filepath.Dir(ns), 0700); err != nil {
				t.Fatal(err)
			}
			write := func(path string, data []byte) {
				t.Helper()
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			remove := func(path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			link := func(target, path string) {
				t.Helper()
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			write(stat, procStatFixture(1234, "name ) (", "987654"))
			write(machine, []byte(fixtureMachineID+"\n"))
			link("pid:[123456]", ns)
			if domain, ticks, err := linuxRegistryProcess(proc, machine, 1234); err != nil || domain != fixtureLinuxDomain || ticks != "987654" {
				t.Fatal("valid local identity fixture refused", err)
			}
			switch mode {
			case "missing-stat":
				remove(stat)
			case "truncated-stat":
				write(stat, []byte("1234 (name) S 1"))
			case "oversized-stat":
				write(stat, bytes.Repeat([]byte{'x'}, maxProcStatBytes+1))
			case "symlink-stat":
				remove(stat)
				link(machine, stat)
			case "fifo-stat":
				remove(stat)
				if err := syscall.Mkfifo(stat, 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-machine":
				remove(machine)
			case "malformed-machine":
				write(machine, []byte("not-a-machine-id"))
			case "oversized-machine":
				write(machine, bytes.Repeat([]byte{'a'}, 34))
			case "directory-machine":
				remove(machine)
				if err := os.Mkdir(machine, 0700); err != nil {
					t.Fatal(err)
				}
			case "missing-namespace":
				remove(ns)
			case "malformed-namespace":
				remove(ns)
				link("net:[123456]", ns)
			case "oversized-namespace":
				remove(ns)
				link("pid:["+strings.Repeat("1", 70)+"]", ns)
			}
			reads := 0
			readFile := func(path string, limit int64) ([]byte, error) {
				if path == stat {
					reads++
					if mode == "unreadable-stat" {
						return nil, os.ErrPermission
					}
					if mode == "gone-after-namespace" && reads == 2 {
						return nil, os.ErrNotExist
					}
					if mode == "replaced-during-read" && reads == 2 {
						return procStatFixture(1234, "replacement", "987655"), nil
					}
				}
				if path == machine && mode == "unreadable-machine" {
					return nil, os.ErrPermission
				}
				return registryIdentityFile(path, limit)
			}
			if domain, ticks, err := linuxRegistryProcessUsing(proc, machine, 1234, readFile); err == nil || domain != "" || ticks != "" || err.Error() != "registry identity unavailable" {
				t.Fatal("unavailable/malformed local identity admitted")
			}
		})
	}
}
