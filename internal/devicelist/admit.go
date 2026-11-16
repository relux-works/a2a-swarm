package devicelist

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
)

// KeySource is the origin of the Megolm session that decrypted an event.
type KeySource string

// KeySourceDirectOlm is the only source that counts (OM2, OM3): the receiver
// received the session as m.room_key in an Olm message from the sending
// device. Every other value, the receiver's own outbound session included,
// gives no command.
const KeySourceDirectOlm KeySource = "direct-olm"

// Event is the input of Admit: the facts about one room event that a verifier
// of the OM2 chain established. This package does not establish them.
type Event struct {
	// OriginatorEd25519 is the Ed25519 key of the device that the Megolm
	// session is bound to, in the unpadded base64 of the Matrix wire (OM1).
	OriginatorEd25519 string
	// SessionID and Index are the Megolm session identifier and the message
	// index that the session's signature covers: the replay key of OM5.
	SessionID string
	Index     uint64
	// Room is the room of the decrypted payload.
	Room string
	// KeySource is how the receiver got the session.
	KeySource KeySource
	// Plaintext marks an event that was not end-to-end encrypted (OM4).
	Plaintext bool
}

// Outcome is the kind of a Result. The zero value is none of them and admits
// nothing.
type Outcome string

const (
	OutcomeInstruct Outcome = "l1-instruct" // An owner command at level 1 with action instruct (OM6).
	OutcomeRefused  Outcome = "refused"     // A refusal with a LIP-0029 code.
	OutcomeIgnored  Outcome = "ignored"     // A plaintext event: no command and no code (OM4, O1.1).
)

// LevelL1 is the only level a messenger device reaches (OM1).
const LevelL1 = 1

// ActionInstruct is the action of a plain encrypted text (OM6).
const ActionInstruct = "instruct"

// Result is the observe-only result of Admit. No outcome authorizes delivery.
type Result struct {
	Outcome Outcome
	// Level and Action are set on OutcomeInstruct only: 1 and "instruct".
	Level  int
	Action string
	// Code and Step are set on OutcomeRefused: the LIP-0029 code, or one of
	// the two local refusal classes, and the step of section 11 that refused.
	// Step is 1 for OutcomeIgnored.
	Code Code
	Step int
	// Live copies the trusted ChainView.Live claim. Even when true, the
	// result is observe-only and must never gate delivery.
	Live bool
	// ObserveOnly is true on every Result that Admit returns, whatever Live
	// says: the journal behind the verdict is observe-grade (ObserveJournal),
	// so no verdict of this package is a source for enforcement.
	ObserveOnly bool
	// Label is the freshness label once step 4 reads the view. An absent
	// file is unknown; a present refused file or unwatched seal is invalid.
	Label Freshness
	// Tip is the tip the view was made at and NameHeight the height of the
	// name operation that carries the pin, once step 4 has run.
	Tip        Tip
	NameHeight uint64
	// SealState is the actual step-8 answer, empty if no seal read was reached.
	SealState SealState
	// ListSeq and ListSHA256 name the list the verdict was made under, once
	// step 5 has passed.
	ListSeq    uint64
	ListSHA256 string
}

// ObservedInstruct reports what an enforcing reader WOULD have decided:
// L1 instruct, subject to all checks outside this package. It reports only
// marked observe-only results and must never gate delivery.
func (r Result) ObservedInstruct() bool {
	return r.ObserveOnly && r.Outcome == OutcomeInstruct && r.Code == "" && r.Level == LevelL1 && r.Action == ActionInstruct
}

// sealState is the seal read of step 8 (O8.9). It is a variable only so that
// a control can plant its removal and prove that a named assertion fails;
// production never reassigns it.
var sealState = func(chain ChainView, seal Outpoint) (SealState, error) { return chain.Seal(seal) }

// seenUnspentBefore is the condition of a permanent closure: this journal
// recorded the seal as positively unspent before. It is a variable only so
// that a control can plant its removal and prove that a named assertion
// fails; production never reassigns it.
var seenUnspentBefore = func(state *ownerJournal, seal Outpoint) bool { _, ok := state.seenUnspent[seal]; return ok }

