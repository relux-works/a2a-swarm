// Package inboundmedia handles explicit, opaque plaintext Matrix media.
// Metadata is untrusted peer data. No function grants sender authority,
// transcribes speech, decodes a codec, decompresses bytes or records receipts.
package inboundmedia

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

type Code string

const (
	Encrypted         Code = "MEDIA_ENCRYPTED"
	MetadataInvalid   Code = "MEDIA_METADATA_INVALID"
	Unsupported       Code = "MEDIA_UNSUPPORTED"
	InvalidMXC        Code = "MEDIA_MXC_INVALID"
	ContextInvalid    Code = "MEDIA_CONTEXT_INVALID"
	Redacted          Code = "MEDIA_REDACTED"
	LimitUnavailable  Code = "MEDIA_LIMIT_UNAVAILABLE"
	TooLarge          Code = "MEDIA_TOO_LARGE"
	SizeMismatch      Code = "MEDIA_SIZE_MISMATCH"
	TransferFailed    Code = "MEDIA_TRANSFER_FAILED"
	Timeout           Code = "MEDIA_TIMEOUT"
	Cancelled         Code = "MEDIA_CANCELLED"
	EncodingRefused   Code = "MEDIA_HTTP_ENCODING"
	RedirectRefused   Code = "MEDIA_REDIRECT"
	OutputRequired    Code = "MEDIA_OUTPUT_REQUIRED"
	UnsafeOutput      Code = "MEDIA_OUTPUT_UNSAFE"
	OutputExists      Code = "MEDIA_OUTPUT_EXISTS"
	PublicationFailed Code = "MEDIA_PUBLICATION_UNSUPPORTED"
)

type Handle struct {
	RoomID  string `json:"room_id"`
	EventID string `json:"event_id"`
}

// Escape format controls without changing opaque identifiers. Encoding/json
// already escapes line separators and C0 controls, but leaves bidi Cf runes.
func escapeFormatJSON(raw []byte) []byte {
	var b strings.Builder
	for _, r := range string(raw) {
		if unicode.Is(unicode.Cf, r) {
			for _, u := range utf16.Encode([]rune{r}) {
				fmt.Fprintf(&b, "\\u%04x", u)
			}
		} else {
			b.WriteRune(r)
		}
	}
	return []byte(b.String())
}
func (h Handle) MarshalJSON() ([]byte, error) {
	type plain Handle
	raw, err := json.Marshal(plain(h))
	return escapeFormatJSON(raw), err
}

type Refusal struct {
	Code         Code
	Handle       Handle
	outputReason outputReason
}

// Output diagnostics are closed labels, never filesystem error strings or paths.
type outputReason string

const (
	outputOutside outputReason = "output_outside_tmpdir"
	tmpdirInvalid outputReason = "tmpdir_unset_or_relative"
	outputExists  outputReason = "output_exists"
	parentMissing outputReason = "parent_missing"
	symlinkInPath outputReason = "symlink_in_path"
)

func (r *Refusal) Error() string {
	// Escaping preserves the full selection without frame/terminal injection.
	b, _ := json.Marshal(r.Handle)
	message := "media: " + string(r.Code) + " handle=" + string(b)
	if r.Code == UnsafeOutput {
		reason := r.outputReason
		switch reason {
		case outputOutside, tmpdirInvalid, outputExists, parentMissing, symlinkInPath:
		default:
			reason = parentMissing
		}
		message += " subreason=" + string(reason) + "\nhint: use -o \"$TMPDIR/<name>\" beneath an existing absolute TMPDIR with nonsymlink parents and a fresh leaf"
	}
	return message
}
func refuse(code Code, handle Handle) error { return &Refusal{Code: code, Handle: handle} }

func refuseOutput(code Code, reason outputReason, handle Handle) error {
	if code == OutputExists {
		code, reason = UnsafeOutput, outputExists
	}
	return &Refusal{Code: code, Handle: handle, outputReason: reason}
}

type Metadata struct {
	Integrity    string `json:"integrity"`
	Kind         string `json:"kind"`
	Handle       Handle `json:"handle"`
	SenderMXID   string `json:"sender_mxid"`
	Name         string `json:"declared_name,omitempty"`
	NameSource   string `json:"name_source"`
	NameStatus   string `json:"name_status"`
	MIMEType     string `json:"declared_mimetype,omitempty"`
	MIMEStatus   string `json:"mimetype_status"`
	DeclaredSize *int64 `json:"declared_size"`
	Refusal      Code   `json:"refusal,omitempty"`
	// Locator is validated input, not a URL to dial or an output path.
	Locator string `json:"-"`
}

func (m Metadata) MarshalJSON() ([]byte, error) {
	type plain Metadata
	raw, err := json.Marshal(plain(m))
	return escapeFormatJSON(raw), err
}

