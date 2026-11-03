package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func notificationAttemptKey() NotificationAttemptKey {
	return NotificationAttemptKey{
		Token: strings.Repeat("a", 32), Session: "source-session", Generation: "consumer-generation", NoticeSHA256: strings.Repeat("b", 64),
		Binding: HostNotificationBinding{
			Execution: "execution", ControllerEpoch: "1", RunnerInstance: "runner", NativeThread: "native-thread",
			MemberRef: "@member:example.org", ConnectionRef: "own-connection", ConfigurationRef: "own-config", ProfileLockRef: "qualified-lock",
			ConsumerGeneration: "consumer-generation", EnrollmentRef: "enrollment", WorkflowReference: "host-notification-inbox-v1", WorkflowSHA256: strings.Repeat("c", 64),
		},
	}
}

func attemptBytes(t *testing.T, l NotificationAttempts) string {
	t.Helper()
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Proves one original trigger may progress to retained Host acceptance and
// later same-sequence stages; acceptance is not a mailbox or model receipt.
func TestNotificationAttemptAcceptance(t *testing.T) {
	k := notificationAttemptKey()
	var l NotificationAttempts
	if err := l.Prepare(k); err != nil {
		t.Fatal(err)
	}
	if err := l.Begin(k, "original-request"); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"not-evidenced", "write-unresolved", "native-accepted", "native-completed"} {
		if err := l.Accept(k, "7", stage); err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		before := attemptBytes(t, l)
		if err := l.Accept(k, "7", stage); err != nil || attemptBytes(t, l) != before {
			t.Fatalf("exact observation not idempotent: %v", err)
		}
	}
	if l.Entries[0].State != "accepted" || l.Entries[0].RequestID != "original-request" {
		t.Fatal("original correlation lost")
	}
}

// Proves a restart retains UNKNOWN and neither another trigger, a new token,
// nor malformed/stale observations can bypass its fence or mutate its bytes.
func TestNotificationAttemptUnknownFence(t *testing.T) {
	k := notificationAttemptKey()
	var l NotificationAttempts
	if err := l.Prepare(k); err != nil {
		t.Fatal(err)
	}
	if err := l.Begin(k, "original-request"); err != nil {
		t.Fatal(err)
	}
	var restart NotificationAttempts
	if err := json.Unmarshal([]byte(attemptBytes(t, l)), &restart); err != nil {
		t.Fatal(err)
	}
	before := attemptBytes(t, restart)
	newKey := k
	newKey.Token = strings.Repeat("d", 32)
	for _, fn := range []func() error{
		func() error { return restart.Begin(k, "original-request") },
		func() error { return restart.Begin(k, "retry-request") },
		func() error { return restart.Prepare(newKey) },
		func() error { return restart.Abandon(k) },
		func() error { return restart.Refuse(k, "other-request", "input-not-ready") },
		func() error { return restart.Refuse(k, "original-request", "not-found") },
		func() error { return restart.Accept(k, "0", "native-accepted") },
		func() error { return restart.Accept(k, "01", "native-accepted") },
		func() error { return restart.Accept(k, "18446744073709551616", "native-accepted") },
		func() error { return restart.Accept(k, "7", "model-read") },
	} {
		if err := fn(); !errors.Is(err, ErrNotificationAttempt) {
			t.Fatalf("missing intended refusal: %v", err)
		}
		if attemptBytes(t, restart) != before {
			t.Fatal("refusal changed original UNKNOWN")
		}
	}
	if err := restart.Accept(k, "7", "not-evidenced"); err != nil {
		t.Fatalf("nearby valid reconciliation: %v", err)
	}
}

