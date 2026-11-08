package cli

import (
	"path/filepath"
	"strings"
)

// Read only the first txt vnode, as bindingImage does. Keep this pure parser
// portable so Linux tests can exercise Darwin records without claiming E2E.
func stateWriterLsofPath(raw, image string) string {
	text := false
	dev, ino, path := "", "", ""
	for _, line := range strings.Split(raw, "\n") {
		if len(line) < 2 {
			continue
		}
		if line[0] == 'f' {
			if text {
				break
			}
			text = line == "ftxt"
			continue
		}
		if !text {
			continue
		}
		switch line[0] {
		case 'D':
			if dev != "" {
				return ""
			}
			dev = line[1:]
		case 'i':
			if ino != "" {
				return ""
			}
			ino = line[1:]
		case 'n':
			if path != "" {
				return ""
			}
			path = line[1:]
		}
	}
	if !text || dev == "" || ino == "" || dev+":"+ino != image || !filepath.IsAbs(path) {
		return ""
	}
	return path
}
