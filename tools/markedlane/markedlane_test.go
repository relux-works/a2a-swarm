package main

import (
	"bytes"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// TestSortLane proves the sorting on one recorded-shape log: a failing parent
// is not a leaf; a leaf whose output holds only the fixture refusal is counted
// and not listed; a leaf with another refusal, with two refusals or with none
// is listed with what it showed; output is attributed to the test that
// printed it across PAUSE, CONT and NAME; and the test running at a timeout is
// named, while the goroutine dump after it adds nothing.
//
// It does not prove that a listed test is a product defect, or that an
// unlisted one is harmless beyond its fixture write: a test that stops at a
// fixture write shows nothing about the code after it.
func TestSortLane(t *testing.T) {
	f, err := os.Open("testdata/lane.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := sortLane(f)
	if err != nil {
		t.Fatal(err)
	}
	want := report{
		Passed: 1, Failed: 6, Leaves: 7, FixtureOnly: 3,
		ByKind: []string{"provider-owned 4", "sidecar-invalid 1", "stale-token 1", "operation-held 1"},
		Review: []string{
			"TestHeld: operation-held",
			"TestMixed: provider-owned, stale-token",
			"TestParallelB: sidecar-invalid",
			"TestUnexplained: no sidecar refusal in its output",
		},
		TimedOut: []string{"TestHangs"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("report\n got %+v\nwant %+v", got, want)
	}
}

// TestKindsFollowTheLibrary keeps the tool's texts tied to the errors the
// State library returns: a reworded refusal would otherwise be sorted as
// unexplained without anyone noticing.
func TestKindsFollowTheLibrary(t *testing.T) {
	texts := map[string]bool{}
	for _, k := range kinds {
		if k.text == "" || texts[k.text] {
			t.Fatalf("kind %q has an empty or repeated text", k.label)
		}
		texts[k.text] = true
	}
	if kinds[0].label != fixtureKind || kinds[0].text != config.ErrBindPartOwned.Error() {
		t.Fatalf("the fixture kind is %+v", kinds[0])
	}
	if !texts[config.ErrBindPartHeld.Error()] {
		t.Fatal("a held operation is not sorted as one")
	}
}

// TestRun covers the command: the report on standard output, exit 0 without
// -strict and 1 with it when something is listed, exit 0 with -strict when
// nothing is, and exit 2 for no input or an unreadable file.
func TestRun(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"testdata/lane.txt"}, nil, &out, &errb); code != 0 || !strings.Contains(out.String(), "REVIEW TestUnexplained: no sidecar refusal in its output") || !strings.Contains(out.String(), "TIMED OUT while running: TestHangs") {
		t.Fatalf("report run: code=%d out=%q err=%q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := run([]string{"-strict", "testdata/lane.txt"}, nil, &out, &errb); code != 1 {
		t.Fatalf("-strict with entries to review: code=%d", code)
	}
	out.Reset()
	clean := strings.NewReader("=== RUN   TestA\n    a_test.go:1: " + config.ErrBindPartOwned.Error() + "\n--- FAIL: TestA (0.00s)\n=== RUN   TestB\n--- PASS: TestB (0.00s)\n")
	if code := run([]string{"-strict", "-"}, clean, &out, &errb); code != 0 || !strings.Contains(out.String(), "nothing to review") || !strings.Contains(out.String(), "1 passed, 1 failed") {
		t.Fatalf("-strict with fixture failures only: code=%d out=%q", code, out.String())
	}
	if code := run(nil, nil, &out, &errb); code != 2 {
		t.Fatalf("no input: code=%d", code)
	}
	if code := run([]string{"testdata/absent.txt"}, nil, &out, &errb); code != 2 {
		t.Fatalf("unreadable input: code=%d", code)
	}
}
