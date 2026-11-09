package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// operationFault is an internal test hook. In production it is nil. It can stop
// a command at a durable boundary without any live service or account.
func (a *App) operationStep(stage string) error {
	if a.postOperationFault != nil {
		return a.postOperationFault(stage)
	}
	return nil
}

// postIntent records supplied semantic choices, not generated time/defaults.
// Only its digest is persisted separately from the frozen framed request.
type postIntent struct {
	ID, Room, Root, Text, Kind, To, ReplyBy, Trunk, Subtype, Subject string
	ExplicitReplyBy                                                  bool
}

func (i postIntent) digest() string {
	data, _ := json.Marshal(i)
	return config.PostOperationDigest(data)
}

type postOperationContext struct {
	paths     config.Paths
	name      string
	binding   config.PostBinding
	protected string
}

// operationContext reads only the selected own named metadata. It does not
// obtain a provider or make an HTTP request. No legacy metadata fallback.
func (a *App) operationContext() (postOperationContext, error) {
	var out postOperationContext
	platform := runtime.GOOS
	if a.postOperationOS != "" {
		platform = a.postOperationOS
	}
	if platform != "darwin" && platform != "linux" {
		return out, usagef("durable post requires macOS or Linux; no ordinary-post fallback")
	}
	name, err := a.identity()
	if err != nil {
		return out, errors.New("post operation refused: own_identity_required")
	}
	selected, err := a.selectedConnection()
	if err != nil || selected == nil {
		return out, errors.New("post operation refused: own_named_binding_required")
	}
	p, err := a.pathsFor(name)
	if err != nil {
		return out, errors.New("post operation refused: binding_unavailable")
	}
	creds, err := a.namedMetadata(p, name)
	if err != nil || creds.ConnectionBinding == nil || creds.DeviceID == "" || creds.UserID == "" || creds.Homeserver == "" {
		return out, errors.New("post operation refused: own_metadata_unavailable")
	}
	out = postOperationContext{paths: p, name: name, binding: config.PostBinding{
		Fingerprint: creds.ConnectionBinding.Fingerprint(), UserID: creds.UserID, DeviceID: creds.DeviceID, Homeserver: creds.Homeserver,
	}}
	if selected.AdminRegistration != nil {
		out.protected = selected.AdminRegistration.RoomID
	}
	return out, nil
}

func (a *App) recheckOperation(want postOperationContext) error {
	registry, e := config.LoadConnections(config.Paths{Base: want.paths.ConfigRoot()})
	if e != nil || registry == nil {
		return errors.New("post operation refused: binding_changed; retained evidence unchanged")
	}
	fresh, ok := registry.Profiles[a.profile]
	binding, e := fresh.Binding(want.name, a.human)
	if !ok || e != nil || binding.Fingerprint() != want.binding.Fingerprint || a.connection == nil || !sameAdminPolicy(fresh.AdminRegistration, a.connection.AdminRegistration) {
		return errors.New("post operation refused: binding_changed; retained evidence unchanged")
	}
	got, err := a.operationContext()
	if err != nil || got.binding != want.binding || got.paths != want.paths || got.protected != want.protected {
		return errors.New("post operation refused: binding_changed; retained evidence unchanged")
	}
	return nil
}

// postOperationResult is deliberately closed: no request, path, raw response,
// binding fingerprint or credential-store reference can enter this projection.
type postOperationResult struct {
	OperationID string `json:"operation_id"`
	Outcome     string `json:"outcome"`
	EventID     string `json:"event_id,omitempty"`
	RoomID      string `json:"room_id"`
	RootID      string `json:"root_id"`
	Reason      string `json:"reason"`
}

func (a *App) emitPostOperation(r config.PostOperation, reason string) error {
	result := postOperationResult{OperationID: r.ID, Outcome: r.Outcome, RoomID: r.RoomID, RootID: r.RootID, Reason: reason}
	if r.Outcome == "acknowledged" {
		result.EventID = r.EventID
	}
	data, err := json.Marshal(result)
	if err != nil || a.secrets.Scrub(string(data)) != string(data) {
		return errors.New("post operation output failed; do not repost")
	}
	if err = writeCLIOutput(a.Stdout, string(data)+"\n"); err != nil {
		return errors.New("post operation output failed; use local status; do not repost")
	}
	if r.Outcome == "unknown" {
		return errors.New("post operation remains unknown; do not repost")
	}
	return nil
}

