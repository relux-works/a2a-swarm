package association

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Context is derived from the already selected own identity/routing metadata,
// never from request assertions. Resolve must not load credentials or probe a
// runtime. The source does not authenticate descriptor origin or intent policy.
type Context struct{ Name, Connection, Base, Root, Selection string }

var hostSlug = regexp.MustCompile(`^[a-z0-9-]{1,24}$`)

func trimHost(b []byte) string {
	s := strings.TrimSpace(string(b))
	if !hostSlug.MatchString(s) {
		return ""
	}
	return s
}

type Options struct {
	Context     context.Context
	Input       *os.File
	Output      io.Writer
	Now         func() time.Time
	Resolve     func() (Context, error)
	InvalidArgs bool
}

// Run emits one closed result buffer, without stderr, state writes, native
// observations or network. Cooperative regular-file I/O is bounded by size and
// checked monotonic deadlines; kernel scheduling is not a hard real-time promise.
func Run(o Options) int {
	start := time.Now()
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Context == nil {
		o.Context = context.Background()
	}
	first := o.Now()
	ms := first.UnixMilli()
	var observation *int64
	if !first.IsZero() && ms >= 0 && ms <= maxTime {
		observation = &ms
	}
	out := refusal("input_untrusted", observation)
	var r request
	var own member
	admitted := false
	var leaseUntil time.Time
	finish := func() int {
		last := o.Now()
		lastMS := last.UnixMilli()
		if observation == nil || last.IsZero() || lastMS < ms || lastMS > maxTime {
			out = refusal("clock_unavailable", nil)
		} else if admitted && lastMS >= r.Expires {
			out = refusal("input_expired", observation)
		} else if time.Since(start) > 250*time.Millisecond {
			out = refusal("budget_exceeded", observation)
			if admitted {
				out.Digest = &r.Digest
			}
		} else if !leaseUntil.IsZero() && !leaseUntil.After(last) {
			out = refusal("stale_binding", observation)
			out.Digest = &r.Digest
		}
		if admitted && o.Context.Err() != nil && out.Reason != "clock_unavailable" && out.Reason != "input_expired" && out.Reason != "budget_exceeded" {
			out = refusal("snapshot_unavailable", observation)
			out.Digest = &r.Digest
		}
		if collides(out, r.Expected, own) {
			out = refusal("privacy_collision", nil)
		}
		b, code := encode(out)
		if b == nil || o.Output == nil {
			return 3
		}
		n, e := o.Output.Write(b)
		if e != nil || n != len(b) {
			return 3
		}
		return code
	}
	if observation == nil {
		if o.Input != nil {
			o.Input.Close()
		}
		return finish()
	}
	if o.InvalidArgs {
		if o.Input != nil {
			o.Input.Close()
		}
		out = refusal("input_invalid", observation)
		return finish()
	}
	b, reason := readIntent(o.Input)
	if reason != "" {
		out.Reason = reason
		return finish()
	}
	var e error
	r, e = decodeRequest(b)
	if e != nil {
		r.Digest = ""
		out.Reason = "input_invalid"
		return finish()
	}
	if o.Resolve == nil {
		out.Reason = "context_unavailable"
		return finish()
	}
	c, e := o.Resolve()
	if e != nil || c.Name == "" || c.Name == "." || filepath.Base(c.Name) != c.Name || c.Connection == "" || c.Name != r.Identity || c.Connection != r.Connection || !filepath.IsAbs(c.Base) || !filepath.IsAbs(c.Root) {
		out.Reason = "context_unavailable"
		return finish()
	}
	if r.Issued > ms {
		out.Reason = "input_invalid"
		return finish()
	}
	if ms >= r.Expires {
		out.Reason = "input_expired"
		return finish()
	}
	admitted = true
	out.Digest = &r.Digest
	s, e := snapshot(c, o.Context)
	if e != nil {
		out.Reason = "snapshot_unavailable"
		return finish()
	}
	fresh, e := o.Resolve()
	if e != nil || fresh != c {
		admitted = false
		out = refusal("context_unavailable", observation)
		return finish()
	}
	if s["terminal"] == nil {
		out.Reason = "binding_missing"
		return finish()
	}
	t, ok := s["terminal"].(map[string]any)
	if !ok {
		out.Reason = "binding_invalid"
		return finish()
	}
	own, ok = decodeMember(t, true)
	stopped, stopOK := t["stopped"].(bool)
	if !ok || !stopOK {
		out.Reason = "binding_invalid"
		return finish()
	}
	host, e := ReadMetadata(filepath.Join(c.Root, "host"), 128)
	// Match LoadHost's existing slug-file whitespace convention, without repair.
	local := trimHost(host)
	lease, _ := s["lease"].(map[string]any)
	holder, _ := lease["holder"].(string)
	expires, _ := lease["expires"].(string)
	until, e2 := time.Parse(time.RFC3339Nano, expires)
	if e != nil || local == "" || t["host"] != local || strings.HasPrefix(holder, "pid:") || holder != own.SessionID || e2 != nil || !until.After(first) {
		out.Reason = "stale_binding"
		return finish()
	}
	leaseUntil = until
	out.Stored = project(own, stopped)
	switch {
	case stopped:
		out.Reason = "binding_stopped"
	case own.Generation != r.Expected.Generation:
		out.Result = "mismatch"
		out.Reason = "generation_mismatch"
	case own != r.Expected:
		out.Result = "mismatch"
		out.Reason = "tuple_mismatch"
	default:
		out.Result = "match"
		out.Reason = "stored_tuple_equal"
	}
	return finish()
}
