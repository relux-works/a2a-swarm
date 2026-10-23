package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/relux-works/a2a-swarm/internal/config"
	"regexp"
	"sort"
	"strings"
	"time"
)

type ownerDMJournalEntry struct {
	Key   string    `json:"key"`
	State string    `json:"state"`
	At    time.Time `json:"at"`
}

func (f ownerDMFiles) journal(now time.Time) ([]ownerDMJournalEntry, error) {
	b, st, err := ownerRead(f.path("journal"))
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, nil
	}
	var out []ownerDMJournalEntry
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		var e ownerDMJournalEntry
		if json.Unmarshal([]byte(line), &e) != nil || e.Key == "" || e.State == "" || e.At.IsZero() {
			return nil, fmt.Errorf("invalid owner-dm journal")
		}
		if !e.At.Before(now.Add(-7 * 24 * time.Hour)) {
			out = append(out, e)
		}
	}
	if len(out) > 256 {
		out = out[len(out)-256:]
	}
	return out, nil
}
func (f ownerDMFiles) saveJournal(ctx context.Context, entries []ownerDMJournalEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, _, err := ownerRead(f.path("journal")); err != nil {
		return err
	}
	if len(entries) > 256 {
		entries = entries[len(entries)-256:]
	}
	var b strings.Builder
	for _, entry := range entries {
		raw, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	return writeSettings(f.path("journal"), []byte(b.String()), 0600)
}

// Lock failure is best effort only: do not race another holder's atomic journal
// replacement. Attempt a line only if a fresh immediate lock attempt succeeds.
func (f ownerDMFiles) journalLockFailure(ctx context.Context, key string, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	brief, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()
	unlock, err := f.lock(brief)
	if err != nil {
		return
	}
	defer unlock()
	entries, err := f.journal(now)
	if err != nil {
		return
	}
	_ = f.saveJournal(brief, append(entries, ownerDMJournalEntry{Key: key, State: "failed:lock", At: now}))
}

var ownerSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----.*?-----END (?:[A-Z0-9]+ )*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)Authorization\s*:\s*Bearer\s+[^\s"']+`),
	regexp.MustCompile(`\b(?:syt_|mat_|ghp_|gho_|ghs_|ghu_|github_pat_|xox[abpr]-|AIza|sk-)[A-Za-z0-9_=-]+`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`),
	regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s/:@]+:[^\s/@]+@[^\s/]+`),
	regexp.MustCompile(`(?i)(?:^|[\s{,;])(?:"|')?(?:password|passwd|token|secret|api_key|access_token|refresh_token|client_secret)(?:"|')?\s*[:=]\s*(?:"(?:[^"\\]|\\.)*"|'[^']*'|[^\s,;}]+)`),
}