type Result struct {
	Handle       Handle `json:"handle"`
	DeclaredSize *int64 `json:"declared_size"`
	ActualSize   int64  `json:"actual_size"`
	SHA256       string `json:"sha256"`
	HashStatus   string `json:"hash_status"`
	Path         string `json:"path"`
}

func nativeKind(s string) bool {
	switch s {
	case "m.image", "m.file", "m.audio", "m.video":
		return true
	}
	return false
}

// Dispatch has the initial entity's 1 MiB bound, independently of the strict
// 64 KiB consumed-metadata bound. An oversized preceding value must not hide
// a native kind and send a refused native item to legacy attachment decoding.
// This probe never supplies values to the actual fetch parser.
func mediaHint(raw []byte) string {
	var hint string
	probeObject(raw, func(key string, value json.RawMessage) bool {
		if strings.EqualFold(key, "msgtype") {
			var kind string
			if json.Unmarshal(value, &kind) == nil && nativeKind(kind) {
				hint = kind
				return true
			}
		}
		return false
	})
	return hint
}

// probeObject is only a bounded presence/dispatch probe. It visits every key,
// including repeated and escaped spellings, rather than selecting a map value.
// This cannot authorize native transfer: native values come from the single
// strict 64 KiB tree below. The 1 MiB bound matches initial event dispatch and
// preserves legacy attachments with larger advisory bodies.
func probeObject(raw []byte, visit func(string, json.RawMessage) bool) bool {
	if len(raw) > matrix.VerificationEntityLimit {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return false
		}
		name, ok := key.(string)
		if !ok {
			return false
		}
		var rawValue json.RawMessage
		if d.Decode(&rawValue) != nil {
			return false
		}
		if visit(name, rawValue) {
			return true
		}
	}
	return false
}

// HasLegacyAttachment preserves the historical decoder's dispatch, including
// malformed/null descriptors. Such events must never fall back to native URL.
func HasLegacyAttachment(ev matrix.Event) bool {
	return probeObject(ev.Content, func(key string, raw json.RawMessage) bool {
		return strings.EqualFold(key, "ai.relux.a2a") && probeObject(raw, func(key string, _ json.RawMessage) bool { return strings.EqualFold(key, "attachment") })
	})
}
func encryptedContent(ev matrix.Event) bool {
	if ev.Type == "m.room.encrypted" {
		return true
	}
	if ev.Type != "m.room.message" {
		return false
	}
	return probeObject(ev.Content, func(key string, raw json.RawMessage) bool {
		for _, indicator := range []string{"file", "key", "iv", "hashes"} {
			if strings.EqualFold(key, indicator) {
				return true
			}
		}
		if strings.EqualFold(key, "info") {
			return probeObject(raw, func(key string, _ json.RawMessage) bool { return strings.EqualFold(key, "thumbnail_file") })
		}
		if strings.EqualFold(key, "org.matrix.msc1767.file") || strings.EqualFold(key, "m.file") {
			return probeObject(raw, func(key string, _ json.RawMessage) bool {
				for _, indicator := range []string{"file", "key", "iv", "hashes", "encrypted"} {
					if strings.EqualFold(key, indicator) {
						return true
					}
				}
				return false
			})
		}
		return false
	})
}

// CheckEncryption also guards legacy attachment dispatch; presence, including
// null/malformed encrypted fields, is sufficient. No decryption is attempted.
func CheckEncryption(ev matrix.Event, handle Handle) error {
	if encryptedContent(ev) {
		return refuse(Encrypted, handle)
	}
	return nil
}

