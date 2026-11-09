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
func stationHookEvent(args []string) string {
	rest := stationArgs(args)
	if len(rest) < 2 || rest[0] != "hook" {
		return ""
	}
	for _, e := range hookEvents {
		if rest[1] == e.arg {
			return e.name
		}
	}
	return ""
}
