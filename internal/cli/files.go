package cli

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// PinnedEventsType is the state event that lists a room's pinned events.
const PinnedEventsType = "m.room.pinned_events"

var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// safeFileName reports whether name may be used as a file name in the
// current directory: a single path element that is not empty, ".", "..",
// has no separators, NUL or control characters, and does not start with "-".
func safeFileName(name string) error {
	switch {
	case name == "", name == ".", name == "..":
		return fmt.Errorf("unsafe file name %q refused", sanitizeLine(name))
	case strings.ContainsAny(name, "/\\\x00"), strings.HasPrefix(name, "-"):
		return fmt.Errorf("unsafe file name %q refused", sanitizeLine(name))
	}
	for _, r := range name {
		if isUnsafeRune(r) {
			return fmt.Errorf("unsafe file name %q refused", sanitizeLine(name))
		}
	}
	return nil
}

// attachmentOf returns the attachment of a message event, if it has one.
func attachmentOf(ev matrix.Event) (*Attachment, bool) {
	if ev.Type != "m.room.message" {
		return nil, false
	}
	h, ok := decodeHeader(ev.Message().A2A)
	if !ok || h.Attachment == nil {
		return nil, false
	}
	return h.Attachment, true
}

// attachmentForFetch decodes once. The selected pointer carries both the
// required size evidence and every field consumed by fetch, including through
// repeated attachment keys and nulls. Attachment's public wire shape is unchanged.
func attachmentForFetch(ev matrix.Event) (*Attachment, error) {
	var h struct {
		Header
		Attachment *struct {
			Attachment
			Size *int64 `json:"size"`
		} `json:"attachment"`
	}
	if ev.Type != "m.room.message" || json.Unmarshal(ev.Message().A2A, &h) != nil || h.Attachment == nil {
		return nil, fmt.Errorf("event %s has no a2a attachment", sanitizeLine(ev.EventID))
	}
	selected := h.Attachment
	if selected.Size == nil || *selected.Size <= 0 {
		return nil, fmt.Errorf("event %s: attachment has no valid size", sanitizeLine(ev.EventID))
	}
	att := selected.Attachment
	att.Size = *selected.Size
	return &att, nil
}

