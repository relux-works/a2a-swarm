package skill

import (
	"io/fs"
	"strings"
	"testing"
)

// The embedded install artifact must teach the principal refusal and repair
// contract alongside ordinary subscription controls. Reads are from embed.FS;
// this checks shipped guidance, not runtime enforcement or live delivery.
func TestEmbeddedPrincipalSubscriptionGuidance(t *testing.T) {
	data, err := fs.ReadFile(Files(), "SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{"a2a subs check", "a2a subs sync", "a2a subs trim", "host principals set/show/clear", "missing_principals", "principal_subscription_required", "no `unsubscribe --force`", "Owner-DM cleanup"} {
		if !strings.Contains(text, required) {
			t.Errorf("installed skill omits %q", required)
		}
	}
}
