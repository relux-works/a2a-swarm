package e2ee

import (
	"fmt"
	"io"
)

// formatLabel ignores verbs, flags, width and precision: even %x and %q must
// emit the fixed label, never an encoding of private fields. JSON storage is
// separate and intentionally does not use fmt methods.
func formatLabel(state fmt.State, label string) { _, _ = io.WriteString(state, label) }

func (Working) String() string                   { return "<e2ee-working>" }
func (v Working) GoString() string               { return v.String() }
func (v Working) Format(state fmt.State, _ rune) { formatLabel(state, v.String()) }

func (Engine) String() string                   { return "<e2ee-engine>" }
func (v Engine) GoString() string               { return v.String() }
func (v Engine) Format(state fmt.State, _ rune) { formatLabel(state, v.String()) }

func (Snapshot) String() string                   { return "<e2ee-snapshot>" }
func (v Snapshot) GoString() string               { return v.String() }
func (v Snapshot) Format(state fmt.State, _ rune) { formatLabel(state, v.String()) }

func (Prepared) String() string                   { return "<e2ee-prepared>" }
func (v Prepared) GoString() string               { return v.String() }
func (v Prepared) Format(state fmt.State, _ rune) { formatLabel(state, v.String()) }

func (Cursor) String() string                   { return "<e2ee-cursor>" }
func (v Cursor) GoString() string               { return v.String() }
func (v Cursor) Format(state fmt.State, _ rune) { formatLabel(state, v.String()) }

func (passwordRecord) String() string                   { return "<e2ee-password-record>" }
func (v passwordRecord) GoString() string               { return v.String() }
func (v passwordRecord) Format(state fmt.State, _ rune) { formatLabel(state, v.String()) }

func (FileCustody) String() string                   { return "<e2ee-file-custody>" }
func (v FileCustody) GoString() string               { return v.String() }
func (v FileCustody) Format(state fmt.State, _ rune) { formatLabel(state, v.String()) }

func (FileStore) String() string                   { return "<e2ee-file-store>" }
func (v FileStore) GoString() string               { return v.String() }
func (v FileStore) Format(state fmt.State, _ rune) { formatLabel(state, v.String()) }

func (LockedStore) String() string                   { return "<e2ee-locked-store>" }
func (v LockedStore) GoString() string               { return v.String() }
func (v LockedStore) Format(state fmt.State, _ rune) { formatLabel(state, v.String()) }

func (v Root) Format(state fmt.State, _ rune)        { formatLabel(state, v.String()) }
func (v EngineState) Format(state fmt.State, _ rune) { formatLabel(state, v.String()) }
func (v Codec) Format(state fmt.State, _ rune)       { formatLabel(state, "<e2ee-codec>") }

func (PinRecord) String() string                   { return "<e2ee-pin-record>" }
func (v PinRecord) GoString() string               { return v.String() }
func (v PinRecord) Format(state fmt.State, _ rune) { formatLabel(state, v.String()) }

// Embedded value methods protect Inventory values too, without copying its
// mutex into a formatter receiver. Callers must never copy an in-use Inventory.
type inventoryFormat struct{}

func (inventoryFormat) String() string                   { return "<e2ee-secret-inventory>" }
func (v inventoryFormat) GoString() string               { return v.String() }
func (v inventoryFormat) Format(state fmt.State, _ rune) { formatLabel(state, v.String()) }

func (jsonScan) String() string                   { return "<e2ee-json-scan>" }
func (v jsonScan) GoString() string               { return v.String() }
func (v jsonScan) Format(state fmt.State, _ rune) { formatLabel(state, v.String()) }