// cmdAttach: a2a attach <room> --topic <slug|$root> <file> [--kind K] [--to a,b] [--pin] [caption text|@file|-]
func cmdAttach(ctx context.Context, a *App, args []string) error {
	const use = "usage: a2a attach <room> --topic <slug|$event_id> <file> [--kind K] [--to a,b] [--pin] [caption text|@file|-]"
	fset := newFlagSet("attach")
	topic := fset.String("topic", "", "topic slug or root event id")
	kind := fset.String("kind", "notice", "message kind")
	to := fset.String("to", "all", "comma-separated addressees")
	pin := fset.Bool("pin", false, "also pin the message")
	pos, err := parseInterspersed(fset, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return usagef(use)
	}
	if *topic == "" {
		return usagef("--topic is required (%s)", use)
	}
	if !validKind(*kind) {
		return usagef("invalid --kind %q (one of: %s)", *kind, strings.Join(Kinds, ", "))
	}
	caption := ""
	if len(pos) > 2 {
		if len(pos) > 3 && (pos[2] == "-" || strings.HasPrefix(pos[2], "@")) {
			return usagef("give exactly one caption: text, @file, or -")
		}
		if caption, err = a.readBody(strings.Join(pos[2:], " ")); err != nil {
			return err
		}
	}

	path := pos[1]
	name := filepath.Base(path)
	if err := safeFileName(name); err != nil {
		return usageError{err.Error()}
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	if !st.Mode().IsRegular() {
		return usagef("attach: %s is not a regular file", sanitizeLine(path))
	}
	size := st.Size()
	if size == 0 {
		return usagef("attach: empty file refused")
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("attach: read %s: %w", sanitizeLine(path), err)
	}
	mime := http.DetectContentType(head[:n])
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("attach: %w", err)
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
	max, err := s.client.MediaConfig(ctx)
	if err != nil {
		return fmt.Errorf("attach: media config: %w", err)
	}

	limit, err := attachmentLimit(max)
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	if size > limit {
		return fmt.Errorf("attach: file is %d bytes, over the server's upload limit of %d bytes", size, limit)
	}
	att := &Attachment{Name: name, Size: size, MimeType: mime}
	body, err := jsonObjectFile(f, size)
	if err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	var upload io.Reader = f
	if body != nil {
		var encoded bytes.Buffer
		zw := gzip.NewWriter(&encoded)
		if _, err := zw.Write(body); err != nil {
			return fmt.Errorf("attach: gzip: %w", err)
		}
		if err := zw.Close(); err != nil {
			return fmt.Errorf("attach: gzip: %w", err)
		}
		storedSize := int64(encoded.Len())
		if err := validateGzipSizes(storedSize, size, limit); err != nil {
			return fmt.Errorf("attach: gzip: %w", err)
		}
		originalHash := sha256.Sum256(body)
		descriptor := attachmentEncoding{Version: attachmentEncodingVersion, Coding: "gzip", Reason: attachmentEncodingReason,
			Original: AttachmentOriginal{Name: name, Size: size, SHA256: hex.EncodeToString(originalHash[:]), MimeType: mime}}
		att.Encoding, err = json.Marshal(descriptor)
		if err != nil {
			return fmt.Errorf("attach: encoding: %w", err)
		}
		att.Name, att.Size, att.MimeType = name+".gz", storedSize, "application/gzip"
		upload = bytes.NewReader(encoded.Bytes())
	}

	sum := sha256.New()
	cnt := &countWriter{}
	mxc, err := s.client.Upload(ctx, att.Name, att.MimeType, io.TeeReader(upload, io.MultiWriter(sum, cnt)), att.Size)
	if err != nil {
		return fmt.Errorf("attach: upload: %w", err)
	}
	if cnt.n != att.Size {
		return fmt.Errorf("attach: %s changed while uploading (%d of %d bytes read)", sanitizeLine(path), cnt.n, att.Size)
	}
	att.MXC = mxc
	att.SHA256 = hexSum(sum)

	h := Header{V: 1, Type: "message", Slug: slug, Topic: root, From: s.name, To: toNames,
		At: a.Now().UTC().Format(time.RFC3339), Kind: *kind, Attachment: att}
	msgtype := "m.file"
	if strings.HasPrefix(mime, "image/") {
		msgtype = "m.image"
	}
	content := threadReply(h, mentions)
	content["msgtype"] = msgtype
	content["body"] = formatHeaderBody(h, firstNonEmpty(caption, att.Name))
	content["filename"] = att.Name
	content["url"] = mxc
	content["info"] = map[string]any{"size": att.Size, "mimetype": att.MimeType}
	receipt, err := s.client.SendMessageReceipt(ctx, roomID, content)
	if err != nil {
		return fmt.Errorf("attach to %s: %w", label, err)
	}
	id := receipt.EventID()
	if err := s.rememberThread(root); err != nil {
		fmt.Fprintf(a.Stderr, "a2a: warning: could not record thread in state: %v\n", err)
	}
	a.printf("%s\n", id)
	if *pin {
		if _, err := s.pin(ctx, roomID, id, true); err != nil {
			return fmt.Errorf("attach: sent %s but could not pin it: %w", id, err)
		}
	}
	return nil
}

// jsonObjectFile only buffers candidate objects; other media keep the existing
// streaming upload. Seek back for every raw path, including invalid JSON.
func jsonObjectFile(f *os.File, size int64) ([]byte, error) {
	r := bufio.NewReader(f)
	object := false
	for {
		b, err := r.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			continue
		}
		object = b == '{'
		break
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if !object {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(f, size))
	if err != nil {
		return nil, err
	}
	var extra [1]byte
	n, err := f.Read(extra[:])
	if err != nil && err != io.EOF {
		return nil, err
	}
	if int64(len(body)) != size || n != 0 {
		return nil, errors.New("file changed while reading JSON object")
	}
	// Workaround for tuwunel rewriting JSON-object media bodies (matrix-construct/tuwunel#666); relevant until the homeserver runs a version with the fix. Remove the gzip path then; fetch must keep decoding old events.
	if json.Valid(body) {
		return body, nil
	}
	_, err = f.Seek(0, io.SeekStart)
	return nil, err
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// cmdFetch: a2a fetch <room> <$event_id> [-o path] | a2a fetch <mxc://...> --sha256 <hex> [-o path]
func cmdFetch(ctx context.Context, a *App, args []string) error {
	const use = "usage: a2a fetch <room> <$event_id> [-o path] | a2a fetch <mxc://...> --sha256 <hex> [-o path]"
	fset := newFlagSet("fetch")
	out := fset.String("o", "", "output path (never overwritten)")
	want := fset.String("sha256", "", "expected sha256 (bare mxc only)")
	pos, err := parseInterspersed(fset, args)
	if err != nil {
		return err
	}
	var bare *matrix.MXC
	switch {
	case len(pos) == 1 && strings.HasPrefix(pos[0], "mxc:"):
		m, err := matrix.ParseMXC(pos[0])
		if err != nil {
			return usageError{err.Error()}
		}
		if *want == "" {
			return usagef("fetch of a bare mxc URI needs --sha256 <hex>")
		}
		*want = strings.ToLower(*want)
		if !sha256RE.MatchString(*want) {
			return usagef("invalid --sha256: must be 64 hex characters")
		}
		bare = &m
	case len(pos) == 2 && strings.HasPrefix(pos[1], "$"):
		if *want != "" {
			return usagef("--sha256 is only for a bare mxc URI; an event carries its own checksum")
		}
	default:
		return usagef(use)
	}

	s, err := a.session()
	if err != nil {
		return err
	}
	var att Attachment
	if bare != nil {
		att = Attachment{Name: bare.MediaID, MXC: bare.String(), SHA256: *want, Size: -1}
	} else {
		roomID, label, err := s.resolveRoom(ctx, pos[0])
		if err != nil {
			return err
		}
		ev, err := s.client.GetEvent(ctx, roomID, pos[1])
		if err != nil {
			if isNotFound(err) {
				return fmt.Errorf("event %s not found in %s", sanitizeLine(pos[1]), label)
			}
			return err
		}
		p, err := attachmentForFetch(ev)
		if err != nil {
			return err
		}
		att = *p
		if !sha256RE.MatchString(att.SHA256) {
			return fmt.Errorf("event %s: attachment has no valid sha256", sanitizeLine(pos[1]))
		}
		if _, err := matrix.ParseMXC(att.MXC); err != nil {
			return fmt.Errorf("event %s: attachment %w", sanitizeLine(pos[1]), err)
		}
	}

	// Structural validation precedes even media config. The returned descriptor
	// contains exactly the validated selected values; never decode it again.
	descriptor, err := parseAttachmentEncoding(att.Encoding, attachmentMaxLimit)
	if err != nil {
		return err
	}
	max, err := s.client.MediaConfig(ctx)
	if err != nil {
		return fmt.Errorf("fetch: media config: %w", err)
	}
	limit, err := attachmentLimit(max)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	if bare == nil && att.Size > limit {
		return fmt.Errorf("fetch: declared size %d is over the server's upload limit of %d bytes", att.Size, limit)
	}
	if descriptor != nil && descriptor.Version == attachmentEncodingVersion && descriptor.Original.Size > limit {
		return fmt.Errorf("fetch: malformed attachment encoding: invalid original.size (outside limit)")
	}
	decode := descriptor != nil && descriptor.Version == attachmentEncodingVersion && descriptor.Coding == "gzip"
	if decode {
		if err := validateGzipSizes(att.Size, descriptor.Original.Size, limit); err != nil {
			return fmt.Errorf("fetch: %w", err)
		}
	}
	name := att.Name
	if decode {
		name = descriptor.Original.Name
	}
	target := *out
	if target == "" {
		if err := safeFileName(name); err != nil {
			return err
		}
		target = name
	}
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite", sanitizeLine(target))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("fetch: %w", err)
	}
	if bare == nil {
		limit = att.Size
	}

	rc, err := s.client.Download(ctx, att.MXC)
	if err != nil {
		return fmt.Errorf("fetch: download: %w", err)
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(filepath.Dir(target), ".a2a-fetch-*")
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, sum), io.LimitReader(rc, limit+1))
	if err != nil {
		return fmt.Errorf("fetch: download: %w", err)
	}
	if n > limit {
		return fmt.Errorf("fetch: the server sent more than %d bytes; refused", limit)
	}
	if bare == nil && n != att.Size {
		return fmt.Errorf("fetch: size mismatch: got %d bytes, want %d", n, att.Size)
	}
	if got := hexSum(sum); got != att.SHA256 {
		return fmt.Errorf("fetch: sha256 mismatch: got %s, want %s", got, sanitizeLine(att.SHA256))
	}
	if decode {
		// Stored-byte integrity is established before constructing a gzip reader.
		encoded := tmp
		defer func() { encoded.Close(); os.Remove(encoded.Name()) }()
		if _, err := encoded.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("fetch: %w", err)
		}
		buffered := bufio.NewReader(encoded)
		gz, err := gzip.NewReader(buffered)
		if err != nil {
			return fmt.Errorf("fetch: gzip: %w", err)
		}
		gz.Multistream(false)
		defer gz.Close()
		decoded, err := os.CreateTemp(filepath.Dir(target), ".a2a-fetch-*")
		if err != nil {
			return fmt.Errorf("fetch: %w", err)
		}
		tmp = decoded
		sum = sha256.New()
		original := descriptor.Original
		n, err = io.Copy(io.MultiWriter(tmp, sum), io.LimitReader(gz, original.Size+1))
		if err != nil {
			return fmt.Errorf("fetch: gzip decode: %w", err)
		}
		if n > original.Size {
			return fmt.Errorf("fetch: decoded output exceeds declared original size %d; refused", original.Size)
		}
		if _, err := buffered.ReadByte(); err != io.EOF {
			if err != nil {
				return fmt.Errorf("fetch: gzip trailing data read: %w", err)
			}
			return errors.New("fetch: gzip trailing data or second member refused")
		}
		if n != original.Size {
			return fmt.Errorf("fetch: original size mismatch: got %d bytes, want %d", n, original.Size)
		}
		if got := hexSum(sum); got != original.SHA256 {
			return fmt.Errorf("fetch: original sha256 mismatch: got %s, want %s", got, sanitizeLine(original.SHA256))
		}
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	if err := placeNoClobber(tmp.Name(), target); err != nil {
		return err
	}
	done = true
	if descriptor != nil && !decode {
		label := descriptor.Version
		if descriptor.Version == attachmentEncodingVersion {
			label += "/" + descriptor.Coding
		}
		fmt.Fprintf(a.Stderr, "a2a: attachment encoding %s not supported; saved the stored bytes as %s\n", sanitizeLine(label), sanitizeLine(target))
	}
	a.printf("%s\n", sanitizeLine(target))
	return nil
}

