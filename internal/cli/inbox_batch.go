package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/redact"
)

const (
	inboxFullBodyMaxRunes   = 4000
	inboxFullOutputMax      = 1 << 20
	inboxProjectionFieldMax = 512
)

// inboxBatchProjection is the sanitized, display-ready full inbox view.
// Arrival is local observation time;
// OriginAt is Matrix's event time. Empty arrival is rendered as unknown.
type inboxBatchProjection struct {
	RecordType   string                          `json:"record_type"`
	ArrivalAt    string                          `json:"arrival_at"`
	OriginAt     string                          `json:"origin_at"`
	Sender       string                          `json:"sender"`
	Room         string                          `json:"room"`
	RoomID       string                          `json:"room_id"`
	Topic        string                          `json:"topic"`
	Thread       string                          `json:"thread"`
	Kind         string                          `json:"kind"`
	Relation     string                          `json:"relation"`
	EventID      string                          `json:"event_id"`
	Attachment   *inboxBatchAttachmentProjection `json:"attachment,omitempty"`
	Verification string                          `json:"verification,omitempty"`
	Redacted     bool                            `json:"redacted,omitempty"`
	Body         string                          `json:"body"`
	hasMessage   bool                            `json:"-"`
}

type inboxBatchAttachmentProjection struct {
	Name string `json:"name"`
	Size *int64 `json:"size"`
}

func projectInboxRecord(rec EventRecord, relation string, arrival *time.Time, attachmentSize *int64, hasMessage bool) inboxBatchProjection {
	arrivalAt := "unknown"
	if arrival != nil && !arrival.IsZero() {
		arrivalAt = arrival.UTC().Format(time.RFC3339Nano)
	}
	originAt := ""
	if rec.AtMS != 0 {
		originAt = time.UnixMilli(rec.AtMS).UTC().Format(time.RFC3339Nano)
	} else {
		originAt = strings.TrimSpace(rec.At)
	}
	if originAt == "" {
		originAt = "unknown"
	}
	body := rec.Body
	if rec.Redacted {
		body = "[redacted]"
	}
	p := inboxBatchProjection{
		RecordType:   rec.Type,
		ArrivalAt:    arrivalAt,
		OriginAt:     originAt,
		Sender:       rec.Sender,
		Room:         firstNonEmpty(rec.Room, rec.RoomID),
		RoomID:       rec.RoomID,
		Topic:        rec.Topic,
		Thread:       rec.Root,
		Kind:         rec.Kind,
		Relation:     relation,
		EventID:      rec.EventID,
		Verification: rec.Verification,
		Redacted:     rec.Redacted,
		Body:         sanitizeText(redactString(body)),
		hasMessage:   hasMessage && !rec.Redacted,
	}
	if rec.Attachment != nil {
		name := sanitizeLine(rec.Attachment.Name)
		if name == "" {
			name = "unknown"
		}
		p.Attachment = &inboxBatchAttachmentProjection{Name: name, Size: attachmentSize}
	}
	return p
}

// inboxRelation uses the same pure classifier as delivery. The stored inbox
// relation remains the source for subscription-only mail, because the
// classifier's direct-address decision intentionally precedes subscriptions.
func inboxRelation(it waitItem, s *session, st config.State) string {
	if it.ev != nil && s != nil {
		rel, _, _ := classify(*it.ev, it.rec.RoomID, s.creds.UserID, s.name, st, nil, nil)
		if rel == relTo || rel == relMention {
			return "addressed"
		}
	}
	switch it.rec.Type {
	case string(relTo), string(relMention):
		return "addressed"
	case string(relSub):
		return "subscription"
	case string(relThread):
		return "thread"
	default:
		return "unknown"
	}
}

// declaredInboxAttachmentSize preserves the distinction between a missing
// declared size and an explicitly declared zero. It reads only the event's
// attachment metadata; it never fetches media.
func declaredInboxAttachmentSize(it waitItem) *int64 {
	if it.ev == nil {
		return nil
	}
	var header struct {
		Attachment *struct {
			Size *int64 `json:"size"`
		} `json:"attachment"`
	}
	if err := json.Unmarshal(it.ev.Message().A2A, &header); err != nil || header.Attachment == nil || header.Attachment.Size == nil || *header.Attachment.Size < 0 {
		return nil
	}
	size := *header.Attachment.Size
	return &size
}

