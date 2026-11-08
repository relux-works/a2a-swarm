package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Proves namedMetadata refuses aliases before rewriting (no malformed value
// can be hidden), while each single-key neighbor survives a real file rewrite
// and named reload with the same binding and optional validation result.
func TestOpaqueWaitKeywordsNamedRoundTrip(t *testing.T) {
	for _, raw := range []string{`[ "A" ]`, `{ "future" : 1 }`} {
		for _, duplicate := range []bool{true, false} {
			f := newNamedFileFixture(t)
			p := f.paths("alpha", false)
			path := p.AgentFile("alice")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = append(bytes.TrimSpace(data[:bytes.LastIndexByte(data, '}')]), []byte(`,"WAIT_KEYWORDS":`+raw)...)
			if duplicate {
				data = append(data, []byte(`,"wait_keywords":["B"]`)...)
			}
			data = append(data, '}')
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			c, err := f.app().namedMetadata(p, "alice")
			if duplicate {
				if err == nil {
					t.Fatal("named metadata accepted aliases before rewrite")
				}
				if f.effects() != 0 {
					t.Fatal("duplicate refusal performed effects")
				}
				continue
			}
			if err != nil {
				t.Fatal("single-key named control refused")
			}
			if _, err := config.UpdateCredentials(p, "alice", func(c *config.Credentials) error { c.DeviceID = "next"; return nil }); err != nil {
				t.Fatal(err)
			}
			reloaded, err := f.app().namedMetadata(p, "alice")
			if err != nil || reloaded.DeviceID != "next" || (reloaded.ValidateWaitKeywords() == nil) != (c.ValidateWaitKeywords() == nil) {
				t.Fatal("rewrite bricked named metadata or changed predicate validity")
			}
		}
	}
}

// Proves malformed optional values cannot disable inbox/post/read, with a valid
// neighbor for each shape. Unrelated credential rewrites retain exact raw bytes.
func TestOpaqueWaitKeywordsUnrelatedCommands(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"object", `{ "future" : [9007199254740993, "<opaque>", {"nested":true}] }`},
		{"scalar", `"opaque\u0020value"`},
		{"array", `[ "TASK-56", { "future" : true } ]`},
		{"null", `null`},
		{"valid", `[ "TASK-56" ]`},
	} {
		for _, control := range []bool{false, true} {
			if control {
				tc.name += "-valid-control"
				tc.raw = `[ "TASK-56" ]`
			}
			t.Run(tc.name, func(t *testing.T) {
				e, _ := world(t)
				root := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "fixture", "synthetic topic").ok(t).stdout)
				path := e.paths().AgentFile("alice")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				data = append(bytes.TrimSuffix(data, []byte("}")), []byte(`,"wait_keywords":`+tc.raw+`}`)...)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				id := strings.TrimSpace(e.as("alice", "post", "proj", "--topic", root, "synthetic message").ok(t).stdout)
				if id == "" {
					t.Fatal("post returned no event")
				}
				if out := e.as("alice", "read", "proj", "--event", id).ok(t).stdout; !strings.Contains(out, "synthetic message") {
					t.Fatal("read did not retrieve the posted message")
				}
				e.as("alice", "inbox").ok(t)
				if _, err := config.UpdateCredentials(e.paths(), "alice", func(c *config.Credentials) error {
					c.DeviceID = "updated-device"
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				data, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(data, []byte(`"wait_keywords": `+tc.raw)) {
					t.Fatal("credential rewrite changed optional configuration bytes")
				}
				e.as("alice", "post", "proj", "--topic", root, "after rewrite").ok(t)
				e.as("alice", "read", "proj", "--event", id).ok(t)
				e.as("alice", "inbox").ok(t)
			})
		}
	}
}
