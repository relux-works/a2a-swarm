package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// ContentKey is the custom content key that carries structured a2a headers.
const ContentKey = "ai.relux.a2a"

// TopicPrefix starts every topic root body.
const TopicPrefix = "[topic] "

// Kinds lists the message kinds of protocol rule 3.
var Kinds = []string{"notice", "question", "answer", "proposal", "decision", "ack", "offer", "request", "accept", "decline", "handover", "subscribe", "unsubscribe", "owner-needed", "system"}

// DefaultReplyBy is the protocol default deadline for questions (rule 6).
const DefaultReplyBy = 12 * time.Hour

var (
	slugRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	trunkRE = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
)

// Header is the structured a2a header stored under ContentKey.
type Header struct {
	V       int      `json:"v"`
	Type    string   `json:"type"` // "topic" or "message"
	Slug    string   `json:"slug,omitempty"`
	Title   string   `json:"title,omitempty"`
	Topic   string   `json:"topic,omitempty"` // thread root event id
	From    string   `json:"from"`
	To      []string `json:"to,omitempty"`
	At      string   `json:"at"`
	Re      string   `json:"re,omitempty"`
	Kind    string   `json:"kind,omitempty"`
	ReplyBy string   `json:"reply_by,omitempty"`
	Trunk   string   `json:"trunk,omitempty"`
	// Attachment is set on file messages sent by `a2a attach`.
	Attachment *Attachment `json:"attachment,omitempty"`
}

// Attachment describes a file shared through the media repository.
type Attachment struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	MXC      string `json:"mxc"`
	MimeType string `json:"mimetype"`
}

func decodeHeader(raw json.RawMessage) (Header, bool) {
	var h Header
	if len(raw) == 0 || json.Unmarshal(raw, &h) != nil {
		return h, false
	}
	return h, true
}

// parseTopicBody extracts slug and title from "[topic] <slug>: <title>".
func parseTopicBody(body string) (slug, title string, ok bool) {
	if !strings.HasPrefix(body, TopicPrefix) {
		return "", "", false
	}
	first, _, _ := strings.Cut(body[len(TopicPrefix):], "\n")
	slug, title, ok = strings.Cut(first, ":")
	slug = strings.TrimSpace(slug)
	if !ok || !slugRE.MatchString(slug) {
		return "", "", false
	}
	return slug, strings.TrimSpace(title), true
}

// topicInfo is one thread root in a room.
type topicInfo struct {
	Slug    string
	Title   string
	EventID string
	Sender  string
	TS      int64
	Replies int
}

// maxScanEvents bounds the /messages scan used to find topic roots.
const maxScanEvents = 2000

// listTopics returns the thread roots of a room, oldest first. It uses the v1
// /threads API and adds topic roots that have no replies yet (which /threads
// does not return) from a bounded /messages scan. If /threads is unsupported,
// reply counts come from the scan.
func (s *session) listTopics(ctx context.Context, roomID string) ([]topicInfo, error) {
	byID := map[string]*topicInfo{}
	add := func(ev matrix.Event, replies int) {
		if _, ok := byID[ev.EventID]; ok {
			return
		}
		m := ev.Message()
		slug, title, _ := parseTopicBody(m.Body)
		if slug == "" {
			if h, ok := decodeHeader(m.A2A); ok && h.Type == "topic" {
				slug, title = h.Slug, h.Title
			}
		}
		if title == "" {
			title = snippet(m.Body, 60)
		}
		byID[ev.EventID] = &topicInfo{Slug: slug, Title: title, EventID: ev.EventID, Sender: ev.Sender, TS: ev.OriginServerTS, Replies: replies}
	}

	threadsOK := true
	from := ""
	for page := 0; page < 20; page++ {
		p, err := s.client.Threads(ctx, roomID, from, 100)
		if err != nil {
			if isNotFound(err) || matrix.IsCode(err, "M_UNRECOGNIZED") || matrix.IsStatus(err, 405) {
				threadsOK = false
				break
			}
			return nil, err
		}
		for _, ev := range p.Chunk {
			n := ev.ThreadCount()
			if n < 0 {
				n = 0
			}
			add(ev, n)
		}
		if p.NextBatch == "" || len(p.Chunk) == 0 {
			break
		}
		from = p.NextBatch
	}

	counts := map[string]int{}
	from = ""
	scanned := 0
	for scanned < maxScanEvents {
		p, err := s.client.Messages(ctx, roomID, from, "b", 100)
		if err != nil {
			return nil, err
		}
		for _, ev := range p.Chunk {
			if ev.Type != "m.room.message" {
				continue
			}
			if root := ev.ThreadRoot(); root != "" {
				counts[root]++
				continue
			}
			if strings.HasPrefix(ev.Message().Body, TopicPrefix) {
				add(ev, 0)
			}
		}
		scanned += len(p.Chunk)
		if p.End == "" || p.End == from || len(p.Chunk) == 0 {
			break
		}
		from = p.End
	}
	if !threadsOK {
		for id, t := range byID {
			t.Replies = counts[id]
		}
	}
	out := make([]topicInfo, 0, len(byID))
	for _, t := range byID {
		out = append(out, *t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TS != out[j].TS {
			return out[i].TS < out[j].TS
		}
		return out[i].EventID < out[j].EventID
	})
	return out, nil
}

