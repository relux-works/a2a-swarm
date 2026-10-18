package tmuxwake

import "testing"

// Synthetic screens prove that recognized empty inputs admit a notice while
// drafts, modals, busy screens, missing boxes and uncertain layouts fail closed.
func TestScreenAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, runtime, screen, text string
		ok                          bool
	}{
		{"codex placeholder", "codex", "\n› \x1b[2mAsk anything\x1b[0m\n\n  ? for shortcuts\n", "", true},
		{"codex draft", "codex", "\n› human draft\n\n  ? for shortcuts\n", "human draft", true},
		{"codex busy", "codex", "Working (esc to interrupt)\n› \n\n  ? for shortcuts\n", "", false},
		{"claude placeholder", "claude", "──────────\n❯ \x1b[90mTry this\x1b[39m\n──────────\n", "", true},
		{"claude draft", "claude", "──────────\n❯ keep  two spaces\n──────────\n", "keep  two spaces", true},
		{"permission", "claude", "Do you want to allow?\n──────────\n❯ \n──────────\n", "", false},
		{"plan", "codex", "Would you like to implement this plan?\n› \n\n  ? for shortcuts\n", "", false},
		{"missing box", "claude", "❯ \n", "", false},
		{"unknown footer", "codex", "› \nprivate modal\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Input(tc.screen, tc.runtime)
			if ok != tc.ok || ok && got.Text != tc.text {
				t.Fatalf("got %#v/%v; want %q/%v", got, ok, tc.text, tc.ok)
			}
		})
	}
}

// Whitespace is significant: normalizing a draft into the notice would permit
// an unintended Enter. Extended color parsing must not hide subsequent text.
func TestStyledText(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"\x1b[2mplaceholder\x1b[22mreal", "real"},
		{"\x1b[38;5;245msuggestion\x1b[38;5;2mreal", "real"},
		{"\x1b[38;2;128;128;128msuggestion\x1b[39mreal", "real"},
		{"a  b", "a  b"},
	} {
		if got := visible(tc.in); got != tc.want {
			t.Fatalf("got %q want %q", got, tc.want)
		}
	}
}
