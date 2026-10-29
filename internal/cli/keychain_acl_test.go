package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// aclEntry is one access entry of an item in `security dump-keychain -a`
// output: its authorizations, and either any application (<null>) or the
// listed trusted applications.
type aclEntry struct {
	auths  []string
	anyApp bool
	apps   []aclApp
}

// aclApp is one trusted application: its path, the status in parentheses and
// its complete code requirement.
type aclApp struct {
	path, status, requirement string
}

// aclAuthorizations is the closed set of authorization tags security prints.
// An unknown tag refuses: format drift must not turn a reading entry into an
// ignored one.
var aclAuthorizations = map[string]bool{
	"any": true, "login": true, "genkey": true, "delete": true, "export_wrapped": true, "export_clear": true,
	"import_wrapped": true, "import_clear": true, "sign": true, "encrypt": true, "decrypt": true, "mac": true,
	"derive": true, "keychain_create": true, "keychain_delete": true, "keychain_read": true, "keychain_write": true,
	"db_insert": true, "db_delete": true, "db_read": true, "db_modify": true, "change_acl": true, "change_owner": true,
	"integrity": true, "partition_id": true,
}

var (
	aclAccessRE  = regexp.MustCompile(`^access: (\d+) entries$`)
	aclEntryRE   = regexp.MustCompile(`^    entry (\d+):$`)
	aclAuthRE    = regexp.MustCompile(`^        authorizations \((\d+)\): (.+)$`)
	aclAppsRE    = regexp.MustCompile(`^        applications \((\d+)\):$`)
	aclAppRE     = regexp.MustCompile(`^            (\d+): (.+) \(([^()]+)\)$`)
	aclReqRE     = regexp.MustCompile(`^                requirement: (.+)$`)
	aclOtherRE   = regexp.MustCompile(`^        (don't-require-password|require-password|description: .*)$`)
	aclAccountRE = regexp.MustCompile(`^    "acct"<blob>="(.*)"$`)
	aclServiceRE = regexp.MustCompile(`^    "svce"<blob>="(.*)"$`)
)

// itemACL returns the access entries of the generic-password item whose svce
// and acct attributes are service and account, from `security dump-keychain
// -a` output (never -d).
// Any line it does not know, a count that does not match, or a missing access
// section refuses: the check fails closed rather than skipping structure.
func itemACL(dump, service, account string) ([]aclEntry, error) {
	var found []string
	for _, block := range strings.Split(dump, "\nkeychain: ") {
		lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
		acct, svce := false, false
		for _, line := range lines {
			if m := aclAccountRE.FindStringSubmatch(line); m != nil && m[1] == account {
				acct = true
			}
			if m := aclServiceRE.FindStringSubmatch(line); m != nil && m[1] == service {
				svce = true
			}
		}
		if acct && svce {
			if found != nil {
				return nil, errors.New("item listed twice")
			}
			found = lines
		}
	}
	if found == nil {
		return nil, errors.New("item not in dump")
	}
	start := -1
	for i, line := range found {
		if line == "no access control for this item" {
			return nil, errors.New("item has no access control")
		}
		if aclAccessRE.MatchString(line) {
			start = i
		}
	}
	if start < 0 {
		return nil, errors.New("no access section")
	}
	want, _ := strconv.Atoi(aclAccessRE.FindStringSubmatch(found[start])[1])
	var entries []aclEntry
	appsLeft := 0
	for _, line := range found[start+1:] {
		switch {
		case aclEntryRE.MatchString(line):
			if appsLeft != 0 {
				return nil, errors.New("application count mismatch")
			}
			if n, _ := strconv.Atoi(aclEntryRE.FindStringSubmatch(line)[1]); n != len(entries) {
				return nil, errors.New("entry numbering mismatch")
			}
			entries = append(entries, aclEntry{})
		case len(entries) == 0:
			return nil, fmt.Errorf("line before the first entry: %q", line)
		case aclAuthRE.MatchString(line):
			m := aclAuthRE.FindStringSubmatch(line)
			auths := strings.Fields(m[2])
			if n, _ := strconv.Atoi(m[1]); n != len(auths) {
				return nil, errors.New("authorization count mismatch")
			}
			for _, a := range auths {
				if !aclAuthorizations[a] {
					return nil, fmt.Errorf("unknown authorization %q", a)
				}
			}
			entries[len(entries)-1].auths = auths
		case line == "        applications: <null>":
			entries[len(entries)-1].anyApp = true
		case aclAppsRE.MatchString(line):
			appsLeft, _ = strconv.Atoi(aclAppsRE.FindStringSubmatch(line)[1])
		case aclAppRE.MatchString(line):
			if appsLeft == 0 {
				return nil, errors.New("unannounced application")
			}
			appsLeft--
			m := aclAppRE.FindStringSubmatch(line)
			entries[len(entries)-1].apps = append(entries[len(entries)-1].apps, aclApp{path: m[2], status: m[3]})
		case aclReqRE.MatchString(line):
			apps := entries[len(entries)-1].apps
			if len(apps) == 0 || apps[len(apps)-1].requirement != "" {
				return nil, errors.New("requirement without its application")
			}
			apps[len(apps)-1].requirement = aclReqRE.FindStringSubmatch(line)[1]
		case aclOtherRE.MatchString(line):
		default:
			return nil, fmt.Errorf("unknown ACL line: %q", line)
		}
	}
	if appsLeft != 0 || len(entries) != want {
		return nil, errors.New("entry or application count mismatch")
	}
	return entries, nil
}