// resolveTopic maps --topic (slug or $event_id) to the root event id and slug.
func (s *session) resolveTopic(ctx context.Context, roomID, roomLabel, topic string) (string, string, error) {
	if strings.HasPrefix(topic, "$") {
		ev, err := s.client.GetEvent(ctx, roomID, topic)
		if err != nil {
			if isNotFound(err) {
				return "", "", fmt.Errorf("topic event %s not found in %s", topic, roomLabel)
			}
			return "", "", err
		}
		if ev.ThreadRoot() != "" {
			return "", "", fmt.Errorf("%s is a thread reply, not a topic root (use the root: %s)", topic, ev.ThreadRoot())
		}
		slug, _, _ := parseTopicBody(ev.Message().Body)
		return topic, slug, nil
	}
	if !slugRE.MatchString(topic) {
		return "", "", usagef("invalid topic %q: use a slug (a-z, 0-9, -) or a $event_id", topic)
	}
	topics, err := s.listTopics(ctx, roomID)
	if err != nil {
		return "", "", err
	}
	for _, t := range topics {
		if t.Slug == topic {
			return t.EventID, t.Slug, nil
		}
	}
	return "", "", fmt.Errorf("topic %q not found in %s (see: a2a topics %s)", topic, roomLabel, roomLabel)
}

// rememberThread records a thread this agent takes part in, so `a2a wait` rings on replies.
func (s *session) rememberThread(root string) error {
	_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
		st.AddThread(root)
		return nil
	})
	return err
}

