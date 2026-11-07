package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

// MediaContentObject is a local, bounded parser for native media only. It must
// never replace the permissive timeline decoder or discard a valid neighbour.
func MediaContentObject(raw []byte) (map[string]any, error) {
	if len(raw) > VerificationContentLimit || !utf8.Valid(raw) {
		return nil, errWireJSON
	}
	p := wireParser{b: raw, depthLimit: 32}
	v, err := p.value(0)
	p.space()
	m, ok := v.(map[string]any)
	if err != nil || p.i != len(raw) || !ok {
		return nil, errWireJSON
	}
	return m, nil
}

var (
	ErrOpaqueRedacted = errors.New("matrix: selected media event redacted")
	ErrOpaqueRedirect = errors.New("matrix: opaque media redirect refused")
	ErrOpaqueEncoding = errors.New("matrix: opaque media encoding refused")
	ErrOpaqueTransfer = errors.New("matrix: opaque media transfer failed")
)

// GetFetchEvent bounds the initial dispatch lookup without strict-decoding
// legacy attachment values. Native consumers must still GetMediaEvent before
// transfer; the ordinary bulk/timeline and GetEvent APIs are unchanged.
func (c *Client) GetFetchEvent(ctx context.Context, roomID, eventID string) (Event, error) {
	var zero Event
	ctx, cancel := context.WithTimeout(ctx, DefaultRequestTimeout)
	defer cancel()
	resp, err := c.send(ctx, http.MethodGet, v3+"/rooms/"+seg(roomID)+"/event/"+seg(eventID), nil, nil, "", -1)
	if err != nil {
		return zero, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, VerificationEntityLimit+1))
	closeErr := resp.Body.Close()
	if readErr != nil || closeErr != nil || ctx.Err() != nil || len(raw) > VerificationEntityLimit {
		return zero, ErrOpaqueTransfer
	}
	var ev Event
	if json.Unmarshal(raw, &ev) != nil {
		return zero, ErrOpaqueTransfer
	}
	if c.FilterEvents != nil {
		events, err := c.FilterEvents(roomID, []Event{ev})
		if err != nil {
			return zero, err
		}
		if len(events) != 1 {
			return zero, ErrOpaqueTransfer
		}
		ev = events[0]
	}
	return ev, nil
}

// MediaConfigOpaque applies the existing upload-limit semantics with a
// bounded original-endpoint control entity; legacy MediaConfig is unchanged.
func (c *Client) MediaConfigOpaque(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, DefaultRequestTimeout)
	defer cancel()
	resp, err := c.WithoutRedirects().send(ctx, http.MethodGet, v1+"/media/config", nil, nil, "", -1)
	if err != nil {
		return 0, ErrOpaqueTransfer
	}
	v, err := readControl(ctx, resp, maxErrorBody+1)
	if err != nil {
		return 0, ErrOpaqueTransfer
	}
	var out struct {
		Size int64 `json:"m.upload.size"`
	}
	if err = v.consume(ctx, &out); err != nil {
		return 0, ErrOpaqueTransfer
	}
	for key := range v.value {
		if key != "m.upload.size" && strings.EqualFold(key, "m.upload.size") {
			return 0, ErrOpaqueTransfer
		}
	}
	return out.Size, nil
}

