package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const maxRedactionReasonBytes = 1024

const redactionLimit = "Event redaction is best effort on homeservers and across federation. It does not recall already copied mailbox versions, archive Git commits, owner-DM mirrors, projections, downloads or media. Matrix redaction does not guarantee media deletion. Rotate leaked secrets."

const archiveRedactionWarning = "a2a: redacted bodies are omitted; earlier archive Git commits may still retain them."

var redactionTxnRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// Keep every diagnostic in a closed vocabulary: neither an arbitrary
// transport error, response entity, event type nor reason can be printed.
func redactionFailure(err error) error {
	for _, code := range []string{"M_FORBIDDEN", "M_NOT_FOUND", "M_LIMIT_EXCEEDED"} {
		if matrix.IsCode(err, code) {
			return fmt.Errorf("redact: %s", code)
		}
	}
	if errors.Is(err, matrix.ErrAuth) {
		return errors.New("redact: REDACT_AUTH")
	}
	return errors.New("redact: REDACT_FAILED (outcome may be unknown; replay only with the same transaction id)")
}

func cmdRedact(ctx context.Context, a *App, args []string) error {
	const use = "usage: a2a redact <room> <event> --reason TEXT [--txn-id ID]"
	fs := newFlagSet("redact")
	reason := fs.String("reason", "", "literal reason (1..1024 UTF-8 bytes; no controls)")
	txn := fs.String("txn-id", "", "reuse the original transaction id for an explicit replay")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 2 {
		return usagef(use)
	}
	if !utf8.ValidString(*reason) || strings.TrimSpace(*reason) == "" || len(*reason) > maxRedactionReasonBytes {
		return usagef("redact: reason must be 1..1024 UTF-8 bytes of nonempty text")
	}
	for _, r := range *reason {
		if isUnsafeRune(r) {
			return usagef("redact: reason must not contain control or bidi characters")
		}
	}
	if len(pos[1]) < 2 || len(pos[1]) > 255 || pos[1][0] != '$' || sanitizeLine(pos[1]) != pos[1] {
		return usagef("redact: invalid event id")
	}
	if *txn == "" {
		*txn = matrix.NewTxnID()
	}
	if !redactionTxnRE.MatchString(*txn) {
		return usagef("redact: invalid transaction id (1..128 ASCII letters, digits, dot, underscore or hyphen)")
	}
	s, err := a.session()
	if err != nil {
		return redactionFailure(err)
	}
	room, _, err := s.resolveRoom(ctx, pos[0])
	if err != nil {
		return redactionFailure(err)
	}
	fmt.Fprintln(a.Stderr, redactionLimit)
	fmt.Fprintln(a.Stderr, "redact transaction:", *txn)
	typ, err := s.client.RedactionEventType(ctx, room, pos[1])
	if err != nil {
		return redactionFailure(err)
	}
	if typ == "m.room.redaction" {
		return errors.New("redact: REDACT_TARGET_REDACTION (redacting a redaction does not undo the original redaction)")
	}
	id, err := s.client.RedactEventTxn(ctx, room, pos[1], *txn, *reason)
	if err != nil {
		return redactionFailure(err)
	}
	// Store the authorised target immediately as well as on later sync. A local
	// failure never resends the successful server request.
	_, err = s.lockedDeliver(ctx, time.Time{}, false, func(_ *config.State, mb *config.Mailbox) ([]string, error) {
		return nil, mb.Redact([]config.Redaction{{Room: room, EventID: pos[1]}})
	})
	if err != nil {
		return errors.New("redact: REDACT_LOCAL_CACHE_FAILED (server accepted; no resend was attempted)")
	}
	a.printf("%s\n", sanitizeLine(id))
	return nil
}

// eventRedactions records content-suppression targets, never payload data.
// Filtered local tombstones do not prove that each event was redacted remotely.
func eventRedactions(room string, events []matrix.Event) []config.Redaction {
	var targets []config.Redaction
	for _, ev := range events {
		if id := ev.RedactionTarget(); id != "" {
			targets = append(targets, config.Redaction{Room: room, EventID: id})
		}
		if ev.IsRedacted() && ev.EventID != "" {
			targets = append(targets, config.Redaction{Room: room, EventID: ev.EventID})
		}
	}
	return targets
}

// installRedactionCache filters every message-reading client endpoint before
// content decoding. Reads use an atomic mailbox snapshot, never a nested state
// lock (some existing consumers already hold it). Sync owns durable mutation.
func (s *session) installRedactionCache() {
	seen := map[config.Redaction]bool{}
	var mu sync.Mutex
	s.client.FilterEvents = func(room string, events []matrix.Event) ([]matrix.Event, error) {
		mu.Lock()
		defer mu.Unlock()
		mb, err := config.OpenMailbox(s.paths, s.name)
		if err != nil {
			// Preserve the existing mailbox refusal class when filtering runs before
			// delivery opens the mailbox. Report only a fixed diagnostic, never the
			// cache path or arbitrary file/transport data.
			if info, statErr := os.Lstat(s.paths.MailboxFile(s.name)); statErr == nil && !info.Mode().IsRegular() {
				return nil, errors.New("local redaction cache unavailable: mailbox is not a regular file; refusing to use it")
			}
			return nil, errors.New("local redaction cache unavailable")
		}
		for _, target := range eventRedactions(room, events) {
			seen[target] = true
		}
		out := make([]matrix.Event, len(events))
		for i, ev := range events {
			known := func(id string) bool {
				return id != "" && (mb.IsRedacted(room, id) || seen[config.Redaction{Room: room, EventID: id}])
			}
			if known(ev.EventID) || known(ev.ReplacementTarget()) {
				ev = ev.WithoutContent()
			}
			out[i] = ev
		}
		return out, nil
	}
}
