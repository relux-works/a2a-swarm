package devicelist

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strconv"
)

// The snapshot file is the root snapshot file of the chain gateway contract
// (swarma-chain-lab CONTRACT.md at d1cab6f8, section 5, part (B)).
// It is OBSERVE ONLY.
//
// One CG1 canonical object, at most 65536 bytes, no LF, keys in ascending
// ASCII order at every level, all members required, none unknown:
//
//	{"consistency":C,"mode":"observe_only","names":[NameRecord...],
//	 "network":"main" or "regtest","seals":[SealRecord...],"tip":Tip,"v":1,
//	 "written_at":N}
//	NameRecord {"expired":bool,"height":N,"name":Name,"name_block_hash":H,
//	            "txid":H,"value_b64":Value,"vout":O}
//	SealRecord {"confirmations":N,"script_pubkey":Script,"state":S,"txid":H,"vout":O}
//	           for unspent, unconfirmed and spend_pending; for absence only:
//	           {"state":"not_in_utxo_set","txid":H,"vout":O}
//	Tip        {"hash":H,"height":N,"time":N}
//
// snapshotFile and its three record types are the one parser type of the
// file: no member name of the file is spelled outside this file, and
// parseSnapshotFile is the only entry. What a change of the contract touches
// beyond this file depends on the change: a renamed member or another
// spelling stays here; a new fact (as the tip time and the consistency were)
// also changes snapshotDataValue, the view and the label function in snapshot.go.
type snapshotFile struct {
	Consistency string             `json:"consistency"`
	Mode        string             `json:"mode"`
	Names       []snapshotFileName `json:"names"`
	Network     string             `json:"network"`
	Seals       []snapshotFileSeal `json:"seals"`
	Tip         snapshotFileTip    `json:"tip"`
	V           uint64             `json:"v"`
	WrittenAt   uint64             `json:"written_at"`
}

type snapshotFileName struct {
	Expired       bool   `json:"expired"`
	Height        uint64 `json:"height"`
	Name          string `json:"name"`
	NameBlockHash string `json:"name_block_hash"`
	TxID          string `json:"txid"`
	ValueB64      string `json:"value_b64"`
	Vout          uint32 `json:"vout"`
}

type snapshotFileSeal struct {
	Confirmations *uint64 `json:"confirmations"`
	ScriptPubKey  *string `json:"script_pubkey"`
	State         string  `json:"state"`
	TxID          string  `json:"txid"`
	Vout          uint32  `json:"vout"`
}

type snapshotFileTip struct {
	Hash   string `json:"hash"`
	Height uint64 `json:"height"`
	Time   uint64 `json:"time"`
}

const (
	snapshotFileMode    = "observe_only"
	snapshotFileVersion = 1
)

// consistencyAccepted is the consistency rule of the snapshot file: this
// reader accepts exactly "bracketed". Every other value, "witnessed" and any
// value a later contract adds included, makes the file invalid: this reader
// was not qualified for it. It is a variable only so that a control can plant
// its removal and prove that a named assertion fails; production never
// reassigns it.
var consistencyAccepted = func(value string) bool { return value == ConsistencyBracketed }