// creatorOnly accepts an ACL in which every entry that can release the
// plaintext (decrypt, export_clear, export_wrapped or any) trusts exactly one
// application: the creator's path, status OK, and exactly the creator's own
// code requirement. No entry may let any application do so; a missing,
// broadened or different requirement refuses.
func creatorOnly(entries []aclEntry, requirement string, creator ...string) error {
	readers := map[string]bool{}
	for _, e := range entries {
		plaintext := false
		for _, a := range e.auths {
			switch a {
			case "decrypt", "export_clear", "export_wrapped", "any":
				plaintext = true
			}
		}
		if !plaintext {
			continue
		}
		if e.anyApp {
			return errors.New("any application may release the plaintext")
		}
		for _, app := range e.apps {
			readers[app.path] = true
			if !slices.Contains(creator, app.path) {
				continue
			}
			if app.status != "OK" {
				return fmt.Errorf("creator application status %q", app.status)
			}
			if app.requirement != requirement {
				return fmt.Errorf("creator requirement differs: %q", app.requirement)
			}
		}
	}
	if len(readers) != 1 {
		return fmt.Errorf("plaintext readers: %d, want exactly the creator", len(readers))
	}
	for _, c := range creator {
		if readers[c] {
			return nil
		}
	}
	return errors.New("the plaintext reader is not the creator")
}

// The parser and the creator-only rule accept exactly the creator's ACL, and
// refuse a planted extra application, the security tool, an allow-all export
// entry, an "any" authorization for another app, a missing access section, a
// count mismatch and an unknown line. The fixtures follow real
// `dump-keychain -a` output; the sentinel applies the same check to its live
// dump (#138 review, finding 3). Limit: format drift in a future macOS makes
// the sentinel refuse, not pass.
func TestKeychainACLCreatorOnly(t *testing.T) {
	const service, account = "works.relux.a2a.sentinel", "@sentinel:sentinel.invalid"
	creator := "/private/tmp/go-build123/b001/cli.test"
	requirement := `cdhash H"0123456789abcdef0123456789abcdef01234567"`
	for _, c := range []struct{ file, want string }{
		{"creator-only.txt", ""},
		{"broadened-requirement.txt", "creator requirement differs"},
		{"status-not-ok.txt", "creator application status"},
		{"missing-requirement.txt", "creator requirement differs"},
		{"unknown-auth.txt", "unknown authorization"},
		{"extra-app.txt", "plaintext readers: 2"},
		{"security-tool.txt", "not the creator"},
		{"allow-all-export.txt", "any application"},
		{"any-auth.txt", "plaintext readers: 2"},
		{"no-acl.txt", "no access control"},
		{"count-mismatch.txt", "count mismatch"},
		{"unknown-line.txt", "unknown ACL line"},
	} {
		t.Run(c.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "acl", c.file))
			if err != nil {
				t.Fatal(err)
			}
			entries, err := itemACL(string(data), service, account)
			if err == nil {
				err = creatorOnly(entries, requirement, creator)
			}
			if c.want == "" && err != nil {
				t.Fatal("creator-only ACL refused:", err)
			}
			if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("want refusal %q, got %v", c.want, err)
			}
		})
	}
	data, _ := os.ReadFile(filepath.Join("testdata", "acl", "creator-only.txt"))
	if _, err := itemACL(string(data), service, "@absent:sentinel.invalid"); err == nil {
		t.Fatal("absent item accepted")
	}
	if _, err := itemACL(string(data), "works.relux.other", account); err == nil {
		t.Fatal("item of another service accepted")
	}
	if entries, err := itemACL(string(data), service, "@foreign:sentinel.invalid"); err != nil || creatorOnly(entries, requirement, creator) == nil {
		t.Fatal("foreign item's security-tool ACL not told apart", err)
	}
}
