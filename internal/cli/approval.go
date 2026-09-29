package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const (
	approvalContentKey = "ai.relux.a2a.approval"
	approvalSigKey     = "ai.relux.a2a.approval_sig"
	approvalNamespace  = "approve.request.v1@waggle"
)

var approvalPrincipalRE = regexp.MustCompile(`^op:[a-z0-9._=-]{1,64}@relux$`)

type approvalPayload struct {
	Schema     string            `json:"schema"`
	Type       string            `json:"type"`
	ID         string            `json:"id"`
	From       string            `json:"from"`
	Signers    map[string]string `json:"signers"`
	Scope      string            `json:"scope"`
	Text       string            `json:"text"`
	TextSHA256 string            `json:"text_sha256"`
	IssuedAt   int64             `json:"issued_at"`
	ExpiresAt  int64             `json:"expires_at"`
}

func canonicalApproval(p approvalPayload) ([]byte, error) {
	// Marshal through maps: encoding/json sorts string map keys recursively.
	v := map[string]any{
		"schema": p.Schema, "type": p.Type, "id": p.ID, "from": p.From,
		"signers": map[string]string{"approver": p.Signers["approver"]},
		"scope":   p.Scope, "text": p.Text, "text_sha256": p.TextSHA256,
		"issued_at": p.IssuedAt, "expires_at": p.ExpiresAt,
	}
	return json.Marshal(v)
}

func cmdApproval(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 {
		return usagef("usage: a2a approval request|sign|verify ...")
	}
	switch args[0] {
	case "request":
		return cmdApprovalRequest(ctx, a, args[1:])
	case "sign":
		return cmdApprovalSign(ctx, a, args[1:])
	case "verify":
		return cmdApprovalVerify(ctx, a, args[1:])
	default:
		return usagef("usage: a2a approval request|sign|verify ...")
	}
}

func cmdApprovalRequest(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("approval request")
	room := fs.String("room", "", "room")
	topic := fs.String("topic", "", "topic")
	approver := fs.String("approver", "", "operator principal")
	scope := fs.String("scope", "", "approval scope")
	text := fs.String("text", "", "approved text")
	expires := fs.String("expires", "", "RFC3339 time or +duration")
	if _, err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if *room == "" || *topic == "" || *scope == "" || *text == "" || *expires == "" || !approvalPrincipalRE.MatchString(*approver) {
		return usagef("usage: a2a approval request --room R --topic T --approver op:<name>@relux --scope S --text T --expires <RFC3339|+24h>")
	}
	deadline, err := parseApprovalExpiry(*expires, a.Now())
	if err != nil {
		return usagef("invalid --expires: %v", err)
	}
	if deadline.Before(a.Now()) || deadline.Equal(a.Now()) {
		return usagef("--expires must be in the future")
	}
	if strings.TrimSpace(*text) == "" || !validUTF8(*text) {
		return usagef("approval text must be non-empty UTF-8")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	roomID, label, err := s.resolveRoom(ctx, *room)
	if err != nil {
		return err
	}
	root, err := approvalTopic(ctx, s, roomID, *topic)
	if err != nil {
		return err
	}
	id, err := newApprovalID(a.Now())
	if err != nil {
		return fmt.Errorf("approval id: %w", err)
	}
	hash := sha256.Sum256([]byte(*text))
	p := approvalPayload{Schema: "waggle-v1", Type: "approval.request", ID: id, From: s.name, Signers: map[string]string{"approver": *approver}, Scope: *scope, Text: *text, TextSHA256: hex.EncodeToString(hash[:]), IssuedAt: a.Now().UnixMilli(), ExpiresAt: deadline.UnixMilli()}
	data, err := canonicalApproval(p)
	if err != nil {
		return err
	}
	path := filepath.Join(s.paths.Base, "state", "approvals", id+".json")
	if err := writeApprovalFile(path, data); err != nil {
		return fmt.Errorf("write canonical payload: %w", err)
	}
	payloadMap := map[string]any{}
	if err := json.Unmarshal(data, &payloadMap); err != nil {
		return err
	}
	content := map[string]any{"msgtype": "m.text", "body": "approval request " + id, approvalContentKey: payloadMap, "m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root}}
	_, err = s.client.SendMessage(ctx, roomID, content)
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("approval request in %s: %w", label, err)
	}
	a.printf("%s\t%s\n", id, path)
	return nil
}

func parseApprovalExpiry(raw string, now time.Time) (time.Time, error) {
	if strings.HasPrefix(raw, "+") {
		d, err := time.ParseDuration(strings.TrimPrefix(raw, "+"))
		if err != nil || d <= 0 {
			return time.Time{}, errors.New("expected a positive duration such as +24h")
		}
		return now.Add(d), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, errors.New("expected RFC3339 or +duration")
	}
	return t, nil
}

func newApprovalID(now time.Time) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	ms := uint64(now.UnixMilli())
	b[0], b[1], b[2], b[3], b[4], b[5] = byte(ms>>40), byte(ms>>32), byte(ms>>24), byte(ms>>16), byte(ms>>8), byte(ms)
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[:8], h[8:12], h[12:16], h[16:20], h[20:]), nil
}

