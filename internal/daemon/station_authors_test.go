package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const stationFixtureKey = "bip340:Ux_mBoE0UD0nIxMyJ8hnrI-myDxTfppEw8W9vcsf4zc"

// Claim: strict host snapshots reject duplicate UIDs, JSON aliases, unknown fields and invalid keys beside a valid map.
func TestStationAuthorsSchema(t *testing.T) {
	row := fmt.Sprintf(`{"uid":123,"author_key":%q}`, stationFixtureKey)
	good := `{"version":"a2a-station-authors/1","authors":[` + row + `]}`
	control := func(t *testing.T) {
		authors, err := ParseStationAuthors([]byte(good))
		if err != nil || len(authors) != 1 || authors[123] != stationFixtureKey {
			t.Fatal("valid host map unavailable")
		}
	}
	cases := map[string]string{
		"duplicate-uid":    strings.Replace(good, row, row+","+row, 1),
		"duplicate-member": strings.Replace(good, `"uid":123`, `"uid":123,"uid":123`, 1),
		"case-member":      strings.Replace(good, `"uid":123`, `"UID":123`, 1),
		"case-extra":       strings.Replace(good, `"uid":123`, `"uid":123,"UID":123`, 1),
		"unknown":          strings.Replace(good, `"uid":123`, `"uid":123,"extra":0`, 1),
		"root-extra":       strings.Replace(good, `"version":`, `"extra":0,"version":`, 1),
		"key":              strings.Replace(good, stationFixtureKey, "bip340:bad", 1),
		"key-profile":      strings.Replace(good, "bip340:", "ed25519:", 1),
		"version":          strings.Replace(good, "authors/1", "authors/2", 1),
		"negative-uid":     strings.Replace(good, "123", "-1", 1),
		"uid-overflow":     strings.Replace(good, "123", "4294967296", 1),
		"fractional-uid":   strings.Replace(good, "123", "123.0", 1),
		"trailing":         good + `{}`,
		"too-large":        strings.Repeat(" ", maxStationAuthorsBytes+1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			control(t)
			if authors, err := ParseStationAuthors([]byte(raw)); err != ErrStationAuthors || authors != nil {
				t.Fatal("malformed host map available")
			}
			control(t)
		})
	}
}

// Claim: only an absent selector disables policy; malformed, aliased and symlink selectors are configured failures.
func TestStationAuthorsSelector(t *testing.T) {
	home := t.TempDir()
	name := filepath.Join(home, "station-authors.json")
	if _, configured, err := StationAuthorsPath(home); configured || err != nil {
		t.Fatal("absent selector not disabled")
	}
	good := `{"authors_path":"/etc/a2a/station-authors.json"}`
	for _, bad := range []string{`{}`, `{"Authors_path":"/etc/map"}`, `{"authors_path":"relative"}`, `{"authors_path":"/etc/../map"}`, `{"authors_path":"/etc/map","unknown":0}`, `{"authors_path":"/etc/map","authors_path":"/etc/map"}`, strings.Repeat(" ", 4097)} {
		if err := os.WriteFile(name, []byte(good), 0600); err != nil {
			t.Fatal(err)
		}
		if path, configured, err := StationAuthorsPath(home); path != "/etc/a2a/station-authors.json" || !configured || err != nil {
			t.Fatal("valid selector unavailable")
		}
		if err := os.WriteFile(name, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, configured, err := StationAuthorsPath(home); !configured || err != ErrStationAuthors {
			t.Fatal("bad selector disabled policy")
		}
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("absent", name); err != nil {
		t.Fatal(err)
	}
	if _, configured, err := StationAuthorsPath(home); !configured || err != ErrStationAuthors {
		t.Fatal("symlink selector disabled policy")
	}
}
