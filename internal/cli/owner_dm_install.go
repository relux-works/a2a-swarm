package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Separate bare and argument forms: a leading wildcard means Claude's special
// single-trailing-wildcard bare-command shortcut does not apply.
var ownerClaudeDenials = []string{
	"Bash(*owner-dm ensure)", "Bash(*owner-dm ensure *)",
	"Bash(*owner-dm set)", "Bash(*owner-dm set *)",
	"Bash(*owner-dm unset)", "Bash(*owner-dm unset *)",
	"Bash(*principal-dm ensure)", "Bash(*principal-dm ensure *)",
	"Bash(*principal-dm unset)", "Bash(*principal-dm unset *)",
}
var ownerLegacyClaudeDenials = []string{"Bash(*owner-dm*ensure*)", "Bash(*owner-dm*set*)", "Bash(*owner-dm*unset*)"}

func ownerMergeDenials(root *jsonObject, install bool) error {
	permissions, err := root.childObject("permissions")
	if err != nil {
		return err
	}
	var current []string
	if raw, ok := permissions.get("deny"); ok {
		if json.Unmarshal(raw, &current) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("permissions.deny must be a string array")
		}
	}
	kept := []string{}
	for _, rule := range current {
		if !contains(ownerClaudeDenials, rule) && !contains(ownerLegacyClaudeDenials, rule) {
			kept = append(kept, rule)
		}
	}
	if install {
		kept = append(kept, ownerClaudeDenials...)
	}
	if len(kept) == 0 {
		permissions.del("deny")
	} else {
		permissions.set("deny", marshalNoEscape(kept))
	}
	if len(permissions.keys) == 0 {
		root.del("permissions")
	} else {
		root.set("permissions", permissions.marshal())
	}
	return nil
}

const ownerRulesHeader = "# a2a owner-dm terminal fallback: managed by a2a hook install\n"

func ownerCodexRules(path, exe, name string, install bool, aliases ...string) error {
	rules := filepath.Join(filepath.Dir(path), "rules", "a2a-owner-dm.rules")
	old, mode, err := readSettings(rules)
	if err != nil {
		return err
	}
	if old != nil && !strings.HasPrefix(string(old), ownerRulesHeader) {
		return fmt.Errorf("%s is not an a2a rules file; refusing to replace it", rules)
	}
	if !install {
		if old != nil {
			return os.Remove(rules)
		}
		return nil
	}
	var b strings.Builder
	b.WriteString(ownerRulesHeader)
	binaries := []string{}
	for _, alias := range append([]string{exe, "a2a", "bin/a2a", "./bin/a2a"}, aliases...) {
		if alias != "" && !contains(binaries, alias) {
			binaries = append(binaries, alias)
		}
	}
	for _, binary := range binaries {
		for _, action := range []string{"ensure", "set", "unset"} {
			fmt.Fprintf(&b, "prefix_rule(pattern = [%s, \"owner-dm\", %q], decision = \"forbidden\", justification = \"Host terminal only; use owner-dm bind/unbind --event or status.\")\n", marshalNoEscape(binary), action)
		}
		for _, action := range []string{"ensure", "unset"} {
			fmt.Fprintf(&b, "prefix_rule(pattern = [%s, \"principal-dm\", %q], decision = \"forbidden\", justification = \"Host terminal only; use principal-dm status or say.\")\n", marshalNoEscape(binary), action)
		}
	}
	if bytes.Equal(old, []byte(b.String())) {
		return nil
	}
	return writeSettings(rules, []byte(b.String()), mode)
}