func approvalTopic(ctx context.Context, s *session, roomID, topic string) (string, error) {
	if strings.HasPrefix(topic, "$") {
		return topic, nil
	}
	topics, err := s.listTopics(ctx, roomID)
	if err != nil {
		return "", err
	}
	for _, t := range topics {
		if t.Slug == topic {
			return t.EventID, nil
		}
	}
	return "", fmt.Errorf("topic %q not found", sanitizeLine(topic))
}

func cmdApprovalSign(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("approval sign")
	payloadPath := fs.String("payload", "", "canonical request payload")
	key := fs.String("key", "", "SSH private key")
	room := fs.String("post", "", "post decision to room")
	topic := fs.String("topic", "", "decision topic")
	if _, err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if *payloadPath == "" || *key == "" || ((*room == "") != (*topic == "")) {
		return usagef("usage: a2a approval sign --payload FILE --key PATH [--post R --topic T]")
	}
	p, _, err := readCanonicalApproval(*payloadPath)
	if err != nil {
		return fmt.Errorf("approval payload: %w", err)
	}
	sigPath := *payloadPath + ".sig"
	cmd := exec.CommandContext(ctx, "ssh-keygen", "-Y", "sign", "-f", *key, "-n", approvalNamespace, *payloadPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh-keygen sign failed: %s", sanitizeLine(string(out)))
	}
	if *room == "" {
		a.printf("%s\n", sigPath)
		return nil
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	roomID, label, err := s.resolveRoom(ctx, *room)
	if err != nil {
		return err
	}
	root, err := approvalTopic(ctx, s, roomID, *topic)
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(sigPath)
	if err != nil {
		return fmt.Errorf("read SSH signature: %w", err)
	}
	content := map[string]any{"msgtype": "m.text", "body": "approval decision " + p.ID, approvalSigKey: map[string]any{"request_id": p.ID, "principal": p.Signers["approver"], "sshsig": string(sig)}}
	content["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": root}
	if _, err := s.client.SendMessage(ctx, roomID, content); err != nil {
		return fmt.Errorf("approval decision in %s: %w", label, err)
	}
	a.printf("%s\n", p.ID)
	return nil
}

func readCanonicalApproval(path string) (approvalPayload, []byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return approvalPayload{}, nil, err
	}
	var p approvalPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return p, nil, err
	}
	canonical, err := canonicalApproval(p)
	if err != nil {
		return p, nil, err
	}
	if !equalBytes(b, canonical) {
		return p, nil, errors.New("payload is not canonical JSON")
	}
	if !validApprovalPayload(p) {
		return p, nil, errors.New("invalid approval payload")
	}
	return p, canonical, nil
}