func projectInboxBatch(items []waitItem, records []EventRecord, s *session, st config.State) ([]inboxBatchProjection, error) {
	if len(items) != len(records) {
		return nil, fmt.Errorf("inbox projection count does not match saved records")
	}
	projected := make([]inboxBatchProjection, 0, len(records))
	for i, rec := range records {
		var arrival *time.Time // The current mailbox adapter does not persist local arrival yet.
		projected = append(projected, projectInboxRecord(rec, inboxRelation(items[i], s, st), arrival, declaredInboxAttachmentSize(items[i]), items[i].ev != nil && items[i].gap == nil))
	}
	return projected, nil
}

func projectionField(v string, quote bool) (string, error) {
	if utf8.RuneCountInString(v) > inboxProjectionFieldMax {
		return "", fmt.Errorf("full inbox metadata field exceeds %d characters", inboxProjectionFieldMax)
	}
	v = sanitizeLine(v)
	if v == "" {
		v = "-"
	}
	if quote {
		return fmt.Sprintf("%q", v), nil
	}
	return v, nil
}

func inboxProjectionHeader(p inboxBatchProjection, index, count int) (string, error) {
	arrival := validProjectionTime(p.ArrivalAt)
	origin := validProjectionTime(p.OriginAt)
	relation := p.Relation
	if relation != "addressed" && relation != "subscription" && relation != "thread" {
		relation = "unknown"
	}
	from, err := projectionField(p.Sender, false)
	if err != nil {
		return "", err
	}
	room, err := projectionField(firstNonEmpty(p.Room, p.RoomID), true)
	if err != nil {
		return "", err
	}
	topic, err := projectionField(p.Topic, true)
	if err != nil {
		return "", err
	}
	thread, err := projectionField(p.Thread, true)
	if err != nil {
		return "", err
	}
	kind, err := projectionField(p.Kind, true)
	if err != nil {
		return "", err
	}
	id, err := projectionField(p.EventID, true)
	if err != nil {
		return "", err
	}
	typ, err := projectionField(p.RecordType, true)
	if err != nil {
		return "", err
	}
	if p.Sender != "" {
		from = senderText(p.Sender)
	}
	if p.Topic == "" {
		topic = "-"
	}
	if p.Thread == "" {
		thread = "-"
	}
	if p.Kind == "" {
		kind = "-"
	}
	if p.EventID == "" {
		id = "-"
	}
	if p.RecordType == "" {
		typ = "unknown"
	}
	fields := []string{
		fmt.Sprintf("--- event %d/%d:", index, count),
		"arrival=" + arrival,
		"origin=" + origin,
		"type=" + typ,
		"from=" + from,
		"room=" + room,
		"topic=" + topic,
		"thread=" + thread,
		"kind=" + kind,
		"relation=" + relation,
		"id=" + id,
	}
	if p.Attachment != nil {
		name, err := projectionField(p.Attachment.Name, true)
		if err != nil {
			return "", err
		}
		size := "unknown"
		if p.Attachment.Size != nil {
			size = fmt.Sprint(*p.Attachment.Size)
		}
		fields = append(fields, "file="+name, "size="+size)
	}
	if p.Verification != "" {
		verification, err := projectionField(p.Verification, true)
		if err != nil {
			return "", err
		}
		fields = append(fields, "verification="+verification)
	}
	return strings.Join(fields, " "), nil
}

func validProjectionTime(raw string) string {
	if raw == "unknown" || raw == "" {
		return "unknown"
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return "unknown"
	}
	return at.UTC().Format(time.RFC3339Nano)
}

