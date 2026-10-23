package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ownerClaudeDenials = []string{"Bash(*owner-dm*ensure*)", "Bash(*owner-dm*set*)", "Bash(*owner-dm*unset*)"}

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
		if !contains(ownerClaudeDenials, rule) {
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

func ownerCodexRules(path, exe, name string, install bool) error {
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
	binaries := []string{exe, "a2a", "bin/a2a", "./bin/a2a"}
	for _, action := range []string{"ensure", "set", "unset"} {
		fmt.Fprintf(&b, "prefix_rule(pattern = [%s, \"owner-dm\", %q], decision = \"forbidden\", justification = \"Host terminal only; use owner-dm bind/unbind --event or status.\")\n", marshalNoEscape(binaries), action)
		fmt.Fprintf(&b, "prefix_rule(pattern = [%s, \"--as\", %q, \"owner-dm\", %q], decision = \"forbidden\", justification = \"Host terminal only; use owner-dm bind/unbind --event or status.\")\n", marshalNoEscape(binaries), name, action)
	}
	if bytes.Equal(old, []byte(b.String())) {
		return nil
	}
	return writeSettings(rules, []byte(b.String()), mode)
}
