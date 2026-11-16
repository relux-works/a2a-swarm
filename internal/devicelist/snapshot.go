package devicelist

import "path/filepath"

// Bounds of the snapshot file (chain gateway contract, section 5).
const (
	MaxSnapshotBytes = 64 << 10
	MaxSnapshotNames = 16
	MaxSnapshotSeals = 256
)

// ConsistencyBracketed is the only consistency of a snapshot that this reader
// accepts: the helper read the tip before and after its collection and found
// it equal. That is weaker than a witnessed collection (a tip that left and
// came back between the two reads is not seen), which is one reason why what
// this reader learns from the file is observe-grade.
const ConsistencyBracketed = "bracketed"

// Freshness thresholds. They are the CONSUMER's constants: no bound is ever
// read from the snapshot file, which cannot state how long it may be used.
const (
	// SnapshotMaxAgeSeconds: a snapshot written more than this long before
	// the check is stale. The helper rewrites the file at least every 30 s.
	SnapshotMaxAgeSeconds = 60
	// SnapshotMaxTipLagSeconds: a snapshot whose tip block time lies more
	// than this long before the check is stale, however young the file is. A
	// lag of exactly the bound is not stale.
	SnapshotMaxTipLagSeconds = 7200
	// SnapshotFutureSkewSeconds: future write times are invalid without grace.
	SnapshotFutureSkewSeconds = 0
)

// NameRecord is one name of the snapshot: the exact octets of its value, the
// expiry the node evaluated at the tip, and the outpoint and block of its
// last confirmed name operation.
type NameRecord struct {
	Name    string
	Value   []byte
	Expired bool
	Op      NameOp
	Vout    uint32
}

// SealRecord is the state of one seal outpoint of the snapshot.
type SealRecord struct {
	Outpoint Outpoint
	State    SealState
	// Confirmations and ScriptPubKey are transported for present states only.
	// No accepted script binding exists at the pinned specification revision.
	Confirmations uint64
	ScriptPubKey  string
}

// SnapshotData is the parsed value of a snapshot file: everything the file
// states. All records share the one Tip. WrittenAt is POSIX seconds.
type SnapshotData struct {
	Consistency string
	Network     string
	Names       []NameRecord
	Seals       []SealRecord
	Tip         Tip
	WrittenAt   uint64
}

// SnapshotConfig is the protected host configuration that a snapshot is read
// under. Nothing in it comes from the file, an agent's configuration or the
// environment, and there is no default and no discovery.
type SnapshotConfig struct {
	// Path is the one absolute clean path of the snapshot file.
	Path string
	// Network is the network the host pins: "main" or "regtest".
	Network string
	// Owner is the id/ name whose pin the view serves. It is one of Names.
	Owner string
	// Names and Seals are the exact watch set that the host configured for
	// the helper. A file that names more, fewer or other records is invalid,
	// so a plausible snapshot of a foreign owner is not acceptable.
	Names []string
	Seals []Outpoint
}

func (c SnapshotConfig) valid() bool {
	if !filepath.IsAbs(c.Path) || filepath.Clean(c.Path) != c.Path || c.Path == "/" {
		return false
	}
	if !validNetwork(c.Network) || !validName(c.Owner) {
		return false
	}
	if len(c.Names) < 1 || len(c.Names) > MaxSnapshotNames || len(c.Seals) < 1 || len(c.Seals) > MaxSnapshotSeals {
		return false
	}
	names, owned := map[string]bool{}, false
	for _, name := range c.Names {
		if !validSnapshotName(name) || names[name] {
			return false
		}
		names[name] = true
		owned = owned || name == c.Owner
	}
	seals := map[Outpoint]bool{}
	for _, seal := range c.Seals {
		if !validHash(seal.TxID) || seals[seal] {
			return false
		}
		seals[seal] = true
	}
	return owned
}

// Snapshot is the ChainView over one snapshot file for one owner: a
// host-written, root-owned statement of the watched name values and seal
// outpoints at one tip. It is valid for observation only. It cannot see a
// seal spend that happens after it was written, so Live is false whatever its
// label is, and a verdict made on it must not switch admission.
type Snapshot struct {
	name      NameView
	seals     map[Outpoint]SealState
	tip       Tip
	writtenAt uint64
}

