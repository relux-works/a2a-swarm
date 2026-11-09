// Package deliverypolicy shares delivery lifetime classification between the
// station authorization parser and the command engine.
package deliverypolicy

import "time"

const DefaultWaitTimeout = 25 * time.Minute

// LongConsumer excludes finite waits, including the default wait timeout.
func LongConsumer(command string, timeout time.Duration) bool {
	return command == "watch" || command == "wait" && timeout == 0
}
