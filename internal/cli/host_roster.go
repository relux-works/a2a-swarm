package cli

import (
	"errors"
	"io"
	"os"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/envelopeobserve"
)

const hostRosterUsage = "usage: a2a host roster build --from FILE --out ABSOLUTE_PATH | a2a host roster show [--check]"

// The input bound matches the strict parser's entity limit; one more byte is
// read so an oversized document is refused instead of silently truncated.
const hostRosterInputLimit = 1 << 20

// errHostRosterUnavailable carries no path or file content: the reader does
// not distinguish an absent, unprotected or malformed roster.
var errHostRosterUnavailable = errors.New("host roster unavailable: absent, unprotected or malformed")

// cmdHostRoster: a2a host roster build|show. It opens no Matrix session, reads
// no key store and calls no signer; the roster is the operator's assertion.
func cmdHostRoster(a *App, args []string) error {
	if len(args) == 0 {
		return usagef("%s", hostRosterUsage)
	}
	switch args[0] {
	case "build":
		fs := newFlagSet("host roster build")
		from := fs.String("from", "", "a2a-roster-input/1 document")
		out := fs.String("out", "", "absolute path of the roster to publish")
		pos, err := parseInterspersed(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 0 || *from == "" || *out == "" {
			return usagef("%s", hostRosterUsage)
		}
		input, err := readHostRosterInput(*from)
		if err != nil {
			return err
		}
		n, err := envelopeobserve.WriteRoster(input, *out, a.rosterProtection)
		if err != nil {
			return err
		}
		a.printf("roster %s: %d authors\n", sanitizeLine(*out), n)
		return nil
	case "show":
		fs := newFlagSet("host roster show")
		check := fs.Bool("check", false, "exit non-zero when the reader would load no roster")
		pos, err := parseInterspersed(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 0 {
			return usagef("%s", hostRosterUsage)
		}
		p, err := a.paths()
		if err != nil {
			return err
		}
		load := envelopeobserve.Load
		if a.loadEnvelopeRoster != nil {
			load = a.loadEnvelopeRoster
		}
		observer, err := load(config.EnvelopeRosterPath(p))
		if err != nil {
			if *check {
				return errHostRosterUnavailable
			}
			a.printf("roster unavailable\n")
			return nil
		}
		for _, row := range observer.Authors() {
			a.printf("%s\t%s\t%s\n", row.MXID, row.KeyType, row.Fingerprint)
		}
		return nil
	}
	return usagef("%s", hostRosterUsage)
}

func readHostRosterInput(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, &envelopeobserve.RosterRefusal{Code: "input_unreadable"}
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, hostRosterInputLimit+1))
	if err != nil {
		return nil, &envelopeobserve.RosterRefusal{Code: "input_unreadable"}
	}
	return raw, nil
}