// newSnapshot makes the view from the octets of a snapshot file. Every view
// passed the one parser: there is no way to make one from other data. After
// the schema it checks the bindings to the host configuration: the network,
// and the exact watch set of names and of seals. A mismatch is
// ErrSnapshotInvalid; a malformed configuration is ErrInvalid.
//
// An expired name gives no pin, and neither does a name value without a
// valid swarma-devices: the view then reports no pin, and Admit refuses
// DEVICE_PIN_INVALID.
func newSnapshot(raw []byte, config SnapshotConfig) (*Snapshot, error) {
	if !config.valid() {
		return nil, ErrInvalid
	}
	data, err := parseSnapshotFile(raw)
	if err != nil {
		return nil, err
	}
	if data.Network != config.Network || len(data.Names) != len(config.Names) || len(data.Seals) != len(config.Seals) {
		return nil, ErrSnapshotInvalid
	}
	// The parser refused duplicates and the configuration holds none, so
	// equal counts and "every record of the file is configured" is equality
	// of the two sets.
	watchedNames := map[string]bool{}
	for _, name := range config.Names {
		watchedNames[name] = true
	}
	var owner *NameRecord
	for i := range data.Names {
		if !watchedNames[data.Names[i].Name] {
			return nil, ErrSnapshotInvalid
		}
		if data.Names[i].Name == config.Owner {
			owner = &data.Names[i]
		}
	}
	watchedSeals := map[Outpoint]bool{}
	for _, seal := range config.Seals {
		watchedSeals[seal] = true
	}
	seals := map[Outpoint]SealState{}
	for _, seal := range data.Seals {
		if !watchedSeals[seal.Outpoint] {
			return nil, ErrSnapshotInvalid
		}
		seals[seal.Outpoint] = seal.State
	}
	if owner == nil {
		return nil, ErrSnapshotInvalid
	}
	view := NameView{Network: data.Network, Owner: owner.Name, Expired: owner.Expired, Op: owner.Op, Tip: data.Tip}
	if !owner.Expired {
		if pin, err := PinFromNameValue(owner.Value); err == nil {
			view.HasPin, view.Pin = true, pin
		}
	}
	return &Snapshot{name: view, seals: seals, tip: data.Tip, writtenAt: data.WrittenAt}, nil
}

// Name returns the owner's name as the snapshot holds it.
func (s *Snapshot) Name() (NameView, error) { return s.name, nil }

// Seal returns the state the snapshot holds for the outpoint, or
// ErrSealUnknown when it holds none.
func (s *Snapshot) Seal(seal Outpoint) (SealState, error) {
	state, ok := s.seals[seal]
	if !ok {
		return "", ErrSealUnknown
	}
	return state, nil
}

// Freshness labels the snapshot for one check, with the consumer's constants.
func (s *Snapshot) Freshness(at Check) Freshness { return snapshotLabel(s.writtenAt, s.tip, at) }

// Live is false: a snapshot is for observation only.
func (s *Snapshot) Live() bool { return false }

// snapshotLabel is the one label function of a snapshot that passed the
// parser and the bindings (a file that did not has no view; its label is
// invalid). The precedence is the contract's:
//
//  1. no trustworthy clock: unknown;
//  2. written later than now, or tip time more than 7200 s ahead: invalid;
//  3. older than SnapshotMaxAgeSeconds, or its tip block time more than
//     SnapshotMaxTipLagSeconds before now: stale;
//  4. no independently known live tip: unknown;
//  5. a live tip that differs in hash, height or time: stale;
//  6. otherwise fresh.
//
// On a host that has only the snapshot file there is no independent live
// tip, so the label is never fresh there.
func snapshotLabel(writtenAt uint64, tip Tip, at Check) Freshness {
	if !at.ClockTrusted {
		return LabelUnknown
	}
	// Both times are at most 2^53-1 after the parser, so the differences
	// cannot overflow.
	now := at.Now.Unix()
	age := now - int64(writtenAt)
	if age < -SnapshotFutureSkewSeconds || int64(tip.Time)-now > SnapshotMaxTipLagSeconds {
		return LabelInvalid
	}
	if age > SnapshotMaxAgeSeconds || now-int64(tip.Time) > SnapshotMaxTipLagSeconds {
		return LabelStale
	}
	if at.LiveTip == nil {
		return LabelUnknown
	}
	if *at.LiveTip != tip {
		return LabelStale
	}
	return LabelFresh
}

func validHash(s string) bool {
	_, ok := lowerHex(s, 32)
	return ok
}

// validSnapshotName is the Name of the chain gateway contract: 1 to 255 bytes
// matching [a-z0-9][a-z0-9/._-]*. It is wider than the id/ name of a device
// list owner (validName); the configured owner must satisfy both.
func validSnapshotName(s string) bool {
	if len(s) < 1 || len(s) > 255 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		lead := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if !lead && (i == 0 || (c != '/' && c != '.' && c != '_' && c != '-')) {
			return false
		}
	}
	return true
}