// DownloadOpaque uses only the selected homeserver's authenticated endpoint.
// The MXC server is a path argument, never a dial destination. Policies are
// scoped copies: legacy Download and other users of HTTP remain unchanged.
func (c *Client) DownloadOpaque(ctx context.Context, mxc string) (io.ReadCloser, error) {
	m, err := ParseMXC(mxc)
	if err != nil {
		return nil, ErrOpaqueTransfer
	}
	ctx, cancel := context.WithTimeout(ctx, MediaRequestTimeout)
	fail := func(err error) (io.ReadCloser, error) { cancel(); return nil, err }
	if c.BeforeRequest != nil {
		if err := c.BeforeRequest(); err != nil {
			return fail(ErrOpaqueTransfer)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+v1+"/media/download/"+seg(m.Server)+"/"+seg(m.MediaID), nil)
	if err != nil {
		return fail(ErrOpaqueTransfer)
	}
	req.Header.Set("Accept-Encoding", "identity")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	originalURL := req.URL.String()
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	scoped := *hc
	scoped.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := scoped.Do(req)
	if err != nil {
		return fail(ErrOpaqueTransfer)
	}
	reject := func(err error) (io.ReadCloser, error) { resp.Body.Close(); return fail(err) }
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return reject(ErrOpaqueRedirect)
	}
	if resp.StatusCode != http.StatusOK || resp.Request == nil || resp.Request.URL == nil || resp.Request.URL.String() != originalURL || resp.Request.Method != http.MethodGet {
		return reject(ErrOpaqueTransfer)
	}
	if resp.Uncompressed {
		return reject(ErrOpaqueEncoding)
	}
	for key, values := range resp.Header {
		if !strings.EqualFold(key, "Content-Encoding") {
			continue
		}
		for _, encoding := range values {
			if encoding != "" && !strings.EqualFold(strings.TrimSpace(encoding), "identity") {
				return reject(ErrOpaqueEncoding)
			}
		}
	}
	// No draining of rejected bodies; callers bound the success reader itself.
	return &cancelBody{ReadCloser: resp.Body, cancel: cancel}, nil
}

// GetMediaEvent re-observes the exact native selection before media I/O.
// Ordinary GetEvent/attachment decoding retain their compatibility behavior.
func (c *Client) GetMediaEvent(ctx context.Context, roomID, eventID string) (Event, error) {
	var zero Event
	if !ValidVerificationID(roomID, '!') || !ValidVerificationID(eventID, '$') {
		return zero, ErrOpaqueTransfer
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultRequestTimeout)
	defer cancel()
	resp, err := c.WithoutRedirects().send(ctx, http.MethodGet, v3+"/rooms/"+seg(roomID)+"/event/"+seg(eventID), nil, nil, "", -1)
	if err != nil {
		return zero, ErrOpaqueTransfer
	}
	v, err := readControl(ctx, resp, VerificationEntityLimit+1)
	if err != nil || verificationKeys(v.value) != nil {
		return zero, ErrOpaqueTransfer
	}
	id, idOK := v.value["event_id"].(string)
	sender, senderOK := v.value["sender"].(string)
	typ, typeOK := v.value["type"].(string)
	if !idOK || id != eventID || !senderOK || !ValidVerificationID(sender, '@') || !typeOK {
		return zero, ErrOpaqueTransfer
	}
	if room, present := v.value["room_id"]; present && room != roomID {
		return zero, ErrOpaqueTransfer
	}
	if _, ok := v.value["content"].(map[string]any); !ok {
		return zero, ErrOpaqueTransfer
	}
	if unsigned, present := v.value["unsigned"]; present {
		fields, ok := unsigned.(map[string]any)
		if !ok {
			return zero, ErrOpaqueTransfer
		}
		for key := range fields {
			if strings.EqualFold(key, "redacted_because") {
				return zero, ErrOpaqueRedacted
			}
		}
	}
	// The strict tree above decides semantics. RawMessage only captures spans.
	var spans map[string]json.RawMessage
	if json.Unmarshal(v.raw, &spans) != nil || len(spans["content"]) > VerificationContentLimit {
		return zero, ErrOpaqueTransfer
	}
	ev := Event{EventID: id, Sender: sender, Type: typ, RoomID: roomID, Content: spans["content"], Unsigned: spans["unsigned"]}
	if c.FilterEvents != nil {
		events, err := c.FilterEvents(roomID, []Event{ev})
		if err != nil || len(events) != 1 {
			return zero, ErrOpaqueTransfer
		}
		ev = events[0]
	}
	if ctx.Err() != nil {
		return zero, ErrOpaqueTransfer
	}
	return ev, nil
}