// Inspect is projection only: it never performs I/O or changes delivery gates.
// Nil leaves the nonmedia and legacy EventRecord wire shape unchanged.
func Inspect(ev matrix.Event, roomID string) *Metadata {
	if ev.IsRedacted() {
		return nil
	}
	h := Handle{RoomID: roomID, EventID: ev.EventID}
	m := &Metadata{Integrity: "homeserver_only_no_trusted_hash", Handle: h, SenderMXID: ev.Sender, NameSource: "unknown", NameStatus: "unknown", MIMEStatus: "unknown"}
	// Invalid envelope fields cannot amplify the bounded new metadata line.
	// The historical outer EventRecord still retains the original event fields.
	if !matrix.ValidVerificationID(roomID, '!') {
		m.Handle.RoomID = ""
	}
	if !matrix.ValidVerificationID(ev.EventID, '$') {
		m.Handle.EventID = ""
	}
	if !matrix.ValidVerificationID(ev.Sender, '@') {
		m.SenderMXID = ""
	}
	if (ev.Type == "m.room.encrypted" || mediaHint(ev.Content) != "") && CheckEncryption(ev, h) != nil {
		m.Refusal = Encrypted
		return m
	}
	if ev.Type != "m.room.message" || HasLegacyAttachment(ev) {
		return nil
	}
	// No hinted value is consumed by fetch; every native field below comes
	// from the strict tree.
	hint := mediaHint(ev.Content)
	if hint == "" {
		return nil
	}
	m.Kind = hint
	fail := func(code Code) *Metadata { m.Refusal = code; return m }
	tree, err := matrix.MediaContentObject(ev.Content)
	if err != nil || mediaKeys(tree) != nil {
		return fail(MetadataInvalid)
	}
	kind, ok := tree["msgtype"].(string)
	if !ok || !nativeKind(kind) {
		return fail(MetadataInvalid)
	}
	m.Kind = kind
	if ev.StateKey != nil || !matrix.ValidVerificationID(roomID, '!') || !matrix.ValidVerificationID(ev.EventID, '$') || !matrix.ValidVerificationID(ev.Sender, '@') || (ev.RoomID != "" && ev.RoomID != roomID) {
		return fail(ContextInvalid)
	}
	if header, present := tree["ai.relux.a2a"]; present {
		obj, ok := header.(map[string]any)
		if !ok || mediaKeys(obj) != nil {
			return fail(MetadataInvalid)
		}
		// A duplicate/alias must not make the compatibility probe authorize fallback.
		if _, present := obj["attachment"]; present {
			return fail(MetadataInvalid)
		}
	}
	// Advisory text is bounded and labelled. It never chooses an output name.
	filename, filenameOK := tree["filename"].(string)
	body, bodyOK := tree["body"].(string)
	if filenameOK && filename != "" {
		m.Name = display(filename)
		m.NameSource = "filename"
		m.NameStatus = "declared_untrusted"
	} else if bodyOK && body != "" {
		m.Name = display(body)
		m.NameSource = "body_fallback"
		m.NameStatus = "declared_untrusted"
	}
	if _, present := tree["filename"]; present && !filenameOK {
		m.Name, m.NameSource = "", "unknown"
		m.NameStatus = "invalid"
	}
	if info, present := tree["info"]; present {
		obj, ok := info.(map[string]any)
		if !ok || mediaKeys(obj) != nil {
			return fail(MetadataInvalid)
		}
		if raw, present := obj["size"]; present {
			number, ok := raw.(json.Number)
			if !ok {
				return fail(MetadataInvalid)
			}
			n, err := number.Int64()
			if err != nil || n <= 0 {
				return fail(MetadataInvalid)
			}
			m.DeclaredSize = &n
		}
		if raw, present := obj["mimetype"]; present {
			mime, ok := raw.(string)
			if ok && mime != "" {
				m.MIMEType = display(mime)
				m.MIMEStatus = "declared_untrusted"
			} else {
				m.MIMEStatus = "invalid"
			}
		}
	}
	locator, ok := tree["url"].(string)
	if !ok || locator == "" {
		return fail(Unsupported)
	}
	if _, err := matrix.ParseMXC(locator); err != nil {
		return fail(InvalidMXC)
	}
	m.Locator = locator
	if ext, present := tree["org.matrix.msc1767.file"]; present {
		obj, ok := ext.(map[string]any)
		if !ok || mediaKeys(obj) != nil {
			return fail(MetadataInvalid)
		}
		if raw, present := obj["name"]; present {
			name, ok := raw.(string)
			canonical := filename
			if !filenameOK || filename == "" {
				canonical = body
			}
			if !ok || name == "" || name != canonical || m.NameStatus == "invalid" {
				return fail(MetadataInvalid)
			}
		}
		for key, canonical := range map[string]any{"url": tree["url"], "size": nil, "mimetype": nil} {
			if info, ok := tree["info"].(map[string]any); ok && key != "url" {
				canonical = info[key]
			}
			if redundant, present := obj[key]; present {
				if redundant == nil || canonical == nil {
					return fail(MetadataInvalid)
				}
				switch v := redundant.(type) {
				case string:
					if key == "size" || v != canonical {
						return fail(MetadataInvalid)
					}
				case json.Number:
					if key != "size" || v != canonical {
						return fail(MetadataInvalid)
					}
				default:
					return fail(MetadataInvalid)
				}
			}
		}
	}
	return m
}

// Fold aliases of dispatch/security/advisory keys refuse; benign extensions
// remain opaque. The strict parser separately rejects exact/escaped duplicates.
func mediaKeys(m map[string]any) error {
	for k := range m {
		for _, canonical := range []string{"msgtype", "url", "file", "info", "size", "mimetype", "filename", "name", "body", "ai.relux.a2a", "attachment", "thumbnail_file", "key", "iv", "hashes", "encrypted", "org.matrix.msc1767.file", "m.file"} {
			if k != canonical && strings.EqualFold(k, canonical) {
				return errors.New("ambiguous media metadata")
			}
		}
	}
	return nil
}
func display(s string) string {
	var b strings.Builder
	count := 0
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
			r = ' '
		}
		if count == 256 {
			b.WriteString("...")
			break
		}
		b.WriteRune(r)
		count++
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