func renderInboxFull(projected []inboxBatchProjection, more int, savedPath string) (string, []bool, error) {
	var b strings.Builder
	complete := make([]bool, len(projected))
	b.WriteString(inboxBegin + "\n")
	for i, p := range projected {
		header, err := inboxProjectionHeader(p, i+1, len(projected))
		if err != nil {
			return "", nil, err
		}
		b.WriteString(header + "\n")
		body, cut := truncateRunes(p.Body, inboxFullBodyMaxRunes)
		writeInboxBody(&b, body)
		if cut > 0 {
			fmt.Fprintf(&b, "%s[truncated: %d more characters; full text in saved inbox: %q]\n", inboxIndent, cut, savedPath)
		}
		complete[i] = p.hasMessage && cut == 0
	}
	b.WriteString(inboxEnd + "\n")
	if more > 0 {
		fmt.Fprintf(&b, "(%d more pending: run a2a inbox again)\n", more)
	}
	if b.Len() > inboxFullOutputMax {
		return "", nil, fmt.Errorf("full inbox output exceeds %d bytes; use --full --out FILE for a short summary", inboxFullOutputMax)
	}
	return b.String(), complete, nil
}

func renderInboxFullSummary(projected []inboxBatchProjection, more, width int, savedPath string) (string, error) {
	var b strings.Builder
	b.WriteString(inboxBegin + "\n")
	for i, p := range projected {
		header, err := inboxProjectionHeader(p, i+1, len(projected))
		if err != nil {
			return "", err
		}
		excerpt, cut := truncateRunes(sanitizeLine(p.Body), width)
		b.WriteString(header + "\n")
		if p.Redacted {
			excerpt = "[redacted]"
			cut = 0
		}
		fmt.Fprintf(&b, "%s%s\n", inboxIndent, excerpt)
		if cut > 0 {
			fmt.Fprintf(&b, "%s[truncated: %d more characters; full text in saved inbox: %q]\n", inboxIndent, cut, savedPath)
		}
	}
	b.WriteString(inboxEnd + "\n")
	fmt.Fprintf(&b, "saved inbox: %q\n", savedPath)
	if more > 0 {
		fmt.Fprintf(&b, "(%d more pending: run a2a inbox again)\n", more)
	}
	if b.Len() > inboxFullOutputMax {
		return "", fmt.Errorf("full inbox summary exceeds %d bytes", inboxFullOutputMax)
	}
	return b.String(), nil
}

type inboxFullJSONEvent struct {
	inboxBatchProjection
	BodyCutCharacters int `json:"body_cut_characters,omitempty"`
}

type inboxFullJSONFrame struct {
	Type        string               `json:"type"`
	Version     int                  `json:"version"`
	Trust       string               `json:"trust"`
	FrameBegin  string               `json:"frame_begin"`
	Events      []inboxFullJSONEvent `json:"events"`
	FrameEnd    string               `json:"frame_end"`
	MorePending int                  `json:"more_pending,omitempty"`
	SavedFile   string               `json:"saved_file"`
}

func renderInboxFullJSON(projected []inboxBatchProjection, more int, savedPath string) (string, []bool, error) {
	frame := inboxFullJSONFrame{
		Type:        "a2a-inbox-full",
		Version:     1,
		Trust:       "untrusted-peer-data",
		FrameBegin:  inboxBegin,
		FrameEnd:    inboxEnd,
		MorePending: more,
		SavedFile:   savedPath,
		Events:      make([]inboxFullJSONEvent, 0, len(projected)),
	}
	complete := make([]bool, len(projected))
	for i, p := range projected {
		if _, err := inboxProjectionHeader(p, i+1, len(projected)); err != nil {
			return "", nil, err
		}
		body, cut := truncateRunes(p.Body, inboxFullBodyMaxRunes)
		p.Body = body
		frame.Events = append(frame.Events, inboxFullJSONEvent{inboxBatchProjection: p, BodyCutCharacters: cut})
		complete[i] = p.hasMessage && cut == 0
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return "", nil, err
	}
	data = escapeJSONFormatCharacters(data)
	if len(data) > inboxFullOutputMax {
		return "", nil, fmt.Errorf("full inbox JSON exceeds %d bytes; use --full --out FILE for a short summary", inboxFullOutputMax)
	}
	return string(data) + "\n", complete, nil
}

// redactString mirrors the existing saved-frame redactor for projections
// created from EventRecords. The application-wide scrub is applied at output.
func redactString(v string) string { return redact.String(v) }
