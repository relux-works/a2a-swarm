package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

var hostEnrollmentAliases = []struct {
	name string
	key  string // JSON key contents, including any JSON escape.
}{
	{"uppercase", "HOST_ENROLLMENT"},
	{"mixed-case", "HoSt_EnRoLlMeNt"},
	{"long-s", "hoſt_enrollment"},
	{"escaped-long-s", `ho\u017ft_enrollment`},
}

// Exercise the public load and persistence routes: null under any recognized
// spelling refuses before a callback or write, regardless of alias ordering.
func TestHostEnrollmentAliasNullRefuses(t *testing.T) {
	_, attachment, _ := hostFixture()
	raw, err := json.Marshal(HostEnrollment{Version: 1, Attachment: attachment})
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range hostEnrollmentAliases {
		t.Run(alias.name, func(t *testing.T) {
			valid := `"host_enrollment":` + string(raw)
			null := `"` + alias.key + `": null `
			aliasValid := `"` + alias.key + `":` + string(raw)
			for _, tc := range []struct{ name, members string }{
				{"null-only", null},
				{"canonical-valid-alias-null", valid + "," + null},
				{"alias-null-canonical-valid", null + "," + valid},
				{"canonical-null-alias-valid", `"host_enrollment":null,` + aliasValid},
				{"alias-valid-canonical-null", aliasValid + `,"host_enrollment":null`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					for _, route := range []string{"load", "save", "update"} {
						t.Run(route, func(t *testing.T) {
							p := testPaths(t)
							payload := `{"next_batch":"pending","future":{"large":9007199254740993},` + tc.members + `}`
							seedStateJSON(t, p, payload)
							var err error
							called := false
							switch route {
							case "load":
								_, err = LoadState(p, "a")
							case "save":
								err = SaveState(p, "a", State{NextBatch: "overwritten"})
							case "update":
								_, err = UpdateState(p, "a", func(s *State) error {
									called = true
									s.NextBatch = "overwritten"
									return nil
								})
							}
							if !errors.Is(err, ErrHostEnrollment) {
								t.Errorf("alias null not refused by %s: %v", route, err)
							}
							if called {
								t.Error("alias null reached update callback")
							}
							after, err := os.ReadFile(p.StateFile("a"))
							if err != nil || !bytes.Equal(after, []byte(payload)) {
								t.Errorf("alias null refusal changed disk: %v", err)
							}
						})
					}
				})
			}
		})
	}
}

// Establish actual JSON alias applicability and keep non-null typed enrollment,
// opaque neighbors, and the genuine prior-enrollment persistence fence intact.
func TestHostEnrollmentAliasValidNeighbors(t *testing.T) {
	_, attachment, _ := hostFixture()
	enrollment := HostEnrollment{Version: 1, Attachment: attachment}
	raw, err := json.Marshal(enrollment)
	if err != nil {
		t.Fatal(err)
	}
	keys := append([]struct{ name, key string }{{"canonical", "host_enrollment"}}, hostEnrollmentAliases...)
	for _, key := range keys {
		t.Run(key.name, func(t *testing.T) {
			p := testPaths(t)
			const future = `{"large":9007199254740993,"parts":[null,true]}`
			seedStateJSON(t, p, `{"`+key.key+`":`+string(raw)+`,"next_batch":"before","future":`+future+`,"host_enrollment_extra":null}`)
			s, err := LoadState(p, "a")
			if err != nil || s.HostEnrollment == nil || !s.HostEnrollment.same(enrollment) {
				t.Fatalf("non-null enrollment alias not decoded: %v", err)
			}
			if _, err := UpdateState(p, "a", func(s *State) error { s.NextBatch = "after"; return nil }); err != nil {
				t.Fatal(err)
			}
			got := readStateJSON(t, p)
			if string(got["next_batch"]) != `"after"` || !sameStateJSON(got["future"], []byte(future)) || string(got["host_enrollment_extra"]) != "null" || !sameStateJSON(got["host_enrollment"], raw) {
				t.Fatal("valid update lost typed or opaque neighbors")
			}
			before, err := os.ReadFile(p.StateFile("a"))
			if err != nil {
				t.Fatal(err)
			}
			for _, mutation := range []string{"drop", "replace"} {
				bad := s
				if mutation == "drop" {
					bad.HostEnrollment = nil
				} else {
					e := *s.HostEnrollment
					e.Attachment.Reference = "replacement"
					bad.HostEnrollment = &e
				}
				if err := SaveState(p, "a", bad); !errors.Is(err, ErrHostEnrollment) {
					t.Errorf("prior enrollment %s not refused: %v", mutation, err)
				}
				after, err := os.ReadFile(p.StateFile("a"))
				if err != nil || !bytes.Equal(before, after) {
					t.Errorf("prior enrollment refusal changed disk: %v", err)
				}
			}
		})
	}
}

func TestHostEnrollmentKelvinFoldNotApplicable(t *testing.T) {
	// Kelvin sign folds with K/k. Neither occurs in this JSON field name, so
	// inventing a Kelvin spelling would not exercise encoding/json's aliases.
	for _, r := range "host_enrollment" {
		if strings.EqualFold(string(r), "K") {
			t.Fatalf("Kelvin fold unexpectedly applicable to %q", r)
		}
	}
}