func validApprovalPayload(p approvalPayload) bool {
	return p.Schema == "waggle-v1" && p.Type == "approval.request" && p.ID != "" && p.From != "" &&
		approvalPrincipalRE.MatchString(p.Signers["approver"]) && p.Scope != "" && p.Text != "" &&
		p.TextSHA256 != "" && p.IssuedAt > 0 && p.ExpiresAt > p.IssuedAt
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func cmdApprovalVerify(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("approval verify")
	room := fs.String("room", "", "room")
	eventID := fs.String("event", "", "decision event")
	approvers := fs.String("approvers", "", "allowed signers file")
	if _, err := parseInterspersed(fs, args); err != nil {
		return err
	}
	if *room == "" || *eventID == "" || *approvers == "" {
		return usagef("usage: a2a approval verify --room R --event $E --approvers FILE")
	}
	st, err := os.Stat(*approvers)
	if err != nil {
		return usagef("--approvers file: %v", err)
	}
	if !st.Mode().IsRegular() {
		return usagef("--approvers must be a regular file")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	roomID, _, err := s.resolveRoom(ctx, *room)
	if err != nil {
		return err
	}
	decision, err := s.client.GetEvent(ctx, roomID, *eventID)
	if err != nil {
		return fmt.Errorf("decision event not found: %w", err)
	}
	var dc struct {
		Sig struct {
			RequestID string `json:"request_id"`
			Principal string `json:"principal"`
			SSHSig    string `json:"sshsig"`
		} `json:"ai.relux.a2a.approval_sig"`
	}
	if err := json.Unmarshal(decision.Content, &dc); err != nil || dc.Sig.RequestID == "" || dc.Sig.Principal == "" {
		return errors.New("event is not an approval decision")
	}
	request, err := findApprovalRequest(ctx, s, roomID, dc.Sig.RequestID)
	if err != nil {
		return err
	}
	var requestContent map[string]json.RawMessage
	if err := json.Unmarshal(request.Content, &requestContent); err != nil {
		return errors.New("request event has no approval payload")
	}
	payloadBytes, ok := requestContent[approvalContentKey]
	if !ok {
		return errors.New("request event has no approval payload")
	}
	var p approvalPayload
	if err := json.Unmarshal(payloadBytes, &p); err != nil {
		return errors.New("invalid approval request payload")
	}
	if !validApprovalPayload(p) {
		return errors.New("invalid approval request payload")
	}
	canonical, err := canonicalApproval(p)
	if err != nil {
		return err
	}
	if localpartIfLocal(request.Sender, s.server) != p.From {
		return errors.New("request 'from' does not match the sender of its event")
	}
	if p.ID != dc.Sig.RequestID {
		return errors.New("request id mismatch")
	}
	if p.Signers["approver"] != dc.Sig.Principal {
		return errors.New("principal does not match request approver")
	}
	hash := sha256.Sum256([]byte(p.Text))
	if p.TextSHA256 != hex.EncodeToString(hash[:]) {
		return errors.New("text_sha256 mismatch")
	}
	if a.Now().UnixMilli() >= p.ExpiresAt {
		return errors.New("approval request expired")
	}
	sigFile, err := os.CreateTemp("", "a2a-approval-*.sig")
	if err != nil {
		return err
	}
	sigPath := sigFile.Name()
	defer os.Remove(sigPath)
	if err := sigFile.Chmod(0o600); err != nil {
		sigFile.Close()
		return err
	}
	if _, err := io.WriteString(sigFile, dc.Sig.SSHSig); err != nil {
		sigFile.Close()
		return err
	}
	if err := sigFile.Close(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "ssh-keygen", "-Y", "verify", "-f", *approvers, "-I", dc.Sig.Principal, "-n", approvalNamespace, "-s", sigPath)
	cmd.Stdin = strings.NewReader(string(canonical))
	out, verifyErr := cmd.CombinedOutput()
	if verifyErr != nil {
		return fmt.Errorf("SSH signature verification failed: %s", sanitizeLine(string(out)))
	}
	if err := recordApprovalUse(s.paths, s.name, p.ID); err != nil {
		return err
	}
	a.printf("VERIFIED %s scope=%s approver=%s\n", p.ID, sanitizeLine(p.Scope), sanitizeLine(dc.Sig.Principal))
	return nil
}

func findApprovalRequest(ctx context.Context, s *session, roomID, id string) (matrix.Event, error) {
	from := ""
	for pages := 0; pages < 100; pages++ {
		page, err := s.client.Messages(ctx, roomID, from, "b", 100)
		if err != nil {
			return matrix.Event{}, fmt.Errorf("find request event: %w", err)
		}
		for _, ev := range page.Chunk {
			var m map[string]json.RawMessage
			_ = json.Unmarshal(ev.Content, &m)
			var value struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(m[approvalContentKey], &value)
			if value.ID == id {
				return ev, nil
			}
		}
		if page.End == "" || page.End == from || len(page.Chunk) == 0 {
			break
		}
		from = page.End
	}
	return matrix.Event{}, errors.New("request event does not exist")
}

func recordApprovalUse(paths config.Paths, name, id string) error {
	_, err := config.UpdateState(paths, name, func(state *config.State) error {
		for _, used := range state.UsedApprovals {
			if used == id {
				return errors.New("approval request id already used")
			}
		}
		state.UsedApprovals = append(state.UsedApprovals, id)
		return nil
	})
	return err
}

func writeApprovalFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		_ = os.Remove(path)
		return err
	}
	return f.Sync()
}

func validUTF8(s string) bool { return strings.ToValidUTF8(s, "") == s }
