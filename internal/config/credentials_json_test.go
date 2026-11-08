package config

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Proves observable repair/clear updates replace opaque data and the returned
// credentials agree with reload, with untouched and nil-to-nil controls. The
// callback API cannot distinguish assignment of nil from no assignment when
// the decoded value is already nil; a nonnil empty slice expresses clearing.
func TestOpaqueWaitKeywordsUpdates(t *testing.T) {
	for _, raw := range []string{`{ "future" : 1 }`, `null`, `[ "BAD SPACE" ]`, `[ "BEFORE" ]`, `["BEFORE"],"WAIT_KEYWORDS":{ "future" : 1 }`, `{ "future" : 1 },"WAIT_KEYWORDS":["BEFORE"]`} {
		for _, action := range []string{"repair", "clear-empty", "clear-nil", "untouched"} {
			t.Run(raw+"/"+action, func(t *testing.T) {
				p := testPaths(t)
				writeCred(t, p, "alice", 0600, `{"user_id":"@alice:test.local","access_token":"synthetic-token","wait_keywords":`+raw+`}`)
				before, err := LoadCredentials(p, "alice")
				if err != nil {
					t.Fatal(err)
				}
				got, err := UpdateCredentials(p, "alice", func(c *Credentials) error {
					c.DeviceID = "next"
					switch action {
					case "repair":
						c.WaitKeywords = WaitKeywords{"REPAIRED"}
					case "clear-empty":
						c.WaitKeywords = WaitKeywords{}
					case "clear-nil":
						c.WaitKeywords = nil
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				reloaded, err := LoadCredentials(p, "alice")
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, reloaded) {
					t.Fatal("update return differs from disk reload")
				}
				data, err := os.ReadFile(p.AgentFile("alice"))
				if err != nil {
					t.Fatal(err)
				}
				retained := action == "untouched" || action == "clear-nil" && before.WaitKeywords == nil
				if retained {
					for _, value := range before.waitKeywordsRaw {
						if !bytes.Contains(data, value) {
							t.Fatal("rewrite changed an opaque occurrence")
						}
					}
					if !reflect.DeepEqual(got.WaitKeywords, before.WaitKeywords) || (got.ValidateWaitKeywords() == nil) != (before.ValidateWaitKeywords() == nil) {
						t.Fatal("unobservable/untouched update changed opaque semantics")
					}
				} else {
					if got.ValidateWaitKeywords() != nil {
						t.Fatal("updated predicate retains stale malformed raw state")
					}
					if action == "repair" {
						if !reflect.DeepEqual(got.WaitKeywords, WaitKeywords{"REPAIRED"}) || len(got.waitKeywordsRaw) != 1 {
							t.Fatal("repair did not replace old occurrences")
						}
					} else if got.WaitKeywords != nil || len(got.waitKeywordsRaw) != 0 || bytes.Contains(data, []byte(`"wait_keywords"`)) {
						t.Fatal("observable clear did not omit predicate")
					}
				}
			})
		}
	}
}

// Proves a strict-accepted credential can be rewritten and strictly reloaded;
// case-variant duplicates (including a malformed occurrence) refuse up front.
// A single valid and a single opaque value are adjacent round-trip controls.
func TestOpaqueWaitKeywordsStrictRoundTrip(t *testing.T) {
	for _, raw := range []string{`["A"]`, `{ "future" : 1 }`} {
		for _, duplicate := range []bool{true, false} {
			data := []byte(`{"user_id":"@alice:test.local","access_token":"synthetic-token","WAIT_KEYWORDS":` + raw)
			if duplicate {
				data = append(data, []byte(`,"wait_keywords":["B"]`)...)
			}
			data = append(data, '}')
			var c Credentials
			err := DecodeConnectionJSON(data, &c)
			if duplicate {
				if err == nil {
					t.Fatal("case-variant duplicate admitted before rewrite")
				}
				continue
			}
			if err != nil {
				t.Fatal("single-key strict neighbor refused")
			}
			encoded, err := marshalCredentials(c)
			if err != nil {
				t.Fatal(err)
			}
			var reloaded Credentials
			if DecodeConnectionJSON(encoded, &reloaded) != nil || !reflect.DeepEqual(c, reloaded) {
				t.Fatal("rewrite broke strict reload")
			}
		}
	}
}

// Proves all credential writers retain exact optional bytes, including unknown
// object members and malformed scalars/arrays, alongside a valid filter control.
func TestOpaqueWaitKeywordsWriters(t *testing.T) {
	for _, raw := range []string{
		`{ "future" : [9007199254740993, "<>&", {"member":null}] }`,
		`"opaque\u0020value"`, `[ "TASK-56", { "future":true } ]`, `null`, `false`, `1`, `[]`,
		`[ "TASK-56" ]`,
	} {
		for _, writer := range []string{"update", "scrub", "create", "bound"} {
			for _, value := range []string{raw, `[ "TASK-56" ]`} {
				t.Run(writer+"/"+raw+"/"+value, func(t *testing.T) {
					raw := value
					p := testPaths(t)
					writeCred(t, p, "alice", 0600, `{"user_id":"@alice:test.local","access_token":"synthetic-token","wait_keywords":`+raw+`}`)
					c, err := LoadCredentials(p, "alice")
					if err != nil {
						t.Fatal("optional data blocked credential load")
					}
					if _, err := LoadCredentialMetadata(p, "alice", false); err != nil {
						t.Fatal("optional data blocked metadata load")
					}
					var words WaitKeywords
					valid := json.Unmarshal([]byte(raw), &words) == nil
					if (c.ValidateWaitKeywords() == nil) != valid {
						t.Fatal("optional predicate validation changed")
					}
					path := p.AgentFile("alice")
					switch writer {
					case "update":
						_, err = UpdateCredentials(p, "alice", func(next *Credentials) error { next.DeviceID = "next"; return nil })
					case "scrub":
						err = RewriteCredentialMetadata(p, "alice", false, func(next *Credentials) error { next.AccessToken = ""; return nil })
					case "create":
						path = p.AgentFile("copy")
						var f *os.File
						f, err = CreateCredentialFile(p, "copy")
						if err == nil {
							err = WriteCredentials(f, c)
						}
					case "bound":
						conn := testConnection()
						conn.Store = CredentialStore{Backend: "file"}
						var b ConnectionBinding
						b, err = conn.Binding("alice", false)
						c.ConnectionBinding, c.Homeserver, c.UserID = &b, conn.URL, b.UserID
						q := testPaths(t)
						path = q.AgentFile("alice")
						if err == nil {
							err = PublishBoundCredentials(q, "alice", false, c)
						}
					}
					if err != nil {
						t.Fatal("credential writer refused optional data")
					}
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var got map[string]json.RawMessage
					if json.Unmarshal(data, &got) != nil || !bytes.Equal(got["wait_keywords"], []byte(raw)) {
						t.Fatal("writer changed optional raw bytes")
					}
				})
			}
		}
	}
}

// Proves tolerant optional decoding keeps named metadata strict at both the
// credential and binding levels, with a valid neighbor after every refusal.
func TestOpaqueWaitKeywordsNamedJSON(t *testing.T) {
	for _, extra := range []string{
		`,"unknown":"private-sentinel"`,
		`,"user_id":"@second:test.local"`,
		`,"connection_binding":{"unknown":"private-sentinel"}`,
	} {
		var c Credentials
		bad := []byte(`{"user_id":"@alice:test.local","wait_keywords":{ "future" : 1 }` + extra + `}`)
		if err := DecodeConnectionJSON(bad, &c); err == nil || strings.Contains(err.Error(), "private-sentinel") {
			t.Fatal("named metadata admitted invalid structure or leaked contents")
		}
		good := []byte(`{"user_id":"@alice:test.local","wait_keywords":{ "future" : 1 }}`)
		if err := DecodeConnectionJSON(good, &c); err != nil || c.ValidateWaitKeywords() == nil {
			t.Fatal("valid metadata control did not retain malformed optional data")
		}
	}
}

// Proves a later valid legacy occurrence cannot hide malformed configuration,
// while valid repeated keys keep their existing last-value matching semantics.
func TestOpaqueWaitKeywordsRepeatedValues(t *testing.T) {
	for _, first := range []string{`{ "future" : true }`, `[ "FIRST" ]`} {
		p := testPaths(t)
		writeCred(t, p, "alice", 0600, `{"user_id":"@alice:test.local","access_token":"synthetic-token","wait_keywords":`+first+`,"WAIT_KEYWORDS":[ "LAST" ]}`)
		c, err := UpdateCredentials(p, "alice", func(c *Credentials) error { c.DeviceID = "next"; return nil })
		if err != nil {
			t.Fatal("repeated optional fields blocked unrelated rewrite")
		}
		if len(c.WaitKeywords) != 1 || c.WaitKeywords[0] != "LAST" {
			t.Fatal("valid last-value semantics changed")
		}
		if (c.ValidateWaitKeywords() == nil) != strings.HasPrefix(first, "[") {
			t.Fatal("later occurrence concealed malformed configuration")
		}
		data, err := os.ReadFile(p.AgentFile("alice"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(data, []byte(first)) || !bytes.Contains(data, []byte(`[ "LAST" ]`)) || bytes.Count(data, []byte(`"wait_keywords"`)) != 2 {
			t.Fatal("rewrite dropped a legacy optional occurrence")
		}
	}
}

// Proves optional tolerance does not waive required fields, whole-file JSON or
// secret-file permissions; each refusal has an adjacent valid credential control.
func TestOpaqueWaitKeywordsCredentialGuards(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"token", `{"user_id":"@alice:test.local","password":"private-sentinel","wait_keywords":{}}`, "missing access_token"},
		{"identity", `{"access_token":"private-sentinel","wait_keywords":{}}`, "missing user_id"},
		{"json", `{"access_token":"private-sentinel","wait_keywords":`, "not valid credential JSON"},
		{"type", `{"user_id":7,"access_token":"private-sentinel","wait_keywords":{}}`, "not valid credential JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaths(t)
			writeCred(t, p, "alice", 0600, tc.body)
			_, err := LoadCredentials(p, "alice")
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "private-sentinel") {
				t.Fatal("required credential error changed or disclosed secret")
			}
			if tc.name != "token" {
				if _, err := LoadCredentialMetadata(p, "alice", false); err == nil || strings.Contains(err.Error(), "private-sentinel") {
					t.Fatal("metadata waived required validation")
				}
			}
			writeCred(t, p, "alice", 0600, `{"user_id":"@alice:test.local","access_token":"synthetic-token","wait_keywords":{}}`)
			if _, err := LoadCredentials(p, "alice"); err != nil {
				t.Fatal("valid required control refused")
			}
			if err := os.Chmod(p.AgentFile("alice"), 0644); err != nil {
				t.Fatal(err)
			}
			for _, metadata := range []bool{false, true} {
				if metadata {
					_, err = LoadCredentialMetadata(p, "alice", false)
				} else {
					_, err = LoadCredentials(p, "alice")
				}
				if err == nil || !strings.Contains(err.Error(), "want 0600") {
					t.Fatal("optional tolerance waived permissions")
				}
			}
		})
	}
}