// cmdTopic: a2a topic new <room> <slug> "<title>" [--body -|@file|text]
func cmdTopic(ctx context.Context, a *App, args []string) error {
	const use = `usage: a2a topic new <room> <slug> "<title>" [--body -|@file|text]`
	if len(args) == 0 || args[0] != "new" {
		return usagef(use)
	}
	fs := newFlagSet("topic new")
	bodyArg := fs.String("body", "", "topic body: - (stdin), @file, or text")
	pos, err := parseInterspersed(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) < 3 {
		return usagef(use)
	}
	slug, title := pos[1], sanitizeLine(strings.Join(pos[2:], " "))
	if !slugRE.MatchString(slug) {
		return usagef("invalid slug %q: lowercase letters, digits, and -, max 64", slug)
	}
	if title == "" {
		return usagef("empty topic title refused")
	}
	var body string
	if *bodyArg != "" {
		if body, err = a.readBody(*bodyArg); err != nil {
			return err
		}
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	roomID, label, err := s.resolveRoom(ctx, pos[0])
	if err != nil {
		return err
	}
	existing, err := s.listTopics(ctx, roomID)
	if err != nil {
		return err
	}
	for _, t := range existing {
		if t.Slug == slug {
			return fmt.Errorf("topic %q already exists in %s (%s)", slug, label, t.EventID)
		}
	}
	text := TopicPrefix + slug + ": " + title
	if body != "" {
		text += "\n\n" + body
	}
	content := map[string]any{
		"msgtype": "m.text",
		"body":    text,
		ContentKey: Header{
			V: 1, Type: "topic", Slug: slug, Title: title, From: s.name,
			At: a.Now().UTC().Format(time.RFC3339),
		},
	}
	id, err := s.client.SendMessage(ctx, roomID, content)
	if err != nil {
		return fmt.Errorf("topic new in %s: %w", label, err)
	}
	if err := s.rememberThread(id); err != nil {
		fmt.Fprintf(a.Stderr, "a2a: warning: could not record thread in state: %v\n", err)
	}
	a.printf("%s\n", id)
	return nil
}

// cmdTopics: a2a topics <room>
func cmdTopics(ctx context.Context, a *App, args []string) error {
	if len(args) != 1 {
		return usagef("usage: a2a topics <room>")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	roomID, _, err := s.resolveRoom(ctx, args[0])
	if err != nil {
		return err
	}
	topics, err := s.listTopics(ctx, roomID)
	if err != nil {
		return err
	}
	for _, t := range topics {
		slug := t.Slug
		if slug == "" {
			slug = "-"
		}
		a.printf("%s\t%s\t%d\t%s\n", slug, t.EventID, t.Replies, sanitizeLine(t.Title))
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func validKind(k string) bool {
	for _, x := range Kinds {
		if x == k {
			return true
		}
	}
	return false
}

// cmdPost: a2a post <room> --topic <slug|$event> [--kind K] [--re $ev] [--to a,b] [--reply-by T] [--trunk OID] <text|@file|->
func cmdPost(ctx context.Context, a *App, args []string) error {
	const use = "usage: a2a post <room> --topic <slug|$event_id> [--kind K] [--re $event] [--to a,b] [--reply-by RFC3339] [--trunk OID] <text|@file|->"
	fs := newFlagSet("post")
	topic := fs.String("topic", "", "topic slug or root event id")
	kind := fs.String("kind", "notice", "message kind")
	re := fs.String("re", "", "event id this message answers")
	to := fs.String("to", "all", "comma-separated addressees")
	replyBy := fs.String("reply-by", "", "reply deadline (RFC3339)")
	trunk := fs.String("trunk", "", "repo OID the message talks about")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		if len(pos) == 1 {
			return usagef("empty message body refused")
		}
		return usagef(use)
	}
	if *topic == "" {
		return usagef("--topic is required (%s)", use)
	}
	if !validKind(*kind) {
		return usagef("invalid --kind %q (one of: %s)", *kind, strings.Join(Kinds, ", "))
	}
	if *re != "" && !strings.HasPrefix(*re, "$") {
		return usagef("invalid --re %q: must be an event id ($...)", *re)
	}
	if *trunk != "" && !trunkRE.MatchString(*trunk) {
		return usagef("invalid --trunk %q: must be a hex git object id (7-64 chars)", *trunk)
	}
	now := a.Now().UTC()
	rb := ""
	if *replyBy != "" {
		t, err := time.Parse(time.RFC3339, *replyBy)
		if err != nil {
			return usagef("invalid --reply-by %q: use RFC3339, for example 2026-09-29T12:00:00Z", *replyBy)
		}
		rb = t.UTC().Format(time.RFC3339)
	} else if *kind == "question" {
		rb = now.Add(DefaultReplyBy).Format(time.RFC3339)
	}
	bodyArg := strings.Join(pos[1:], " ")
	if len(pos) > 2 && (pos[1] == "-" || strings.HasPrefix(pos[1], "@")) {
		return usagef("give exactly one body: text, @file, or -")
	}
	text, err := a.readBody(bodyArg)
	if err != nil {
		return err
	}

	s, err := a.session()
	if err != nil {
		return err
	}
	toNames, mentions, err := addressees(*to, s.server)
	if err != nil {
		return err
	}
	roomID, label, err := s.resolveRoom(ctx, pos[0])
	if err != nil {
		return err
	}
	root, slug, err := s.resolveTopic(ctx, roomID, label, *topic)
	if err != nil {
		return err
	}

	h := Header{V: 1, Type: "message", Slug: slug, Topic: root, From: s.name, To: toNames,
		At: now.Format(time.RFC3339), Re: *re, Kind: *kind, ReplyBy: rb, Trunk: *trunk}
	content := threadReply(h, mentions)
	content["msgtype"] = "m.text"
	content["body"] = formatHeaderBody(h, text)
	id, err := s.client.SendMessage(ctx, roomID, content)
	if err != nil {
		return fmt.Errorf("post to %s: %w", label, err)
	}
	if err := s.rememberThread(root); err != nil {
		fmt.Fprintf(a.Stderr, "a2a: warning: could not record thread in state: %v\n", err)
	}
	a.printf("%s\n", id)
	return nil
}

// addressees turns --to into header names and m.mentions user ids.
func addressees(to, server string) (names, mentions []string, err error) {
	for _, t := range splitList(to) {
		if t == "all" {
			names = append(names, "all")
			continue
		}
		uid, err := normalizeUser(t, server)
		if err != nil {
			return nil, nil, err
		}
		names = append(names, localpartIfLocal(uid, server))
		mentions = append(mentions, uid)
	}
	if len(names) == 0 {
		names = []string{"all"}
	}
	return names, mentions, nil
}

// threadReply returns the content shared by every thread reply (post, attach):
// the structured header, the m.thread relation to h.Topic, and m.mentions.
// The caller adds msgtype and body.
func threadReply(h Header, mentions []string) map[string]any {
	return map[string]any{
		ContentKey: h,
		"m.relates_to": matrix.RelatesTo{
			RelType:       "m.thread",
			EventID:       h.Topic,
			IsFallingBack: h.Re == "",
			InReplyTo:     &matrix.InReplyTo{EventID: firstNonEmpty(h.Re, h.Topic)},
		},
		"m.mentions": matrix.Mentions{UserIDs: mentions},
	}
}

// formatHeaderBody renders the protocol header lines (rule 2) above the text.
func formatHeaderBody(h Header, text string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- from: %s\n", h.From)
	fmt.Fprintf(&b, "- to: %s\n", strings.Join(h.To, ", "))
	fmt.Fprintf(&b, "- at: %s\n", h.At)
	fmt.Fprintf(&b, "- re: %s\n", firstNonEmpty(h.Re, "-"))
	fmt.Fprintf(&b, "- kind: %s\n", h.Kind)
	if h.ReplyBy != "" {
		fmt.Fprintf(&b, "- reply-by: %s\n", h.ReplyBy)
	}
	if h.Trunk != "" {
		fmt.Fprintf(&b, "- trunk: %s\n", h.Trunk)
	}
	b.WriteString("\n")
	b.WriteString(text)
	return b.String()
}

// stripHeaderBody returns the text after the protocol header lines, if present.
func stripHeaderBody(body string) string {
	if !strings.HasPrefix(body, "- from: ") {
		return body
	}
	if _, rest, ok := strings.Cut(body, "\n\n"); ok {
		return rest
	}
	return body
}

func localpartIfLocal(userID, server string) string {
	if strings.HasSuffix(userID, ":"+server) {
		return localpart(userID)
	}
	return userID
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// cmdRead: a2a read <room> [--topic X] [--last N] [--event ID] [--json]
func cmdRead(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("read")
	topic := fs.String("topic", "", "topic slug or root event id")
	last := fs.Int("last", 20, "number of messages")
	event := fs.String("event", "", "print only this event id")
	asJSON := fs.Bool("json", false, "one JSON object per event")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: a2a read <room> [--topic X] [--last N] [--event ID] [--json]")
	}
	if *last < 1 || *last > 1000 {
		return usagef("--last must be between 1 and 1000")
	}
	lastSet := false
	fs.Visit(func(f *flag.Flag) { lastSet = lastSet || f.Name == "last" })
	if *event != "" && (*topic != "" || lastSet) {
		return usagef("--event cannot be combined with --topic or --last")
	}
	if *event != "" && !strings.HasPrefix(*event, "$") {
		return usagef("--event must be an event id ($...)")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	roomID, label, err := s.resolveRoom(ctx, pos[0])
	if err != nil {
		return err
	}
	var events []matrix.Event
	if *event != "" {
		ev, err := s.client.GetEvent(ctx, roomID, *event)
		if err != nil {
			if isNotFound(err) {
				return fmt.Errorf("event %s not found in %s", sanitizeLine(*event), label)
			}
			return err
		}
		if ev.Type != "m.room.message" {
			return fmt.Errorf("event %s is %s, not a message", sanitizeLine(*event), sanitizeLine(ev.Type))
		}
		events = []matrix.Event{ev}
	} else if *topic != "" {
		root, _, err := s.resolveTopic(ctx, roomID, label, *topic)
		if err != nil {
			return err
		}
		rootEv, err := s.client.GetEvent(ctx, roomID, root)
		if err != nil {
			return err
		}
		replies, err := s.collect(ctx, *last, func(from string) (matrix.Page, error) {
			return s.client.ThreadReplies(ctx, roomID, root, from, "b", 100)
		})
		if err != nil {
			return err
		}
		events = append([]matrix.Event{rootEv}, replies...)
	} else {
		events, err = s.collect(ctx, *last, func(from string) (matrix.Page, error) {
			return s.client.Messages(ctx, roomID, from, "b", 100)
		})
		if err != nil {
			return err
		}
	}
	for _, ev := range events {
		if !*asJSON {
			a.printEvent(ev, s.server)
			continue
		}
		typ := "message"
		if h, ok := decodeHeader(ev.Message().A2A); ok && h.Type == "topic" {
			typ = "topic"
		}
		if err := a.printJSON(eventRecord(typ, label, roomID, ev, s.server, false)); err != nil {
			return err
		}
	}
	return nil
}

// collect pages newest-first until n message events are gathered, then
// returns them oldest first.
func (s *session) collect(ctx context.Context, n int, page func(from string) (matrix.Page, error)) ([]matrix.Event, error) {
	return s.collectWhere(ctx, n, nil, page)
}

// collectWhere is collect restricted to message events for which keep returns
// true. With a filter, the scan stops after maxScanEvents events.
func (s *session) collectWhere(ctx context.Context, n int, keep func(matrix.Event) bool, page func(from string) (matrix.Page, error)) ([]matrix.Event, error) {
	var out []matrix.Event
	from := ""
	scanned := 0
	for len(out) < n && (keep == nil || scanned < maxScanEvents) {
		p, err := page(from)
		if err != nil {
			return nil, err
		}
		for _, ev := range p.Chunk {
			if ev.Type == "m.room.message" && len(out) < n && (keep == nil || keep(ev)) {
				out = append(out, ev)
			}
		}
		scanned += len(p.Chunk)
		next := firstNonEmpty(p.End, p.NextBatch)
		if next == "" || next == from || len(p.Chunk) == 0 {
			break
		}
		from = next
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (a *App) printEvent(ev matrix.Event, server string) {
	m := ev.Message()
	ts := time.UnixMilli(ev.OriginServerTS).UTC().Format(time.RFC3339)
	meta := []string{}
	if h, ok := decodeHeader(m.A2A); ok {
		if h.Kind != "" {
			meta = append(meta, "kind="+h.Kind)
		}
		if len(h.To) > 0 {
			meta = append(meta, "to="+strings.Join(h.To, ","))
		}
		if h.Re != "" {
			meta = append(meta, "re="+h.Re)
		}
		if h.ReplyBy != "" {
			meta = append(meta, "reply-by="+h.ReplyBy)
		}
		if h.Trunk != "" {
			meta = append(meta, "trunk="+h.Trunk)
		}
		if h.Attachment != nil {
			meta = append(meta, "file="+h.Attachment.Name, fmt.Sprintf("size=%d", h.Attachment.Size))
		}
	}
	if root := ev.ThreadRoot(); root != "" {
		meta = append(meta, "thread="+root)
	}
	line := fmt.Sprintf("[%s] %s %s", ts, localpartIfLocal(ev.Sender, server), ev.EventID)
	if len(meta) > 0 {
		line += " " + strings.Join(meta, " ")
	}
	a.printf("%s\n", sanitizeLine(line))
	body := sanitizeText(m.Body)
	if _, ok := decodeHeader(m.A2A); ok {
		body = stripHeaderBody(body)
	}
	for _, l := range strings.Split(body, "\n") {
		a.printf("    %s\n", l)
	}
}