// Match against the whole selected message before chunking. Merge overlapping
// spans so the threshold counts original redactions, not replacements again.
func ownerDMContent(text string) []string {
	var spans [][2]int
	for _, re := range ownerSecretPatterns {
		for _, m := range re.FindAllStringIndex(text, -1) {
			spans = append(spans, [2]int{m[0], m[1]})
		}
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i][0] != spans[j][0] {
			return spans[i][0] < spans[j][0]
		}
		return spans[i][1] > spans[j][1]
	})
	merged := spans[:0]
	for _, span := range spans {
		if len(merged) > 0 && span[0] < merged[len(merged)-1][1] {
			merged[len(merged)-1][1] = max(merged[len(merged)-1][1], span[1])
		} else {
			merged = append(merged, span)
		}
	}
	if len(merged) > 20 {
		return []string{"final message withheld: too many secret-like strings"}
	}
	var b strings.Builder
	pos := 0
	for _, span := range merged {
		b.WriteString(text[pos:span[0]])
		b.WriteString("[redacted]")
		pos = span[1]
	}
	b.WriteString(text[pos:])
	chars := []rune(b.String())
	if len(chars) > 32000 {
		omitted := len(chars) - 28000
		chars = append(chars[:28000:28000], []rune(fmt.Sprintf("\n[omitted %d characters]\n", omitted))...)
	}
	var chunks []string
	for len(chars) > 0 {
		n := min(4000, len(chars))
		chunks = append(chunks, string(chars[:n]))
		chars = chars[n:]
	}
	return chunks
}
func ownerSelectedPayload(runtime string, payload json.RawMessage) (session, turn, text string, ok bool) {
	var values map[string]json.RawMessage
	if json.Unmarshal(payload, &values) != nil {
		return
	}
	fields := []string{"session_id", "last_assistant_message", "turn_id"}
	if runtime == "claude" {
		fields[2] = "prompt_id"
	}
	selected := make([]string, 3)
	for i, field := range fields {
		if json.Unmarshal(values[field], &selected[i]) != nil || selected[i] == "" {
			return
		}
	}
	return selected[0], selected[2], selected[1], true
}
func (a *App) ownerMirrorStop(parent context.Context, runtime string, in hookInput) {
	budget := 5 * time.Second
	if a.ownerMirrorBudget > 0 {
		budget = a.ownerMirrorBudget
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	name, err := a.identity()
	if err != nil {
		return
	}
	paths, err := a.paths()
	if err != nil {
		return
	}
	f := ownerFiles(paths, name)
	c, _, err := f.load()
	if err != nil || c == nil || (!c.Active && !c.Pending) || !c.Mirror || f.pin().Owner != c.Owner {
		return
	}
	prior := a.ctx
	a.ctx = ctx
	defer func() { a.ctx = prior }()
	session, turn, text, valid := ownerSelectedPayload(runtime, in.Payload)
	sum := sha256.Sum256([]byte(text))
	keyFor := func(generation uint64) string {
		return fmt.Sprintf("%s|%q|%q|%x|%d", runtime, session, turn, sum, generation)
	}
	unlock, err := f.lock(ctx)
	if err != nil {
		f.journalLockFailure(ctx, keyFor(c.Generation), a.Now())
		return
	}
	defer unlock()
	c, st, err := f.load()
	if err != nil || c == nil || (!c.Active && !c.Pending) || !c.Mirror || f.pin().Owner != c.Owner {
		return
	}
	s, err := a.session()
	if err != nil {
		return
	}
	s.client = s.client.WithoutRedirects()
	// Paused hooks retain their existing kill-switch semantics.
	state, err := config.LoadState(s.paths, s.name)
	if err != nil || state.HooksPaused {
		return
	}
	key := keyFor(c.Generation)
	entries, err := f.journal(a.Now())
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Key == key {
			return
		}
	}
	record := ownerDMJournalEntry{Key: key, At: a.Now(), State: "pending"}
	transition, err := s.ownerTransition(ctx, c, st)
	if err != nil {
		record.State = "refused:activation"
		_ = f.saveJournal(ctx, append(entries, record))
		return
	}
	if err = s.ownerSaveTransition(ctx, f, c, transition); err != nil {
		record.State = "failed:activation"
		if latest, readErr := f.journal(a.Now()); readErr == nil {
			_ = f.saveJournal(ctx, append(latest, record))
		}
		return
	}
	// Activation confirmation owns a journal entry too; preserve it when reserving the mirror.
	entries, err = f.journal(a.Now())
	if err != nil {
		return
	}
	if !valid {
		record.State = "skipped:no-payload"
		_ = f.saveJournal(ctx, append(entries, record))
		return
	}
	if !c.Active {
		return
	}
	entries = append(entries, record)
	// A durable pending record must exist before the first mirrored chunk.
	if err = f.saveJournal(ctx, entries); err != nil {
		return
	}
	finish := func(status string) { entries[len(entries)-1].State = status; _ = f.saveJournal(ctx, entries) }
	if _, err = s.ownerAudience(ctx, c.Room, c.Owner, false); err != nil {
		finish("refused:audience")
		return
	}
	chunks := ownerDMContent(text)
	sent := 0
	digest := sha256.Sum256([]byte(key))
	prefix := "a2a-odm-" + hex.EncodeToString(digest[:])[:24]
	for i, chunk := range chunks {
		if err = ctx.Err(); err == nil {
			_, err = s.client.SendMessageTxn(ctx, c.Room, fmt.Sprintf("%s-%d", prefix, i), map[string]any{"msgtype": "m.text", "body": chunk})
		}
		if err != nil {
			if sent == 0 {
				finish("failed:send")
			} else {
				finish(fmt.Sprintf("partial %d/%d:send", sent, len(chunks)))
			}
			return
		}
		sent++
	}
	finish(fmt.Sprintf("sent %d/%d", sent, len(chunks)))
}