// A posting invocation succeeds only for durable acknowledgement. In
// particular, a retained prepared record is evidence, never a send grant.
func (a *App) emitPostInvocation(r config.PostOperation, reason string) error {
	if err := a.emitPostOperation(r, reason); err != nil {
		return err
	}
	if r.Outcome != "acknowledged" {
		return errors.New("post operation not acknowledged; do not repost")
	}
	return nil
}

func cmdPostOperation(_ context.Context, a *App, args []string) error {
	fs := newFlagSet("post-operation status")
	asJSON := fs.Bool("json", false, "print local retained evidence only")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 || pos[0] != "status" || !*asJSON || !config.ValidPostOperationID(pos[1]) || a.secrets.Scrub(pos[1]) != pos[1] {
		return usagef("usage: a2a post-operation status <operation-id> --json")
	}
	own, err := a.operationContext()
	if err != nil {
		return err
	}
	journal, err := config.OpenPostJournal(own.paths, own.name, a.human, false)
	if err != nil {
		return errors.New("post operation status refused: local_evidence_unavailable; absence does not prove non-application")
	}
	defer journal.Close()
	r, ok, err := journal.Read(pos[1])
	if err != nil || !ok {
		return errors.New("post operation status refused: local_evidence_unavailable; absence does not prove non-application")
	}
	if r.Binding != own.binding || r.RoomID == own.protected || a.recheckOperation(own) != nil {
		return errors.New("post operation status refused: binding_unavailable; retained evidence unchanged")
	}
	return a.emitPostOperation(r, "retained_local_evidence")
}

