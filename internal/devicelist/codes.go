package devicelist

import "errors"

// Code is a LIP-0029 refusal code or a package-local refusal class.
// IsLocal separates the two; local classes must not be sent as LIP-0029 codes.
// It is an error so that codec functions can return it wrapped.
type Code string

func (c Code) Error() string { return string(c) }

// IsLocal reports the two local refusal classes, not specification codes.
func (c Code) IsLocal() bool { return c == ChainViewUnavailable || c == JournalFull }

// Refusal codes of LIP-0029 that this package can produce.
const (
	DeviceListInvalid        Code = "DEVICE_LIST_INVALID"       // Sections 3.1, 3.2, 3.6, OM1.
	DeviceListMismatch       Code = "DEVICE_LIST_MISMATCH"      // O3.6, O3.11, step 5.
	DeviceListRollback       Code = "DEVICE_LIST_ROLLBACK"      // O9.2, step 4.
	DevicePinInvalid         Code = "DEVICE_PIN_INVALID"        // O3.4, step 4.
	ListUnknown              Code = "LIST_UNKNOWN"              // O3.14, step 5.
	DeviceUnknown            Code = "DEVICE_UNKNOWN"            // Step 6.
	DeviceAlgMismatch        Code = "DEVICE_ALG_MISMATCH"       // O3.1, step 6.
	DeviceAlgUnsupported     Code = "DEVICE_ALG_UNSUPPORTED"    // O3.17, step 6.
	DeviceRevoked            Code = "DEVICE_REVOKED"            // O8.4, O8.5, step 8.
	MessengerUnauthenticated Code = "MESSENGER_UNAUTHENTICATED" // OM3.
	NotForMe                 Code = "NOT_FOR_ME"                // Step 2, the room for aud (OM7).
	Replay                   Code = "REPLAY"                    // OM5.
)

// Local refusal classes. They are NOT LIP-0029 codes and name no fact about a
// device: they say that this reader could make no admission. Admit returns
// them as refusals for valid events, including when passed UnavailableView.
// Invalid arguments or other adapter errors can still return an error.
const (
	// ChainViewUnavailable: the chain view holds no usable statement for the
	// event: its label is invalid, or it holds no statement about the seal of
	// the device. It is the unavailable class of the chain gateway contract
	// and never means DEVICE_REVOKED.
	ChainViewUnavailable Code = "CHAIN_VIEW_UNAVAILABLE"
	// JournalFull: the journal could not record something it had to record.
	// The refusal is sticky for the owner (see ObserveJournal).
	JournalFull Code = "JOURNAL_FULL"
)

// Operational failures. They are never a verdict about an event: Admit
// returns them as an error with Result{ObserveOnly: true} and no command verdict.
var (
	ErrInvalid             = errors.New("devicelist: invalid argument")
	ErrChainView           = errors.New("devicelist: chain view unavailable")
	ErrSealUnknown         = errors.New("devicelist: seal outpoint not in the chain view")
	ErrJournalFull         = errors.New("devicelist: journal full")
	ErrCapacity            = errors.New("devicelist: input over the size bound")
	ErrSnapshotInvalid     = errors.New("devicelist: snapshot invalid")
	ErrSnapshotUnsafe      = errors.New("devicelist: snapshot outside the host filesystem profile")
	ErrSnapshotUnavailable = errors.New("devicelist: snapshot absent")
)
