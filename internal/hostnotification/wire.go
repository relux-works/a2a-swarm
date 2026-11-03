// Package hostnotification implements the opted-in private Host stream. Wire
// metadata is observation, never source enrollment, mailbox receipt or authority.
package hostnotification

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/relux-works/a2a-swarm/internal/config"
)

const Contract = "curator-host-notification/1"
const MaxFrame = 65536

var ErrProtocol = errors.New("host notification protocol refused")
var ErrUnresolved = errors.New("host notification exchange unresolved")
var ErrBusy = errors.New("host notification exchange already in flight")

type Request struct {
	Contract  string                         `json:"contract"`
	RequestID string                         `json:"request_id"`
	Method    string                         `json:"method"`
	Binding   config.HostNotificationBinding `json:"binding"`
	NoticeID  string                         `json:"notice_id,omitempty"`
}
type Response struct {
	Contract  string   `json:"contract"`
	RequestID string   `json:"request_id"`
	Method    string   `json:"method"`
	Result    string   `json:"result"`
	Reason    string   `json:"reason"`
	View      *View    `json:"view,omitempty"`
	Attempt   *Attempt `json:"attempt,omitempty"`
}
type View struct {
	Binding               config.HostNotificationBinding `json:"binding"`
	Adapter               string                         `json:"adapter"`
	DeclaredNativeRelease string                         `json:"declared_native_release"`
	NoticeMode            string                         `json:"notice_mode"`
	Gate                  string                         `json:"gate"`
	RecordsRemaining      int                            `json:"records_remaining"`
}
type Attempt struct {
	NoticeID          string `json:"notice_id"`
	WorkflowReference string `json:"workflow_reference"`
	WorkflowSHA256    string `json:"workflow_sha256"`
	Sequence          string `json:"sequence"`
	Custody           string `json:"custody"`
	NativeStage       string `json:"native_stage"`
}

func shortRef(s string) bool {
	if len(s) == 0 || len(s) > 128 || !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return false
		}
	}
	return true
}
func noticeID(s string) bool {
	if len(s) != 36 || !strings.HasPrefix(s, "a2a-") {
		return false
	}
	for _, c := range s[4:] {
		if !(c >= 'a' && c <= 'f' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func oneOf(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}
func (r Request) valid() bool {
	return r.Contract == Contract && shortRef(r.RequestID) && r.Binding.Validate() == nil &&
		(r.Method == "observe_binding" && r.NoticeID == "" || oneOf(r.Method, "trigger", "lookup_attempt") && noticeID(r.NoticeID))
}
func EncodeRequest(r Request) ([]byte, error) {
	if !r.valid() {
		return nil, ErrProtocol
	}
	data, err := json.Marshal(r)
	if err != nil || len(data)+1 > MaxFrame {
		return nil, ErrProtocol
	}
	return append(data, '\n'), nil
}

func exact(value any, keys ...string) bool {
	m, ok := value.(map[string]any)
	if !ok || len(m) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := m[key]; !ok {
			return false
		}
	}
	return true
}
func boundedJSON(value any, depth int) bool {
	if depth > 6 || value == nil {
		return false
	}
	switch v := value.(type) {
	case map[string]any:
		for _, child := range v {
			if !boundedJSON(child, depth+1) {
				return false
			}
		}
	case []any:
		for _, child := range v {
			if !boundedJSON(child, depth+1) {
				return false
			}
		}
	}
	return true
}
func bindingShape(value any) bool {
	return exact(value, "execution", "controller_epoch", "runner_instance", "native_thread", "member_ref", "connection_ref", "configuration_ref", "profile_lock_ref", "consumer_generation", "enrollment_ref", "workflow_reference", "workflow_sha256")
}

// DecodeResponse binds every successful payload to the exact original request.
// A refusal is only this call's refusal, never proof about an earlier call.
func DecodeResponse(frame []byte, request Request) (Response, error) {
	var r Response
	if !request.valid() || len(frame) < 3 || len(frame) > MaxFrame || frame[len(frame)-1] != '\n' || frame[len(frame)-2] != '}' || !utf8.Valid(frame) || bytes.Contains(frame[:len(frame)-1], []byte{'\n'}) {
		return r, ErrProtocol
	}
	data := frame[:len(frame)-1]
	if config.DecodeConnectionJSON(data, &r) != nil {
		return Response{}, ErrProtocol
	}
	var tree map[string]any
	if json.Unmarshal(data, &tree) != nil || !boundedJSON(tree, 1) {
		return Response{}, ErrProtocol
	}
	keys := []string{"contract", "request_id", "method", "result", "reason"}
	if r.View != nil {
		keys = append(keys, "view")
	}
	if r.Attempt != nil {
		keys = append(keys, "attempt")
	}
	if !exact(tree, keys...) || r.Contract != Contract || r.RequestID != request.RequestID || r.Method != request.Method {
		return Response{}, ErrProtocol
	}
	valid := false
	switch r.Result {
	case "refused":
		valid = r.View == nil && r.Attempt == nil && oneOf(r.Reason, "unsupported-contract", "attachment-unavailable", "enrollment-unverified", "binding-conflict", "stale-binding", "input-not-ready", "external-held", "security-stopped", "capacity-full", "invalid-attempt", "workflow-conflict")
	case "unresolved":
		valid = r.View == nil && r.Attempt == nil && oneOf(r.Reason, "attempt-unresolved", "observation-unavailable")
	case "observed":
		v := r.View
		valid = request.Method == "observe_binding" && r.Reason == "" && v != nil && r.Attempt == nil && exact(tree["view"], "binding", "adapter", "declared_native_release", "notice_mode", "gate", "records_remaining")
		if valid {
			m := tree["view"].(map[string]any)
			valid = bindingShape(m["binding"]) && v.Binding == request.Binding && v.Adapter == "codex" && v.DeclaredNativeRelease == "0.164.0" && v.NoticeMode == "work-reference" && v.RecordsRemaining >= 0 && v.RecordsRemaining <= 32 && oneOf(v.Gate, "open", "input-not-ready", "external-held", "security-stopped", "capacity-full", "observation-unavailable")
		}
	case "accepted", "existing-attempt":
		a := r.Attempt
		valid = r.Reason == "" && r.View == nil && a != nil && (r.Result == "accepted" && request.Method == "trigger" || r.Result == "existing-attempt" && oneOf(request.Method, "trigger", "lookup_attempt")) && exact(tree["attempt"], "notice_id", "workflow_reference", "workflow_sha256", "sequence", "custody", "native_stage")
		if valid {
			sequence, err := strconv.ParseUint(a.Sequence, 10, 64)
			valid = err == nil && sequence > 0 && strconv.FormatUint(sequence, 10) == a.Sequence && a.NoticeID == request.NoticeID && a.WorkflowReference == request.Binding.WorkflowReference && a.WorkflowSHA256 == request.Binding.WorkflowSHA256 && a.Custody == "host-accepted" && oneOf(a.NativeStage, "not-evidenced", "write-unresolved", "native-accepted", "native-completed")
		}
	}
	if !valid {
		return Response{}, ErrProtocol
	}
	return r, nil
}
