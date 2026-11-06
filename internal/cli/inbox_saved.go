package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/redact"
)

// The file is a complete untrusted frame of indented, single-line JSON records.
// Unlike the legacy stdout frame, saved bodies are not truncated. Both saving
// and reading refuse a frame over this cap; they never receipt a truncated save.
const inboxSavedMax = 16 << 20

func savedInboxFrame(a *App, items []waitItem, server string) ([]EventRecord, []byte, error) {
	records := make([]EventRecord, 0, len(items))
	var b strings.Builder
	b.WriteString(inboxBegin + "\n")
	for _, it := range items {
		rec, _ := inboxRecord(it, server)
		if it.ev != nil {
			rec.Body = eventRecord(it.rec.Type, it.rec.Room, it.rec.RoomID, *it.ev, server, false).Body
		} else if it.gap == nil {
			rec.Body = it.line
		}
		if rec.EventID == "" {
			rec.EventID = it.id
		}
		data, err := json.Marshal(rec)
		if err != nil {
			return nil, nil, err
		}
		// Scrub decoded strings, so escaped quotes/newlines in credentials cannot
		// evade redaction or corrupt JSON. Attachment descriptors are data too.
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			return nil, nil, err
		}
		data, err = json.Marshal(scrubInboxValue(value, a.secrets.Scrub))
		if err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal(data, &rec); err != nil {
			return nil, nil, err
		}
		records = append(records, rec)
		b.WriteString(inboxIndent)
		b.Write(data)
		b.WriteByte('\n')
		if b.Len()+len(inboxEnd)+1 > inboxSavedMax {
			return nil, nil, fmt.Errorf("full inbox frame exceeds %d bytes; use a smaller --limit", inboxSavedMax)
		}
	}
	b.WriteString(inboxEnd + "\n")
	return records, []byte(b.String()), nil
}

func scrubInboxValue(v any, scrub func(string) string) any {
	switch x := v.(type) {
	case string:
		return scrub(x)
	case []any:
		for i := range x {
			x[i] = scrubInboxValue(x[i], scrub)
		}
	case map[string]any:
		clean := make(map[string]any, len(x))
		for k, value := range x {
			clean[scrub(k)] = scrubInboxValue(value, scrub)
		}
		return clean
	}
	return v
}

func summaryField(value string) string {
	if value = sanitizeLine(value); value == "" {
		return "-"
	}
	return value
}

func inboxSummary(records []EventRecord, width, more int) (string, error) {
	// Refuse rather than combine distinct identities under a truncated key.
	// This bounds every field without silently changing grouping or event ids.
	for _, r := range records {
		for _, value := range []string{r.Sender, r.Kind, firstNonEmpty(r.Room, r.RoomID), r.Topic, r.EventID} {
			if utf8.RuneCountInString(summaryField(value)) > 512 {
				return "", fmt.Errorf("summary field exceeds 512 characters; save with --out without --summary")
			}
		}
	}
	var b strings.Builder
	b.WriteString(inboxBegin + "\n")
	for _, group := range []struct {
		name string
		key  func(EventRecord) string
	}{
		{"sender", func(r EventRecord) string { return r.Sender }},
		{"kind", func(r EventRecord) string { return r.Kind }},
		{"room", func(r EventRecord) string { return firstNonEmpty(r.Room, r.RoomID) }},
	} {
		counts := map[string]int{}
		for _, r := range records {
			counts[summaryField(group.key(r))]++
		}
		for _, key := range sortedKeys(counts) {
			fmt.Fprintf(&b, "count %s=%q n=%d\n", group.name, key, counts[key])
		}
	}
	for i, r := range records {
		excerpt, _ := truncateRunes(sanitizeLine(r.Body), width)
		fmt.Fprintf(&b, "event %d sender=%q kind=%q room=%q topic=%q id=%q %s\n",
			i+1, summaryField(r.Sender), summaryField(r.Kind), summaryField(firstNonEmpty(r.Room, r.RoomID)), summaryField(r.Topic), summaryField(r.EventID), excerpt)
	}
	b.WriteString(inboxEnd + "\n")
	if more > 0 {
		fmt.Fprintf(&b, "(%d more pending: run a2a inbox again)\n", more)
	}
	return b.String(), nil
}

func cmdInboxShow(a *App, args []string) error {
	fs := newFlagSet("inbox show")
	from := fs.String("from", "", "read a saved inbox frame instead of the last saved frame")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || pos[0] == "" || len(pos[0]) > 4096 || sanitizeLine(pos[0]) != pos[0] {
		return usagef("usage: a2a inbox show <$event> [--from FILE]")
	}
	if !flagPresent(fs, "from") {
		paths, err := a.paths()
		if err != nil {
			return err
		}
		name, err := a.identity()
		if err != nil {
			return err
		}
		st, err := config.LoadState(paths, name)
		if err != nil {
			return err
		}
		*from = st.LastInbox
		if *from == "" {
			return fmt.Errorf("no saved inbox frame; use inbox --out FILE first")
		}
	}
	records, err := readSavedInbox(*from)
	if err != nil {
		return err
	}
	for _, r := range records {
		if r.EventID != pos[0] {
			continue
		}
		// A saved file is untrusted input. Re-sanitize and frame every body line;
		// reading a frame never loads mail, syncs or records a receipt.
		var b strings.Builder
		b.WriteString(inboxBegin + "\n")
		fmt.Fprintf(&b, "--- event 1/1: from=%q room=%q kind=%q id=%q\n", inboxField(r.Sender), inboxField(r.Room), inboxField(r.Kind), inboxField(r.EventID))
		body := strings.NewReplacer("\u2028", "\n", "\u2029", "\n").Replace(sanitizeText(redact.String(r.Body)))
		for _, line := range strings.Split(body, "\n") {
			b.WriteString(inboxIndent + line + "\n")
		}
		b.WriteString(inboxEnd + "\n")
		return writeCLIOutput(a.Stdout, a.secrets.Scrub(b.String()))
	}
	return fmt.Errorf("event not found in saved inbox frame")
}

func readSavedInbox(path string) ([]EventRecord, error) {
	root, name, err := openOutputParent(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	st, err := root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("read saved inbox: %w", err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 || st.Size() > inboxSavedMax {
		return nil, fmt.Errorf("saved inbox must be a regular 0600 file at most %d bytes", inboxSavedMax)
	}
	f, err := openSavedInboxFile(root, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) || opened.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("saved inbox changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, inboxSavedMax+1))
	if err != nil || len(data) > inboxSavedMax {
		return nil, fmt.Errorf("cannot read complete bounded saved inbox")
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 3 || lines[0] != inboxBegin || lines[len(lines)-2] != inboxEnd || lines[len(lines)-1] != "" || len(lines)-3 > inboxMax {
		return nil, fmt.Errorf("invalid saved inbox frame")
	}
	records := make([]EventRecord, 0, len(lines)-3)
	seen := map[string]bool{}
	for _, line := range lines[1 : len(lines)-2] {
		var r EventRecord
		if !strings.HasPrefix(line, inboxIndent) || json.Unmarshal([]byte(strings.TrimPrefix(line, inboxIndent)), &r) != nil || r.EventID == "" || seen[r.EventID] {
			return nil, fmt.Errorf("invalid saved inbox event")
		}
		seen[r.EventID] = true
		records = append(records, r)
	}
	return records, nil
}