// Admit runs section 11 for one messenger event as OM7 changes it, in order,
// and stops at the first refusal:
//
//	step 1  OM4: a plaintext event is ignored. OM3: a session that is not
//	        direct-olm refuses MESSENGER_UNAUTHENTICATED. (The chain of OM2
//	        is the caller's; Event states its result.)
//	step 2  the event's room is ownerRoom, which stands in for aud:
//	        NOT_FOR_ME.
//	step 4  the view is usable for this check (CHAIN_VIEW_UNAVAILABLE when
//	        its label is invalid) and the journal has room for the owner
//	        (JOURNAL_FULL); the name is not expired and holds a valid
//	        swarma-devices (DEVICE_PIN_INVALID); its seq is not rolled back
//	        under the approximation of LIP-0010 SH5 (DEVICE_LIST_ROLLBACK,
//	        see decideHead).
//	step 5  lists holds a list under the pinned hash (LIST_UNKNOWN) whose
//	        octets hash to the pin and whose seq, owner and network are the
//	        pin's and the name's (DEVICE_LIST_MISMATCH). The head of step 4
//	        is journaled here, before steps 6 to 11: the pin comes from the
//	        protected view and the list hashes to it, so an event of an
//	        unlisted key or a replay can also move the head. Stale, older or
//	        equal-height different-hash observations never write it.
//	step 6  one record has the event's key bytes (DEVICE_UNKNOWN) and the
//	        alg matrix-ed25519 (DEVICE_ALG_UNSUPPORTED for a reserved alg,
//	        DEVICE_ALG_MISMATCH otherwise).
//	step 8  no closure of the device or of its seal is journaled
//	        (DEVICE_REVOKED), the owner's journal is not full (JOURNAL_FULL),
//	        the view holds a statement about the seal (CHAIN_VIEW_UNAVAILABLE
//	        with the label invalid), and the seal is positively unspent
//	        (DEVICE_REVOKED). At an unspent pass, all positively unspent seals
//	        of the authenticated list are journaled atomically, even if the
//	        event is later refused: the view is the evidence, not the event.
//	        Transient unconfirmed and spend_pending states refuse only the
//	        command and create no seal journal records.
//	        An absent seal always refuses this event; it is journaled as a
//	        observe-grade closure only when this journal saw that seal
//	        unspent before at a lower height or the same height and hash.
//	        Never-seen absence journals nothing, so a later unspent view can
//	        produce an instruct observation.
//	step 9  the record has no presence and ceiling exactly 1
//	        (DEVICE_LIST_INVALID, OM1 and S29-17; section 11 names
//	        LEVEL_ABOVE_DEVICE for a command above the ceiling, which a plain
//	        text at L1 never is). The level of the result is the constant 1
//	        and is never read from the record.
//	step 11 the pair (session, index) is not in the replay journal (REPLAY)
//	        and the journal has room for it (JOURNAL_FULL); on success it is
//	        recorded.
//
// Steps 3, 10, 12 and 13 have nothing to check for a plain text at L1 with
// no effect. at is the consumer's knowledge at this check: the age of the
// view is judged here, at every use. A view labelled stale or unknown still
// gives a verdict, with that label; no label makes the verdict more than an
// observation. Stale, older and equal-height different-hash observations
// write no chain-derived state (heads, seen-unspent entries or closures).
// Successful instruct observations always record the consumer-side replay
// pair, including under those views.
//
// An error means no verdict: an invalid argument or an unavailable chain
// view. Everything else is a Result. Admit reads nothing but its arguments
// and changes nothing but journal, and a refusal records no replay key. The
// name is read before the journal lock is taken; the seal is read under it,
// so a view that performs I/O in Seal serialises admissions.
func Admit(event Event, ownerRoom string, at Check, lists Lists, chain ChainView, journal *ObserveJournal) (Result, error) {
	if lists == nil || chain == nil || journal == nil || ownerRoom == "" {
		return Result{ObserveOnly: true}, ErrInvalid
	}
	result := Result{Live: chain.Live(), ObserveOnly: true}
	refuse := func(step int, code Code) (Result, error) {
		result.Outcome, result.Step, result.Code = OutcomeRefused, step, code
		return result, nil
	}
	// Step 1.
	if event.Plaintext {
		result.Outcome, result.Step = OutcomeIgnored, 1
		return result, nil
	}
	if event.KeySource != KeySourceDirectOlm {
		return refuse(1, MessengerUnauthenticated)
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(event.OriginatorEd25519)
	if err != nil || len(key) != 32 || event.SessionID == "" || len(event.SessionID) > 255 || event.Room == "" {
		return Result{ObserveOnly: true}, ErrInvalid
	}
	// Step 2.
	if event.Room != ownerRoom {
		return refuse(2, NotForMe)
	}
	// Step 4.
	switch chain.(type) {
	case UnavailableView, *UnavailableView:
		result.Label = worseLabel(chain.Freshness(at), LabelFresh)
		return refuse(4, ChainViewUnavailable)
	}
	name, err := chain.Name()
	if err != nil {
		return Result{ObserveOnly: true}, ErrChainView
	}
	if !validNetwork(name.Network) || !validName(name.Owner) {
		return Result{ObserveOnly: true}, fmt.Errorf("%w: name view without a network or an owner", ErrChainView)
	}
	result.Label, result.Tip, result.NameHeight = chain.Freshness(at), name.Tip, name.Op.Height
	switch result.Label {
	case LabelFresh, LabelStale, LabelUnknown:
	default:
		result.Label = LabelInvalid
		return refuse(4, ChainViewUnavailable)
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	state := journal.state(name.Network, name.Owner, false)
	writable := result.Label != LabelStale
	if state == nil {
		state = emptyOwnerJournal() // Allocate permanent state only when a write is needed.
		if len(journal.owners) >= journal.maxOwners {
			return refuse(4, JournalFull)
		}
	} else if state.hasHead && !observationCanFollow(name.Tip, state.head.ObservedAt) {
		writable = false
	}
	if name.Expired || !name.HasPin || !name.Pin.valid() {
		return refuse(4, DevicePinInvalid)
	}
	decision := decideHead(state, name.Pin, name.Op)
	if decision == headRollback {
		return refuse(4, DeviceListRollback)
	}
	// Step 5.
	list, ok := lists.Get(name.Pin.SHA256)
	if !ok || list == nil {
		return refuse(5, ListUnknown)
	}
	if !hashMatchesPin(list.raw, name.Pin.SHA256) {
		return refuse(5, DeviceListMismatch)
	}
	if list.seq != name.Pin.Seq || list.owner != name.Owner || list.network != name.Network {
		return refuse(5, DeviceListMismatch)
	}
	result.ListSeq, result.ListSHA256 = name.Pin.Seq, name.Pin.SHA256
	if writable {
		journal.owners[journalOwner{name.Network, name.Owner}] = state
		if decision == headTake {
			state.hasHead, state.head = true, Head{Seq: name.Pin.Seq, SHA256: name.Pin.SHA256, Op: name.Op, ObservedAt: name.Tip}
		} else {
			state.head.ObservedAt = name.Tip
		}
	}
	// Step 6. Keys are unique in a list (O3.3), so at most one record matches.
	var record *Record
	for i := range list.devices {
		decoded, ok := lowerHex(list.devices[i].Key, 32)
		if ok && bytes.Equal(decoded, key) {
			record = &list.devices[i]
			break
		}
	}
	if record == nil {
		return refuse(6, DeviceUnknown)
	}
	if record.Alg != AlgMatrixEd25519 {
		if entry, ok := registry[record.Alg]; ok && entry.class == algRuled {
			return refuse(6, DeviceAlgMismatch)
		}
		return refuse(6, DeviceAlgUnsupported)
	}
	// Step 8.
	if state.closed(record.Key, record.Seal) {
		return refuse(8, DeviceRevoked)
	}
	if state.full {
		return refuse(8, JournalFull)
	}
	seal, err := sealState(chain, record.Seal)
	if errors.Is(err, ErrSealUnknown) {
		result.Label = worseLabel(result.Label, LabelInvalid)
		return refuse(8, ChainViewUnavailable)
	}
	if err != nil {
		return Result{ObserveOnly: true}, ErrChainView
	}
	result.SealState = seal
	switch seal {
	case SealUnspent:
		if writable {
			// The authenticated view is evidence even if step 9 or 11 refuses.
			observations := make(map[Outpoint]Tip)
			for _, device := range list.devices {
				observed, readErr := sealState(chain, device.Seal)
				if readErr == nil && observed == SealUnspent {
					observations[device.Seal] = name.Tip
				}
			}
			if journal.sawUnspentSet(state, observations) != nil {
				return refuse(8, JournalFull)
			}
		}
	case SealUnconfirmed, SealSpendPending:
		return refuse(8, DeviceRevoked)
	case SealNotInUTXOSet:
		// Absence refuses this event whatever the journal holds. The closure
		// is permanent only for a seal seen unspent before; when it cannot be
		// recorded, close marks the owner full, and nothing of the owner is
		// admitted afterwards.
		observed := state.seenUnspent[record.Seal]
		if writable && observationCanFollow(name.Tip, observed) && seenUnspentBefore(state, record.Seal) {
			_ = journal.close(state, record.Key, record.Seal)
		}
		return refuse(8, DeviceRevoked)
	default:
		return Result{ObserveOnly: true}, fmt.Errorf("%w: seal state outside the closed set", ErrChainView)
	}
	// Step 9.
	if record.Presence || !messengerCeilingOK(record.Ceiling) {
		return refuse(9, DeviceListInvalid)
	}
	// Step 11.
	replay := replayKey{session: event.SessionID, index: event.Index}
	if state.replay[replay] {
		return refuse(11, Replay)
	}
	if len(state.replay) >= journal.maxOwnerReplay || journal.replayCount >= journal.maxReplay {
		return refuse(11, JournalFull)
	}
	journal.owners[journalOwner{name.Network, name.Owner}] = state
	state.replay[replay] = true
	journal.replayCount++
	result.Outcome, result.Level, result.Action = OutcomeInstruct, LevelL1, ActionInstruct
	return result, nil
}