// parseSnapshotFile is the single entry point of the snapshot file format.
// It checks the schema and the relations that the file states about itself;
// the bindings to the host configuration are newSnapshot's. The parsed value
// is re-encoded and must give the identical octets, so a duplicated, escaped,
// case-alias, unknown, missing or reordered member, null, a float, an
// exponent, a sign, a leading zero, white space, a BOM, an LF and trailing
// bytes all refuse. Every refusal is ErrSnapshotInvalid, but a file over the
// size bound, which is ErrCapacity; both mean the label invalid.
func parseSnapshotFile(raw []byte) (snapshotDataValue, error) {
	if len(raw) > MaxSnapshotBytes {
		return snapshotDataValue{}, ErrCapacity
	}
	var f snapshotFile
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&f); err != nil {
		return snapshotDataValue{}, ErrSnapshotInvalid
	}
	if f.Mode != snapshotFileMode || f.V != snapshotFileVersion || !validNetwork(f.Network) {
		return snapshotDataValue{}, ErrSnapshotInvalid
	}
	if !printable(f.Consistency) || !consistencyAccepted(f.Consistency) {
		return snapshotDataValue{}, ErrSnapshotInvalid
	}
	if !validHash(f.Tip.Hash) || f.Tip.Height > MaxSeq || f.Tip.Time > MaxSeq || f.WrittenAt > MaxSeq {
		return snapshotDataValue{}, ErrSnapshotInvalid
	}
	if len(f.Names) < 1 || len(f.Names) > MaxSnapshotNames || len(f.Seals) < 1 || len(f.Seals) > MaxSnapshotSeals {
		return snapshotDataValue{}, ErrSnapshotInvalid
	}
	data := snapshotDataValue{
		Consistency: f.Consistency,
		Network:     f.Network,
		Tip:         Tip{Hash: f.Tip.Hash, Height: f.Tip.Height, Time: f.Tip.Time},
		WrittenAt:   f.WrittenAt,
	}
	for i, n := range f.Names {
		// Sorted strictly by the name's ASCII bytes: also no duplicate.
		if !validSnapshotName(n.Name) || (i > 0 && n.Name <= f.Names[i-1].Name) {
			return snapshotDataValue{}, ErrSnapshotInvalid
		}
		if !validHash(n.NameBlockHash) || !validHash(n.TxID) || n.Height > f.Tip.Height {
			return snapshotDataValue{}, ErrSnapshotInvalid
		}
		// An operation in the tip block is in the block the tip names.
		if n.Height == f.Tip.Height && n.NameBlockHash != f.Tip.Hash {
			return snapshotDataValue{}, ErrSnapshotInvalid
		}
		value, err := base64.StdEncoding.Strict().DecodeString(n.ValueB64)
		if err != nil || len(value) > MaxNameValue {
			return snapshotDataValue{}, ErrSnapshotInvalid
		}
		data.Names = append(data.Names, snapshotNameRecord{
			Name:    n.Name,
			Value:   value,
			Expired: n.Expired,
			Op:      NameOp{TxID: n.TxID, Height: n.Height, BlockHash: n.NameBlockHash},
			Vout:    n.Vout,
		})
	}
	for i, s := range f.Seals {
		state := SealState(s.State)
		if !validHash(s.TxID) {
			return snapshotDataValue{}, ErrSnapshotInvalid
		}
		record := snapshotSealRecord{Outpoint: Outpoint{TxID: s.TxID, Vout: s.Vout}, State: state}
		switch state {
		case SealUnspent, SealUnconfirmed, SealSpendPending:
			if s.Confirmations == nil || s.ScriptPubKey == nil || *s.Confirmations > MaxSeq ||
				!validScript(*s.ScriptPubKey) || s.Vout > 2147483647 {
				return snapshotDataValue{}, ErrSnapshotInvalid
			}
			if state == SealUnconfirmed {
				if *s.Confirmations != 0 {
					return snapshotDataValue{}, ErrSnapshotInvalid
				}
			} else if *s.Confirmations == 0 || *s.Confirmations > f.Tip.Height+1 {
				return snapshotDataValue{}, ErrSnapshotInvalid
			}
			record.Confirmations, record.ScriptPubKey = *s.Confirmations, *s.ScriptPubKey
		case SealNotInUTXOSet:
			if s.Confirmations != nil || s.ScriptPubKey != nil {
				return snapshotDataValue{}, ErrSnapshotInvalid
			}
		default:
			return snapshotDataValue{}, ErrSnapshotInvalid
		}
		// Sorted strictly by txid, then by the numeric vout: also no duplicate.
		if i > 0 {
			previous := f.Seals[i-1]
			if s.TxID < previous.TxID || (s.TxID == previous.TxID && s.Vout <= previous.Vout) {
				return snapshotDataValue{}, ErrSnapshotInvalid
			}
		}
		data.Seals = append(data.Seals, record)
	}
	if !bytes.Equal(encodeSnapshotFile(data), raw) {
		return snapshotDataValue{}, ErrSnapshotInvalid
	}
	return data, nil
}

// encodeSnapshotFile writes the one spelling that parseSnapshotFile admits:
// compact, keys in ascending ASCII order, minimal decimals, padded standard
// base64. The reader uses it for the identity check, and controls use it to
// make fixtures without naming a member of the file. The caller has checked
// that every string is printable ASCII.
func encodeSnapshotFile(data snapshotDataValue) []byte {
	b := append(make([]byte, 0, 1024), `{"consistency":`...)
	b = appendString(b, data.Consistency)
	b = append(b, `,"mode":`...)
	b = appendString(b, snapshotFileMode)
	b = append(b, `,"names":[`...)
	for i, n := range data.Names {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"expired":`...)
		b = strconv.AppendBool(b, n.Expired)
		b = append(b, `,"height":`...)
		b = strconv.AppendUint(b, n.Op.Height, 10)
		b = append(b, `,"name":`...)
		b = appendString(b, n.Name)
		b = append(b, `,"name_block_hash":`...)
		b = appendString(b, n.Op.BlockHash)
		b = append(b, `,"txid":`...)
		b = appendString(b, n.Op.TxID)
		b = append(b, `,"value_b64":`...)
		b = appendString(b, base64.StdEncoding.EncodeToString(n.Value))
		b = append(b, `,"vout":`...)
		b = strconv.AppendUint(b, uint64(n.Vout), 10)
		b = append(b, '}')
	}
	b = append(b, `],"network":`...)
	b = appendString(b, data.Network)
	b = append(b, `,"seals":[`...)
	for i, s := range data.Seals {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '{')
		if s.State != SealNotInUTXOSet {
			b = append(b, `"confirmations":`...)
			b = strconv.AppendUint(b, s.Confirmations, 10)
			b = append(b, `,"script_pubkey":`...)
			b = appendString(b, s.ScriptPubKey)
			b = append(b, ',')
		}
		b = append(b, `"state":`...)
		b = appendString(b, string(s.State))
		b = append(b, `,"txid":`...)
		b = appendString(b, s.Outpoint.TxID)
		b = append(b, `,"vout":`...)
		b = strconv.AppendUint(b, uint64(s.Outpoint.Vout), 10)
		b = append(b, '}')
	}
	b = append(b, `],"tip":{"hash":`...)
	b = appendString(b, data.Tip.Hash)
	b = append(b, `,"height":`...)
	b = strconv.AppendUint(b, data.Tip.Height, 10)
	b = append(b, `,"time":`...)
	b = strconv.AppendUint(b, data.Tip.Time, 10)
	b = append(b, `},"v":`...)
	b = strconv.AppendUint(b, snapshotFileVersion, 10)
	b = append(b, `,"written_at":`...)
	b = strconv.AppendUint(b, data.WrittenAt, 10)
	return append(b, '}')
}

func validScript(script string) bool {
	if len(script) > 20000 || len(script)%2 != 0 {
		return false
	}
	for _, c := range []byte(script) {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