// Proves copied token or valid-looking but changed source/binding/digest cannot
// resolve another attempt, with the exact original key succeeding nearby.
func TestNotificationAttemptExactIdentity(t *testing.T) {
	k := notificationAttemptKey()
	mutations := map[string]func(*NotificationAttemptKey){
		"token":   func(k *NotificationAttemptKey) { k.Token = strings.Repeat("d", 32) },
		"session": func(k *NotificationAttemptKey) { k.Session = "other-source-session" },
		"generation": func(k *NotificationAttemptKey) {
			k.Generation = "other-generation"
			k.Binding.ConsumerGeneration = k.Generation
		},
		"notice":          func(k *NotificationAttemptKey) { k.NoticeSHA256 = strings.Repeat("d", 64) },
		"execution":       func(k *NotificationAttemptKey) { k.Binding.Execution = "other-execution" },
		"epoch":           func(k *NotificationAttemptKey) { k.Binding.ControllerEpoch = "2" },
		"runner":          func(k *NotificationAttemptKey) { k.Binding.RunnerInstance = "other-runner" },
		"native-thread":   func(k *NotificationAttemptKey) { k.Binding.NativeThread = "other-thread" },
		"member":          func(k *NotificationAttemptKey) { k.Binding.MemberRef = "@other:example.org" },
		"connection":      func(k *NotificationAttemptKey) { k.Binding.ConnectionRef = "other-connection" },
		"configuration":   func(k *NotificationAttemptKey) { k.Binding.ConfigurationRef = "other-config" },
		"profile":         func(k *NotificationAttemptKey) { k.Binding.ProfileLockRef = "other-lock" },
		"enrollment":      func(k *NotificationAttemptKey) { k.Binding.EnrollmentRef = "other-enrollment" },
		"workflow":        func(k *NotificationAttemptKey) { k.Binding.WorkflowReference = "other-workflow" },
		"workflow-digest": func(k *NotificationAttemptKey) { k.Binding.WorkflowSHA256 = strings.Repeat("d", 64) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var l NotificationAttempts
			if err := l.Prepare(k); err != nil {
				t.Fatal(err)
			}
			if err := l.Begin(k, "original-request"); err != nil {
				t.Fatal(err)
			}
			before := attemptBytes(t, l)
			bad := k
			mutate(&bad)
			if err := l.Accept(bad, "7", "native-accepted"); !errors.Is(err, ErrNotificationAttempt) || attemptBytes(t, l) != before {
				t.Fatalf("changed identity affected original: %v", err)
			}
			if err := l.Accept(k, "7", "native-accepted"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Proves only original pre-acceptance refusal or prepared-before-I/O abandonment
// establishes nonapplication; accepted observations never regress or disappear.
func TestNotificationAttemptNonapplicationAndNoRegression(t *testing.T) {
	k := notificationAttemptKey()
	for _, abandoned := range []bool{true, false} {
		var l NotificationAttempts
		if err := l.Prepare(k); err != nil {
			t.Fatal(err)
		}
		if abandoned {
			if err := l.Abandon(k); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := l.Begin(k, "original-request"); err != nil {
				t.Fatal(err)
			}
			if err := l.Refuse(k, "original-request", "capacity-full"); err != nil {
				t.Fatal(err)
			}
		}
		if l.Entries[0].State != "not-applied" {
			t.Fatal("missing nonapplication")
		}
		before := attemptBytes(t, l)
		if err := l.Accept(k, "7", "native-accepted"); !errors.Is(err, ErrNotificationAttempt) || attemptBytes(t, l) != before {
			t.Fatal("nonapplication rewritten")
		}
		if err := l.Prepare(k); !errors.Is(err, ErrNotificationAttempt) || attemptBytes(t, l) != before {
			t.Fatal("retained token reused")
		}
		k2 := k
		k2.Token = strings.Repeat("d", 32)
		if err := l.Prepare(k2); err != nil {
			t.Fatal(err)
		}
		if err := l.Begin(k2, "second-request"); err != nil {
			t.Fatal(err)
		}
		if err := l.Accept(k2, "7", "native-completed"); err != nil {
			t.Fatal(err)
		}
		before = attemptBytes(t, l)
		for _, fn := range []func() error{
			func() error { return l.Refuse(k2, "second-request", "input-not-ready") },
			func() error { return l.Accept(k2, "8", "native-completed") },
			func() error { return l.Accept(k2, "7", "native-accepted") },
		} {
			if err := fn(); !errors.Is(err, ErrNotificationAttempt) || attemptBytes(t, l) != before {
				t.Fatalf("regression: %v", err)
			}
		}
	}
}

// Proves strict bounded persisted decoding preserves the destination on every
// malformed version/key/null/trailing/size case; valid encoding survives restart.
func TestNotificationAttemptStrictDecode(t *testing.T) {
	var l NotificationAttempts
	if err := l.Prepare(notificationAttemptKey()); err != nil {
		t.Fatal(err)
	}
	valid := attemptBytes(t, l)
	for name, bad := range map[string]string{
		"null": "null", "empty": "{}", "future": strings.Replace(valid, `"version":1`, `"version":2`, 1),
		"duplicate":          strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		"nested-duplicate":   strings.Replace(valid, `"session":"source-session"`, `"session":"source-session","session":"source-session"`, 1),
		"case-alias":         strings.Replace(valid, `"version":1`, `"Version":1`, 1),
		"nested-case-alias":  strings.Replace(valid, `"token":`, `"Token":`, 1),
		"unicode-case-alias": strings.Replace(valid, `"state":`, `"ſtate":`, 1),
		"unknown":            strings.Replace(valid, `"version":1`, `"version":1,"extra":true`, 1),
		"nested-unknown":     strings.Replace(valid, `"execution":"execution"`, `"execution":"execution","extra":true`, 1),
		"field-null":         strings.Replace(valid, `"state":"prepared"`, `"state":null`, 1),
		"optional-null":      strings.Replace(valid, `"state":"prepared"`, `"state":"prepared","request_id":null`, 1),
		"trailing":           valid + "{}", "oversized": strings.Replace(valid, "source-session", strings.Repeat("s", 65536), 1),
	} {
		t.Run(name, func(t *testing.T) {
			dst := l
			if err := json.Unmarshal([]byte(bad), &dst); err == nil || attemptBytes(t, dst) != valid {
				t.Fatalf("unsafe decode: %v", err)
			}
		})
	}
	var restarted NotificationAttempts
	if err := json.Unmarshal([]byte(valid), &restarted); err != nil || attemptBytes(t, restarted) != valid {
		t.Fatalf("valid restart: %v", err)
	}
}

// Proves retained terminal entries are never evicted at the 32-record ceiling.
func TestNotificationAttemptCapacity(t *testing.T) {
	var l NotificationAttempts
	for i := 0; i < 32; i++ {
		k := notificationAttemptKey()
		k.Token = strings.Repeat("0", 30) + string("0123456789abcdef"[i/16]) + string("0123456789abcdef"[i%16])
		if err := l.Prepare(k); err != nil {
			t.Fatal(err)
		}
		if err := l.Abandon(k); err != nil {
			t.Fatal(err)
		}
	}
	before := attemptBytes(t, l)
	if err := l.Prepare(notificationAttemptKey()); !errors.Is(err, ErrNotificationAttemptCapacity) || attemptBytes(t, l) != before {
		t.Fatalf("capacity changed retained state: %v", err)
	}
}

// Proves a request identity or retained Host sequence cannot close a second
// token; a fresh request and Host sequence remain nearby positive controls.
func TestNotificationAttemptCorrelationReuse(t *testing.T) {
	k := notificationAttemptKey()
	var l NotificationAttempts
	if err := l.Prepare(k); err != nil {
		t.Fatal(err)
	}
	if err := l.Begin(k, "request-1"); err != nil {
		t.Fatal(err)
	}
	if err := l.Accept(k, "1", "not-evidenced"); err != nil {
		t.Fatal(err)
	}
	k2 := k
	k2.Token = strings.Repeat("d", 32)
	if err := l.Prepare(k2); err != nil {
		t.Fatal(err)
	}
	before := attemptBytes(t, l)
	if err := l.Begin(k2, "request-1"); !errors.Is(err, ErrNotificationAttempt) || attemptBytes(t, l) != before {
		t.Fatalf("request reused: %v", err)
	}
	if err := l.Begin(k2, "request-2"); err != nil {
		t.Fatal(err)
	}
	before = attemptBytes(t, l)
	if err := l.Accept(k2, "1", "native-accepted"); !errors.Is(err, ErrNotificationAttempt) || attemptBytes(t, l) != before {
		t.Fatalf("Host sequence reused: %v", err)
	}
	if err := l.Accept(k2, "2", "native-accepted"); err != nil {
		t.Fatal(err)
	}
}

// Proves byte capacity can refuse before the count limit, preserving all prior
// terminal entries, with individually valid maximum-size opaque references.
func TestNotificationAttemptByteCapacity(t *testing.T) {
	var l NotificationAttempts
	for i := 0; i < 32; i++ {
		k := notificationAttemptKey()
		k.Token = strings.Repeat("0", 30) + string("0123456789abcdef"[i/16]) + string("0123456789abcdef"[i%16])
		k.Binding.MemberRef = strings.Repeat("m", 512)
		k.Binding.ConfigurationRef = strings.Repeat("c", 512)
		k.Binding.ProfileLockRef = strings.Repeat("p", 512)
		k.Binding.EnrollmentRef = strings.Repeat("e", 512)
		before := attemptBytes(t, l)
		err := l.Prepare(k)
		if errors.Is(err, ErrNotificationAttemptCapacity) {
			if attemptBytes(t, l) != before || i == 0 {
				t.Fatal("capacity mutated state or lacks valid control")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Abandon(k); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("byte budget did not refuse below count limit")
}

// Proves separate source/native bounds and canonical identities reject without
// truncation, with maximum legal source session/thread/u64 accepted nearby.
func TestNotificationAttemptBounds(t *testing.T) {
	for name, mutate := range map[string]func(*NotificationAttemptKey){
		"uppercase-token":    func(k *NotificationAttemptKey) { k.Token = strings.Repeat("A", 32) },
		"uppercase-digest":   func(k *NotificationAttemptKey) { k.NoticeSHA256 = strings.Repeat("B", 64) },
		"long-session":       func(k *NotificationAttemptKey) { k.Session = strings.Repeat("s", 201) },
		"control-session":    func(k *NotificationAttemptKey) { k.Session = "source\u0080" },
		"invalid-utf8":       func(k *NotificationAttemptKey) { k.Session = string([]byte{255}) },
		"generation":         func(k *NotificationAttemptKey) { k.Generation = "other" },
		"long-thread":        func(k *NotificationAttemptKey) { k.Binding.NativeThread = strings.Repeat("t", 129) },
		"unicode-thread":     func(k *NotificationAttemptKey) { k.Binding.NativeThread = "é" },
		"empty-member":       func(k *NotificationAttemptKey) { k.Binding.MemberRef = "" },
		"long-connection":    func(k *NotificationAttemptKey) { k.Binding.ConnectionRef = strings.Repeat("c", 129) },
		"long-configuration": func(k *NotificationAttemptKey) { k.Binding.ConfigurationRef = strings.Repeat("c", 513) },
		"long-profile":       func(k *NotificationAttemptKey) { k.Binding.ProfileLockRef = strings.Repeat("p", 513) },
		"long-enrollment":    func(k *NotificationAttemptKey) { k.Binding.EnrollmentRef = strings.Repeat("e", 513) },
		"workflow-control":   func(k *NotificationAttemptKey) { k.Binding.WorkflowReference = "workflow\n" },
		"workflow-digest":    func(k *NotificationAttemptKey) { k.Binding.WorkflowSHA256 = strings.Repeat("g", 64) },
		"zero-epoch":         func(k *NotificationAttemptKey) { k.Binding.ControllerEpoch = "0" },
		"leading-zero-epoch": func(k *NotificationAttemptKey) { k.Binding.ControllerEpoch = "01" },
		"overflow-epoch":     func(k *NotificationAttemptKey) { k.Binding.ControllerEpoch = "18446744073709551616" },
	} {
		t.Run(name, func(t *testing.T) {
			var l NotificationAttempts
			before := attemptBytes(t, l)
			bad := notificationAttemptKey()
			mutate(&bad)
			if err := l.Prepare(bad); !errors.Is(err, ErrNotificationAttempt) || attemptBytes(t, l) != before {
				t.Fatalf("unsafe bound: %v", err)
			}
		})
	}
	k := notificationAttemptKey()
	k.Session = strings.Repeat("s", 200)
	k.Binding.NativeThread = strings.Repeat("t", 128)
	k.Binding.ControllerEpoch = "18446744073709551615"
	var valid NotificationAttempts
	if err := valid.Prepare(k); err != nil {
		t.Fatalf("maximum valid fields: %v", err)
	}
}
