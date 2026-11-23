// markedlane sorts the failures of the all-marked test lane.
//
// The lane is `BIND_FIXTURES_MARKED=1 go test -v ./internal/cli`: every
// fixture identity starts with a marked State and a bind-state sidecar, so
// the ordinary commands run on an identity that has bound before. Most
// failures of that lane are fixtures that write a binding's fields straight
// into State, which a marked State refuses by design. This tool reads the
// verbose test output and separates those from the failures a person has to
// read: it names every failing leaf test whose own output is not explained
// by that one refusal, and every test that was running when the binary timed
// out.
//
// Usage: markedlane [-strict] FILE...   ("-" reads standard input)
//
// It exits 0 after a report and 2 on an unreadable input. With -strict it
// exits 1 when the report names anything to review.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// A kind is one refusal of the bind-state sidecar, found by its exact text.
type kind struct {
	label string
	text  string
}

// kinds lists the refusals in the order the report prints them. The first is
// the fixture refusal; a leaf test that shows it and no other is not listed.
var kinds = []kind{
	{"provider-owned", config.ErrBindPartOwned.Error()},
	{"marker-missing", config.ErrBindPartUnmarked.Error()},
	{"sidecar-missing", config.ErrBindPartMissing.Error()},
	{"sidecar-invalid", config.ErrBindPartInvalid.Error()},
	{"stale-token", config.ErrBindPartStale.Error()},
	{"operation-held", config.ErrBindPartOperation.Error()},
}

const fixtureKind = "provider-owned"

type report struct {
	Passed, Failed int      // top-level tests
	Leaves         int      // failing tests without a failing subtest
	FixtureOnly    int      // leaves explained by the fixture refusal alone
	ByKind         []string // "label count", for every kind seen in a failing leaf
	Review         []string // "name: labels", sorted
	TimedOut       []string // tests running when the binary timed out
}

func (r report) needsReview() bool { return len(r.Review) != 0 || len(r.TimedOut) != 0 }

// sortLane reads one or more `go test -v` outputs as one stream of lines.
func sortLane(inputs ...io.Reader) (report, error) {
	seen := map[string]map[string]bool{} // test -> labels printed in its output
	failed := map[string]bool{}
	var rep report
	for _, in := range inputs {
		current, inTimeout := "", false
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
		for sc.Scan() {
			line := sc.Text()
			trimmed := strings.TrimLeft(line, " ")
			switch {
			case strings.HasPrefix(line, "=== RUN "), strings.HasPrefix(line, "=== CONT "), strings.HasPrefix(line, "=== NAME "), strings.HasPrefix(line, "=== PAUSE "):
				if f := strings.Fields(line); len(f) >= 3 {
					current = f[2]
				}
				continue
			case strings.HasPrefix(trimmed, "--- FAIL: "), strings.HasPrefix(trimmed, "--- PASS: "), strings.HasPrefix(trimmed, "--- SKIP: "):
				f := strings.Fields(trimmed)
				if len(f) < 3 {
					continue
				}
				top := trimmed == line
				switch f[1] {
				case "FAIL:":
					failed[f[2]] = true
					if top {
						rep.Failed++
					}
				case "PASS:":
					if top {
						rep.Passed++
					}
				}
				continue
			case strings.HasPrefix(line, "panic: test timed out"):
				inTimeout = true
				continue
			}
			if inTimeout {
				// "\trunning tests:" is followed by one "\t\tName (duration)" per test.
				if strings.HasPrefix(line, "\t\t") {
					if f := strings.Fields(line); len(f) >= 1 {
						rep.TimedOut = append(rep.TimedOut, f[0])
					}
					continue
				}
				if strings.TrimSpace(line) != "running tests:" {
					inTimeout = false
				}
			}
			if current == "" {
				continue
			}
			for _, k := range kinds {
				if strings.Contains(line, k.text) {
					if seen[current] == nil {
						seen[current] = map[string]bool{}
					}
					seen[current][k.label] = true
				}
			}
		}
		if err := sc.Err(); err != nil {
			return report{}, err
		}
	}
	counts := map[string]int{}
	for name := range failed {
		leaf := true
		for other := range failed {
			if strings.HasPrefix(other, name+"/") {
				leaf = false
				break
			}
		}
		if !leaf {
			continue
		}
		rep.Leaves++
		var labels []string
		for _, k := range kinds {
			if seen[name][k.label] {
				labels = append(labels, k.label)
				counts[k.label]++
			}
		}
		switch {
		case len(labels) == 1 && labels[0] == fixtureKind:
			rep.FixtureOnly++
		case len(labels) == 0:
			rep.Review = append(rep.Review, name+": no sidecar refusal in its output")
		default:
			rep.Review = append(rep.Review, name+": "+strings.Join(labels, ", "))
		}
	}
	for _, k := range kinds {
		if counts[k.label] != 0 {
			rep.ByKind = append(rep.ByKind, fmt.Sprintf("%s %d", k.label, counts[k.label]))
		}
	}
	sort.Strings(rep.Review)
	sort.Strings(rep.TimedOut)
	return rep, nil
}

func (r report) write(w io.Writer) {
	fmt.Fprintf(w, "top-level tests: %d passed, %d failed\n", r.Passed, r.Failed)
	fmt.Fprintf(w, "failing leaf tests: %d, of which %d show only the fixture refusal (%s)\n", r.Leaves, r.FixtureOnly, fixtureKind)
	if len(r.ByKind) != 0 {
		fmt.Fprintf(w, "refusals in failing leaf tests: %s\n", strings.Join(r.ByKind, "; "))
	}
	for _, name := range r.TimedOut {
		fmt.Fprintf(w, "TIMED OUT while running: %s\n", name)
	}
	for _, line := range r.Review {
		fmt.Fprintf(w, "REVIEW %s\n", line)
	}
	if !r.needsReview() {
		fmt.Fprintln(w, "nothing to review")
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("markedlane", flag.ContinueOnError)
	flags.SetOutput(stderr)
	strict := flags.Bool("strict", false, "exit 1 when the report names anything to review")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() == 0 {
		fmt.Fprintln(stderr, "usage: markedlane [-strict] FILE...   (\"-\" reads standard input)")
		return 2
	}
	var inputs []io.Reader
	for _, name := range flags.Args() {
		if name == "-" {
			inputs = append(inputs, stdin)
			continue
		}
		f, err := os.Open(name)
		if err != nil {
			fmt.Fprintln(stderr, "markedlane:", err)
			return 2
		}
		defer f.Close()
		inputs = append(inputs, f)
	}
	rep, err := sortLane(inputs...)
	if err != nil {
		fmt.Fprintln(stderr, "markedlane:", err)
		return 2
	}
	rep.write(stdout)
	if *strict && rep.needsReview() {
		return 1
	}
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