func (a *App) durablePost(ctx context.Context, intent postIntent, h Header) error {
	if a.secrets.Scrub(intent.ID) != intent.ID {
		return usagef("durable post requires a nonsecret operation ID")
	}
	own, err := a.operationContext()
	if err != nil {
		return err
	}
	if intent.Room == own.protected {
		return usagef("durable post refuses protected/admin rooms")
	}
	if err := a.recheckOperation(own); err != nil {
		return err
	}
	journal, err := config.OpenPostJournal(own.paths, own.name, a.human, true)
	if err != nil {
		return errors.New("post operation refused: journal_unavailable; no send")
	}
	defer journal.Close()
	retained, exists, err := journal.Read(intent.ID)
	if err != nil {
		return errors.New("post operation refused: invalid_local_evidence; no send")
	}
	digest := intent.digest()
	if exists {
		if !config.MatchesPostOperation(retained, own.binding, intent.Room, intent.Root, digest) {
			return errors.New("post operation refused: immutable_intent_changed; retained evidence unchanged")
		}
		if err := a.recheckOperation(own); err != nil {
			return err
		}
		return a.emitPostInvocation(retained, "retained_local_evidence")
	}
	if err := a.operationStep("before_prepare"); err != nil {
		return err
	}
	// Only a fresh operation can obtain its existing session and validate the
	// exact root remotely. Retained operations never resolve or regenerate it.
	s, err := a.session()
	if err != nil {
		return errors.New("post operation refused: session_unavailable; no send")
	}
	if a.secrets.Scrub(intent.ID) != intent.ID {
		return usagef("durable post requires a nonsecret operation ID")
	}
	if s.creds.DeviceID != own.binding.DeviceID || s.creds.UserID != own.binding.UserID || s.creds.Homeserver != own.binding.Homeserver {
		return errors.New("post operation refused: binding_changed; no send")
	}
	s.client = s.client.WithoutRedirects()
	names, mentions, err := s.messageAddressees(intent.To)
	if err != nil {
		return errors.New("post operation refused: invalid_addressees; no send")
	}
	root, slug, err := s.resolveTopic(ctx, intent.Room, intent.Room, intent.Root)
	if err != nil || root != intent.Root {
		return errors.New("post operation refused: exact_root_unavailable; no send")
	}
	h.From, h.To, h.Topic, h.Slug = own.name, names, root, slug
	content, err := framedTextContent(formatHeaderBody(h, intent.Text), threadReply(h, mentions), formatHeaderBody(h, ""), intent.Text, "")
	if err != nil {
		return errors.New("post operation refused: content_unavailable; no send")
	}
	request, err := json.Marshal(content)
	if err != nil || len(request) > config.PostRequestLimit {
		return errors.New("post operation refused: framed_request_too_large; 64 KiB limit; no send")
	}
	if a.secrets.Scrub(string(request)) != string(request) {
		return errors.New("post operation refused: secret_content; no send")
	}
	if err := a.recheckOperation(own); err != nil {
		return err
	}
	candidate := config.PostOperation{Schema: 1, ID: intent.ID, Binding: own.binding, RoomID: intent.Room, RootID: root, EventType: "m.room.message",
		TransactionID: matrix.NewTxnID(), IntentDigest: digest, ContentDigest: config.PostOperationDigest(request), RequestLength: len(request), Request: request, Outcome: "not_submitted"}
	record, created, err := journal.Prepare(candidate)
	if err != nil {
		return errors.New("post operation refused: prepare_failed; no send")
	}
	if !created {
		return a.emitPostInvocation(record, "retained_local_evidence")
	}
	if err := a.operationStep("after_prepare"); err != nil {
		return err
	}
	if err := a.recheckOperation(own); err != nil {
		return err
	}
	// The mutation-control test moves this reservation past I/O to prove the
	// independent ordering oracle detects the missing pre-I/O durability.
	reserve := func() error { var e error; record, e = journal.ReserveAttempt(record); return e }
	if !a.postOperationLateReserve {
		if err := reserve(); err != nil {
			return errors.New("post operation refused: reservation_failed; no send")
		}
	}
	if err := a.operationStep("after_reservation"); err != nil {
		return err
	}
	if err := a.recheckOperation(own); err != nil {
		return err
	}
	receipt, sendErr := s.client.SendMessageTxnReceipt(ctx, record.RoomID, record.TransactionID, json.RawMessage(record.Request))
	if a.postOperationLateReserve {
		if err := reserve(); err != nil {
			return errors.New("post operation reservation failed; do not repost")
		}
	}
	if sendErr != nil {
		return a.emitPostInvocation(record, "mutation_or_ack_unconfirmed")
	}
	if err := a.operationStep("after_ack_observed"); err != nil {
		return a.unknownPostACK(record, "ack_persist_failed")
	}
	// Retain strict ACK evidence before rechecking mutable connection metadata.
	if err := a.operationStep("before_ack_persist"); err != nil {
		return a.unknownPostACK(record, "ack_persist_failed")
	}
	acknowledged, err := journal.RecordACK(record, receipt.EventID(), receipt.Status(), receipt.ContentLength())
	if err != nil {
		return a.unknownPostACK(record, "ack_persist_failed")
	}
	record = acknowledged
	if err := a.operationStep("after_ack_persist"); err != nil {
		return err
	}
	if err := a.recheckOperation(own); err != nil {
		return a.unknownPostACK(record, "binding_changed_after_send")
	}
	// Follow-state work cannot erase or precede durable ACK evidence.
	if err := s.rememberThread(root); err != nil {
		fmt.Fprintln(a.Stderr, "a2a: durable ACK retained; local follow-state update failed; do not repost")
	}
	return a.emitPostInvocation(record, "original_endpoint_ack_retained")
}

func (a *App) unknownPostACK(reserved config.PostOperation, reason string) error {
	// The observed event ID is deliberately not an argument: only a durable
	// acknowledged record may expose it. A failed replacement can have renamed
	// already; this invocation still reports uncertainty, never success.
	reserved.Outcome, reserved.EventID = "unknown", ""
	return a.emitPostInvocation(reserved, reason)
}

func durablePostInvocation(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--operation-id" || arg == "-operation-id" || strings.HasPrefix(arg, "--operation-id=") || strings.HasPrefix(arg, "-operation-id=") {
			return true
		}
	}
	return false
}
