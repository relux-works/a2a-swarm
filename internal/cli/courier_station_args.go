package cli

import "strings"

// Inspect only global selectors to find the command before any local execution
// or global error output. Authorization still checks every original operand.
func stationArgs(args []string) []string {
	for i := 0; i < len(args); i++ {
		name, _, eq := strings.Cut(args[i], "=")
		switch name {
		case "--as", "-as", "--profile", "--connection-fingerprint":
			if !eq {
				i++
			}
		case "--human":
		default:
			return args[i:]
		}
	}
	return nil
}
func stationCommand(args []string) string {
	rest := stationArgs(args)
	if len(rest) > 0 {
		return rest[0]
	}
	return ""
}

// Strip globals at every position before --, matching Authorize. Never
// interpret an event after -- or use arbitrary event bytes in diagnostics.
func stationHookEvent(args []string) string {
	var rest []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			break
		}
		name, _, eq := strings.Cut(args[i], "=")
		switch name {
		case "--as", "-as", "--profile", "--connection-fingerprint":
			if !eq {
				i++
			}
		default:
			rest = append(rest, args[i])
		}
	}
	if len(rest) == 0 || rest[0] != "hook" {
		return ""
	}
	if len(rest) < 2 {
		return "Hook"
	}
	for _, e := range hookEvents {
		if rest[1] == e.arg {
			return e.name
		}
	}
	if rest[1] == "session-end" {
		return "SessionEnd"
	}
	return "Hook"
}
