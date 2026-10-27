package tmuxwake

import (
	"context"
	"testing"
)

// A deterministic typing-time registry transition alone strands the notice:
// the normalized composer start stays unchanged while the comparable key
// changes. Stable named 309/310 and Codex controls submit; this does not claim
// which transition occurred in the unrecorded live panes or qualify old images.
func TestBoundTypingTransitionStrandsNotice(t *testing.T) {
	for _, runtime := range []string{"claude-named", "codex"} {
		for _, version := range []string{"2.1.309", "2.1.310"} {
			for _, transition := range []string{"stable", "timestamp", "shell", "busy"} {
				if runtime == "codex" && (version != "2.1.310" || transition != "stable") {
					continue
				}
				t.Run(runtime+"/"+version+"/"+transition, func(t *testing.T) {
					f := newBoundFixture(t, runtime, version, "idle")
					sample := f.b.AssessReadiness(context.Background())
					if !sample.Eligible() {
						t.Fatal("empty control unavailable", sample)
					}
					result, err := f.b.Send(context.Background(), 1, sample.Key, nil, func() {
						f.f.wait(func(s fixtureState) bool { return s.Text == Notice(1) })
						if transition != "stable" {
							f.m["statusUpdatedAt"] = int64(1793016002001)
							if transition != "timestamp" {
								f.m["status"] = transition
							}
							f.write(t)
						}
						r, box := f.b.assess(context.Background(), Notice(1))
						if transition != "busy" && (r.Key.ComposerColumn != sample.Key.ComposerColumn || r.Key.ComposerRow != sample.Key.ComposerRow || box.Text != Notice(1)) {
							t.Fatal("geometry changed instead of registry", r, box)
						}
						if (r.Key == sample.Key) != (transition == "stable") {
							t.Fatal("unexpected key continuity", r)
						}
					})
					if err != nil {
						t.Fatal(err)
					}
					if transition == "stable" {
						if result.Disposition != SendSubmitted {
							t.Fatal("stable control stranded", result)
						}
						s := f.f.wait(func(s fixtureState) bool { return len(s.Submitted) == 1 })
						if s.Submitted[0] != Notice(1) {
							t.Fatal("wrong submission", s)
						}
					} else {
						s := f.f.wait(func(s fixtureState) bool { return s.Text == Notice(1) })
						if result.Disposition != SendDraft || len(s.Submitted) != 0 {
							t.Fatal("transition unexpectedly submitted", result, s)
						}
					}
				})
			}
		}
	}
}