func hexSum(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

// placeNoClobber moves tmp to target without ever replacing an existing file:
// a hard link fails if target exists; filesystems without hard links fall back
// to a checked rename.
func placeNoClobber(tmp, target string) error {
	err := os.Link(tmp, target)
	if err == nil {
		os.Remove(tmp)
		return nil
	}
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s already exists; refusing to overwrite", sanitizeLine(target))
	}
	if _, lerr := os.Lstat(target); lerr == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite", sanitizeLine(target))
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	return nil
}

// cmdFiles: a2a files <room> [--topic X] [--last N] [--json]
func cmdFiles(ctx context.Context, a *App, args []string) error {
	fset := newFlagSet("files")
	topic := fset.String("topic", "", "topic slug or root event id")
	last := fset.Int("last", 20, "number of attachments")
	asJSON := fset.Bool("json", false, "one JSON object per attachment")
	pos, err := parseInterspersed(fset, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("usage: a2a files <room> [--topic X] [--last N] [--json]")
	}
	if *last < 1 || *last > 1000 {
		return usagef("--last must be between 1 and 1000")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	roomID, label, err := s.resolveRoom(ctx, pos[0])
	if err != nil {
		return err
	}
	keep := func(ev matrix.Event) bool { _, ok := attachmentOf(ev); return ok }
	var events []matrix.Event
	if *topic != "" {
		root, _, err := s.resolveTopic(ctx, roomID, label, *topic)
		if err != nil {
			return err
		}
		events, err = s.collectWhere(ctx, *last, keep, func(from string) (matrix.Page, error) {
			return s.client.ThreadReplies(ctx, roomID, root, from, "b", 100)
		})
		if err != nil {
			return err
		}
	} else {
		events, err = s.collectWhere(ctx, *last, keep, func(from string) (matrix.Page, error) {
			return s.client.Messages(ctx, roomID, from, "b", 100)
		})
		if err != nil {
			return err
		}
	}
	for _, ev := range events {
		if *asJSON {
			if err := a.printJSON(eventRecord("message", label, roomID, ev, s.server, false)); err != nil {
				return err
			}
			continue
		}
		at, _ := attachmentOf(ev)
		short := at.SHA256
		if len(short) > 12 {
			short = short[:12]
		}
		a.printf("%s\t%s\t%s\t%d\t%s\t%s%s\n", sanitizeLine(ev.EventID), sanitizeLine(localpartIfLocal(ev.Sender, s.server)),
			sanitizeLine(at.Name), at.Size, sanitizeLine(short), sanitizeLine(at.MXC), attachmentEncodingText(at, "\t"))
	}
	return nil
}

// readPinned returns the room's pinned event ids and the rest of the state
// content (kept on write). A missing state event is an empty list.
func (s *session) readPinned(ctx context.Context, roomID string) ([]string, map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	err := s.client.GetState(ctx, roomID, PinnedEventsType, "", &raw)
	if err != nil {
		if isNotFound(err) {
			return nil, map[string]json.RawMessage{}, nil
		}
		return nil, nil, err
	}
	if raw == nil {
		raw = map[string]json.RawMessage{}
	}
	var pinned []string
	if p, ok := raw["pinned"]; ok {
		_ = json.Unmarshal(p, &pinned)
	}
	return pinned, raw, nil
}

func (s *session) writePinned(ctx context.Context, roomID string, pinned []string, rest map[string]json.RawMessage) error {
	if pinned == nil {
		pinned = []string{}
	}
	data, err := json.Marshal(pinned)
	if err != nil {
		return err
	}
	rest["pinned"] = data
	_, err = s.client.PutState(ctx, roomID, PinnedEventsType, "", rest)
	return err
}

// pin adds (on) or removes (off) eventID in the pinned list. It reports
// whether the list changed; an unchanged list is not rewritten.
func (s *session) pin(ctx context.Context, roomID, eventID string, on bool) (bool, error) {
	pinned, rest, err := s.readPinned(ctx, roomID)
	if err != nil {
		return false, err
	}
	var next []string
	found := false
	for _, id := range pinned {
		if id == eventID {
			found = true
			if !on {
				continue
			}
		}
		next = append(next, id)
	}
	if on == found {
		return false, nil
	}
	if on {
		next = append(next, eventID)
	}
	return true, s.writePinned(ctx, roomID, next, rest)
}

// cmdPin: a2a pin <room> <$event_id>
func cmdPin(ctx context.Context, a *App, args []string) error { return pinCommand(ctx, a, args, true) }

// cmdUnpin: a2a unpin <room> <$event_id>
func cmdUnpin(ctx context.Context, a *App, args []string) error {
	return pinCommand(ctx, a, args, false)
}

func pinCommand(ctx context.Context, a *App, args []string, on bool) error {
	verb := map[bool]string{true: "pin", false: "unpin"}[on]
	if len(args) != 2 || !strings.HasPrefix(args[1], "$") {
		return usagef("usage: a2a %s <room> <$event_id>", verb)
	}
	eventID := args[1]
	s, err := a.session()
	if err != nil {
		return err
	}
	roomID, label, err := s.resolveRoom(ctx, args[0])
	if err != nil {
		return err
	}
	if on {
		if _, err := s.client.GetEvent(ctx, roomID, eventID); err != nil {
			if isNotFound(err) {
				return fmt.Errorf("event %s not found in %s", sanitizeLine(eventID), label)
			}
			return err
		}
	}
	changed, err := s.pin(ctx, roomID, eventID, on)
	if err != nil {
		return fmt.Errorf("%s in %s: %w", verb, label, err)
	}
	if !changed {
		state := "already pinned"
		if !on {
			state = "not pinned"
		}
		fmt.Fprintf(a.Stderr, "a2a: note: %s is %s in %s\n", sanitizeLine(eventID), state, label)
	}
	a.printf("%s\n", sanitizeLine(eventID))
	return nil
}
