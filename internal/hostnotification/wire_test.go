package hostnotification

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func fixtureRequest(method string) Request {
	b := config.HostNotificationBinding{Execution: "execution", ControllerEpoch: "1", RunnerInstance: "runner", NativeThread: "thread", MemberRef: "member", ConnectionRef: "connection", ConfigurationRef: "config", ProfileLockRef: "lock", ConsumerGeneration: "generation", EnrollmentRef: "source-receipt", WorkflowReference: "host-notification-inbox-v1", WorkflowSHA256: strings.Repeat("a", 64)}
	r := Request{Contract: Contract, RequestID: "request", Method: method, Binding: b}
	if method != "observe_binding" {
		r.NoticeID = "a2a-" + strings.Repeat("b", 32)
	}
	return r
}
func acceptedResponse(r Request) Response {
	return Response{Contract: Contract, RequestID: r.RequestID, Method: r.Method, Result: "accepted", Attempt: &Attempt{NoticeID: r.NoticeID, WorkflowReference: r.Binding.WorkflowReference, WorkflowSHA256: r.Binding.WorkflowSHA256, Sequence: "1", Custody: "host-accepted", NativeStage: "not-evidenced"}}
}
func wireFrame(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// Proves every documented success/refusal/unresolved sum decodes beside the
// wrong method/result combinations; acceptance means only its recorded stage.
func TestResponseSums(t *testing.T) {
	for _, method := range []string{"observe_binding", "trigger", "lookup_attempt"} {
		r := fixtureRequest(method)
		for _, result := range []string{"observed", "accepted", "existing-attempt", "refused", "unresolved"} {
			response := acceptedResponse(r)
			response.Result = result
			switch result {
			case "observed":
				response.Attempt = nil
				response.View = &View{Binding: r.Binding, Adapter: "codex", DeclaredNativeRelease: "0.164.0", NoticeMode: "work-reference", Gate: "open", RecordsRemaining: 0}
			case "refused":
				response.Attempt = nil
				response.Reason = "input-not-ready"
			case "unresolved":
				response.Attempt = nil
				response.Reason = "attempt-unresolved"
			}
			want := result == "refused" || result == "unresolved" || result == "observed" && method == "observe_binding" || result == "accepted" && method == "trigger" || result == "existing-attempt" && method != "observe_binding"
			_, err := DecodeResponse(wireFrame(t, response), r)
			if (err == nil) != want {
				t.Fatalf("%s/%s: %v", method, result, err)
			}
		}
	}
}

// Proves exact original request/workflow/notice/sequence/stage correlation is
// required; every rejection returns only a fixed error and empty response.
func TestResponseCorrelation(t *testing.T) {
	r := fixtureRequest("trigger")
	good := acceptedResponse(r)
	for _, mutate := range []func(*Response){
		func(x *Response) { x.RequestID = "foreign" }, func(x *Response) { x.Method = "lookup_attempt" }, func(x *Response) { x.Contract = "future" },
		func(x *Response) { x.Attempt.NoticeID = "a2a-" + strings.Repeat("c", 32) }, func(x *Response) { x.Attempt.WorkflowReference = "foreign" }, func(x *Response) { x.Attempt.WorkflowSHA256 = strings.Repeat("c", 64) },
		func(x *Response) { x.Attempt.Sequence = "01" }, func(x *Response) { x.Attempt.Sequence = "18446744073709551616" }, func(x *Response) { x.Attempt.Custody = "model-handled" }, func(x *Response) { x.Attempt.NativeStage = "model-read" }, func(x *Response) { x.Reason = "payload-secret" },
	} {
		bad := good
		a := *good.Attempt
		bad.Attempt = &a
		mutate(&bad)
		got, err := DecodeResponse(wireFrame(t, bad), r)
		if !errors.Is(err, ErrProtocol) || got.Contract != "" {
			t.Fatalf("changed correlation accepted: %+v %v", got, err)
		}
	}
	if _, err := DecodeResponse(wireFrame(t, good), r); err != nil {
		t.Fatal(err)
	}
}

// Proves closed framing/schema reject ambiguous or malformed bytes, beside a
// valid frame. These controls do not exercise a native runtime or mailbox.
func TestResponseFraming(t *testing.T) {
	r := fixtureRequest("trigger")
	good := wireFrame(t, acceptedResponse(r))
	for _, bad := range [][]byte{
		good[:len(good)-1], append([]byte{0xef, 0xbb, 0xbf}, good...), append([]byte{0xff}, good...), append(append([]byte{}, good[:len(good)-1]...), []byte(" \n")...),
		bytes.Replace(good, []byte(`"reason":""`), []byte(`"reason":null`), 1), bytes.Replace(good, []byte(`"request_id":`), []byte(`"request_id":"duplicate","request_id":`), 1),
		bytes.Replace(good, []byte(`"request_id":`), []byte(`"Request_id":`), 1), bytes.Replace(good, []byte(`"reason":""`), []byte(`"future":{},"reason":""`), 1),
		bytes.Replace(good, []byte(`"reason":""`), []byte(`"reason":"","future":[[[[[[[0]]]]]]]`), 1),
		append(append([]byte{}, good[:len(good)-1]...), []byte("{}\n")...), append(bytes.Repeat([]byte(" "), MaxFrame), good...),
	} {
		if bytes.Equal(good, bad) {
			t.Fatal("malformed plant not applied")
		}
		if _, err := DecodeResponse(bad, r); !errors.Is(err, ErrProtocol) {
			t.Fatalf("malformed frame accepted: %v", err)
		}
	}
	if _, err := DecodeResponse(good, r); err != nil {
		t.Fatal(err)
	}
}

// Proves view zero-capacity must be explicitly present, and every binding and
// declaration remains exact; advisory open is not permission for a later write.
func TestObservedBinding(t *testing.T) {
	r := fixtureRequest("observe_binding")
	v := View{Binding: r.Binding, Adapter: "codex", DeclaredNativeRelease: "0.164.0", NoticeMode: "work-reference", Gate: "open", RecordsRemaining: 0}
	response := Response{Contract: Contract, RequestID: r.RequestID, Method: r.Method, Result: "observed", View: &v}
	good := wireFrame(t, response)
	for _, pair := range [][2]string{{`"records_remaining":0`, `"records_remaining":33`}, {`,"records_remaining":0`, ``}, {`"execution":"execution"`, `"execution":"foreign"`}, {`"adapter":"codex"`, `"adapter":"future"`}, {`"gate":"open"`, `"gate":"unknown"`}} {
		bad := bytes.Replace(good, []byte(pair[0]), []byte(pair[1]), 1)
		if bytes.Equal(good, bad) {
			t.Fatal("view plant not applied")
		}
		if _, err := DecodeResponse(bad, r); err == nil {
			t.Fatal("invalid view accepted")
		}
	}
	if _, err := DecodeResponse(good, r); err != nil {
		t.Fatal(err)
	}
}

// Proves invalid local request primitives are refused before bytes are made;
// valid boundary-sized opaque IDs and the closed methods encode normally.
func TestRequestBounds(t *testing.T) {
	r := fixtureRequest("trigger")
	for _, mutate := range []func(*Request){
		func(x *Request) { x.RequestID = "" }, func(x *Request) { x.RequestID = "bad\nvalue" }, func(x *Request) { x.RequestID = strings.Repeat("x", 129) },
		func(x *Request) { x.NoticeID = "a2a-" + strings.Repeat("B", 32) }, func(x *Request) { x.Binding.ControllerEpoch = "0" }, func(x *Request) { x.Method = "new-method" },
	} {
		bad := r
		mutate(&bad)
		if b, err := EncodeRequest(bad); !errors.Is(err, ErrProtocol) || b != nil {
			t.Fatalf("bad request produced bytes: %v", err)
		}
	}
	r.RequestID = strings.Repeat("x", 128)
	if _, err := EncodeRequest(r); err != nil {
		t.Fatal(err)
	}
}