const MaxLimit int64 = 1 << 40 // existing attachment limit, unchanged

// Fetch validates and consumes one native metadata projection. The caller
// supplies an original-endpoint observation (CLI uses GetMediaEvent); this
// consistency check is not an owner-authentication or relevance verdict.
func Fetch(ctx context.Context, c *matrix.Client, ev matrix.Event, handle Handle, output string) (Result, error) {
	result := Result{Handle: handle}
	failure := func(code Code) (Result, error) { return result, refuse(code, handle) }
	if ev.StateKey != nil || ev.EventID != handle.EventID || !matrix.ValidVerificationID(handle.EventID, '$') || !matrix.ValidVerificationID(handle.RoomID, '!') || !matrix.ValidVerificationID(ev.Sender, '@') || (ev.RoomID != "" && ev.RoomID != handle.RoomID) {
		return failure(ContextInvalid)
	}
	if ev.IsRedacted() {
		return failure(Redacted)
	}
	m := Inspect(ev, handle.RoomID)
	if m == nil {
		return failure(Unsupported)
	}
	if m.Refusal != "" {
		return failure(m.Refusal)
	}
	if output == "" {
		return failure(OutputRequired)
	}
	if ctx.Err() != nil {
		return failure(transferCode(ctx, ctx.Err()))
	}
	target, code, reason := openTarget(output)
	if code != "" {
		return result, refuseOutput(code, reason, handle)
	}
	defer target.cleanup()
	configCtx, cancel := context.WithTimeout(ctx, matrix.DefaultRequestTimeout)
	limit, err := c.MediaConfigOpaque(configCtx)
	// Observe before our cleanup cancel: that cancel is not a caller refusal.
	configErr := configCtx.Err()
	cancel()
	if configErr != nil {
		if errors.Is(configErr, context.DeadlineExceeded) {
			return failure(Timeout)
		}
		return failure(Cancelled)
	}
	if err != nil || limit <= 0 {
		return failure(LimitUnavailable)
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	if m.DeclaredSize != nil && *m.DeclaredSize > limit {
		return failure(TooLarge)
	}
	readLimit := limit
	if m.DeclaredSize != nil {
		readLimit = *m.DeclaredSize
	}
	transferCtx, transferCancel := context.WithTimeout(ctx, matrix.MediaRequestTimeout)
	defer transferCancel()
	body, err := c.DownloadOpaque(transferCtx, m.Locator)
	if err != nil {
		return failure(transferCode(transferCtx, err))
	}
	sum := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(target.file, sum), io.LimitReader(body, readLimit+1))
	closeErr := body.Close()
	if copyErr != nil || closeErr != nil || transferCtx.Err() != nil {
		return failure(transferCode(transferCtx, copyErr))
	}
	// Keep this actual-size guard distinct from metadata and equality checks.
	// It is exercised by the 65/64 causal plant with unknown declared size.
	if n > limit {
		return failure(TooLarge)
	}
	if n == 0 || (m.DeclaredSize != nil && n != *m.DeclaredSize) {
		return failure(SizeMismatch)
	}
	if err := target.file.Close(); err != nil {
		return failure(TransferFailed)
	}
	if c.BeforeRequest != nil {
		if err := c.BeforeRequest(); err != nil {
			return failure(ContextInvalid)
		}
	}
	if ctx.Err() != nil {
		return failure(transferCode(ctx, ctx.Err()))
	}
	if code := target.publish(); code != "" {
		return result, refuseOutput(code, parentMissing, handle)
	}
	result.DeclaredSize = m.DeclaredSize
	result.ActualSize = n
	result.SHA256 = hex.EncodeToString(sum.Sum(nil))
	result.HashStatus = "computed_unverified"
	result.Path = output
	return result, nil
}
func transferCode(ctx context.Context, err error) Code {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return Timeout
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return Cancelled
	}
	if errors.Is(err, matrix.ErrOpaqueEncoding) {
		return EncodingRefused
	}
	if errors.Is(err, matrix.ErrOpaqueRedirect) {
		return RedirectRefused
	}
	return TransferFailed
}

// FormatResult describes bounds and the absence of a trusted expected hash.
func FormatResult(r Result) string {
	declared := "unknown"
	if r.DeclaredSize != nil {
		declared = fmt.Sprint(*r.DeclaredSize)
	}
	return fmt.Sprintf("media: actual_size=%d declared_size=%s sha256=%s hash_status=computed_unverified", r.ActualSize, declared, r.SHA256)
}
