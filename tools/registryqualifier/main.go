// Command registryqualifier compares caller-selected registry functions in two
// bundle files as inert bytes. It has no connection to the production CLI.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"sync/atomic"
	"time"
)

func readBundle(path string) ([]byte, string) {
	// No discovery, stdin, symlink following, directory reads or raw OS errors.
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil, "input_not_regular_file"
	}
	if st.Size() <= 0 || st.Size() > maxBytes {
		return nil, "input_size"
	}
	f, err := openRegular(path)
	if err != nil {
		return nil, "input_unreadable"
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) || !opened.Mode().IsRegular() {
		return nil, "input_changed"
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, "input_unreadable"
	}
	if len(data) > maxBytes || int64(len(data)) != st.Size() {
		return nil, "input_size_or_changed"
	}
	return data, ""
}
func validAnchor(s string) bool {
	if len(s) == 0 || len(s) > 128 || !letter(s[0]) || keywords[s] {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !letter(s[i]) && !digit(s[i]) {
			return false
		}
	}
	return true
}
func run(args []string, w io.Writer) int { return runCommand(args, w, false) }
func runCommand(args []string, w io.Writer, watchdog bool) int {
	fs := flag.NewFlagSet("registryqualifier", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	base := fs.String("base", "", "qualified base bundle file")
	candidate := fs.String("candidate", "", "candidate bundle file")
	seconds := fs.Int("seconds", 60, "runtime budget, 1..90 seconds")
	var aa, ba anchors
	for i, role := range []string{"constructor", "reader", "update"} {
		fs.StringVar(&aa[i], "base-"+role, "", "exact lexical binding in the base")
		fs.StringVar(&ba[i], "candidate-"+role, "", "exact lexical binding in the candidate")
	}
	r := newReport(60)
	emit := func(exit int) int {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if enc.Encode(r) != nil {
			return 2
		}
		return exit
	}
	if fs.Parse(args) != nil || fs.NArg() != 0 || *base == "" || *candidate == "" || *seconds < 1 || *seconds > 90 {
		r.Reason = "invalid_arguments"
		fillFailedControls(&r, "invalid_arguments")
		return emit(2)
	}
	r = newReport(*seconds)
	for i := range aa {
		if !validAnchor(aa[i]) || !validAnchor(ba[i]) {
			r.Reason = "invalid_anchor"
			fillFailedControls(&r, "invalid_anchor")
			return emit(2)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*seconds)*time.Second)
	defer cancel()
	// File operations are on explicit regular local files. The process deadline
	// also covers filesystem stalls, for which Go context cancellation cannot
	// interrupt an arbitrary OS read. It emits a closed report before exiting.
	done := make(chan struct{})
	var finished atomic.Bool
	defer func() { finished.Store(true); close(done) }()
	if watchdog {
		go func() {
			select {
			case <-ctx.Done():
				if !finished.Swap(true) {
					timeout := newReport(*seconds)
					timeout.Reason = "deadline"
					fillFailedControls(&timeout, "deadline")
					json.NewEncoder(os.Stderr).Encode(timeout)
					os.Exit(2)
				}
			case <-done:
			}
		}()
	}
	a, reason := readBundle(*base)
	if reason != "" {
		r.Reason = "base_" + reason
		fillFailedControls(&r, "input_refused")
		return emit(2)
	}
	b, reason := readBundle(*candidate)
	if reason != "" {
		r.Reason = "candidate_" + reason
		fillFailedControls(&r, "input_refused")
		return emit(2)
	}
	r = evaluate(ctx, a, b, aa, ba, *seconds)
	if r.Status != equal {
		return emit(1)
	}
	return emit(0)
}
func main() { os.Exit(runCommand(os.Args[1:], os.Stdout, true)) }
