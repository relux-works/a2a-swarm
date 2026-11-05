package tuitestkit

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// ScrollDir represents a mouse scroll direction.
type ScrollDir int

const (
	// ScrollUp scrolls up.
	ScrollUp ScrollDir = iota
	// ScrollDown scrolls down.
	ScrollDown
	// ScrollLeft scrolls left.
	ScrollLeft
	// ScrollRight scrolls right.
	ScrollRight
)

// specialKeyMap maps user-friendly key names to bubbletea KeyType constants.
var specialKeyMap = map[string]tea.KeyType{
	"enter":     tea.KeyEnter,
	"tab":       tea.KeyTab,
	"esc":       tea.KeyEsc,
	"escape":    tea.KeyEscape,
	"space":     tea.KeySpace,
	"backspace": tea.KeyBackspace,
	"up":        tea.KeyUp,
	"down":      tea.KeyDown,
	"left":      tea.KeyLeft,
	"right":     tea.KeyRight,
	"home":      tea.KeyHome,
	"end":       tea.KeyEnd,
	"pgup":      tea.KeyPgUp,
	"pgdown":    tea.KeyPgDown,
	"delete":    tea.KeyDelete,
	"insert":    tea.KeyInsert,
	"shift+tab": tea.KeyShiftTab,

	// F-keys
	"f1":  tea.KeyF1,
	"f2":  tea.KeyF2,
	"f3":  tea.KeyF3,
	"f4":  tea.KeyF4,
	"f5":  tea.KeyF5,
	"f6":  tea.KeyF6,
	"f7":  tea.KeyF7,
	"f8":  tea.KeyF8,
	"f9":  tea.KeyF9,
	"f10": tea.KeyF10,
	"f11": tea.KeyF11,
	"f12": tea.KeyF12,
	"f13": tea.KeyF13,
	"f14": tea.KeyF14,
	"f15": tea.KeyF15,
	"f16": tea.KeyF16,
	"f17": tea.KeyF17,
	"f18": tea.KeyF18,
	"f19": tea.KeyF19,
	"f20": tea.KeyF20,
}

// ctrlKeyMap maps ctrl+letter combos to bubbletea KeyType constants.
var ctrlKeyMap = map[string]tea.KeyType{
	"ctrl+a":  tea.KeyCtrlA,
	"ctrl+b":  tea.KeyCtrlB,
	"ctrl+c":  tea.KeyCtrlC,
	"ctrl+d":  tea.KeyCtrlD,
	"ctrl+e":  tea.KeyCtrlE,
	"ctrl+f":  tea.KeyCtrlF,
	"ctrl+g":  tea.KeyCtrlG,
	"ctrl+h":  tea.KeyCtrlH,
	"ctrl+i":  tea.KeyCtrlI,
	"ctrl+j":  tea.KeyCtrlJ,
	"ctrl+k":  tea.KeyCtrlK,
	"ctrl+l":  tea.KeyCtrlL,
	"ctrl+m":  tea.KeyCtrlM,
	"ctrl+n":  tea.KeyCtrlN,
	"ctrl+o":  tea.KeyCtrlO,
	"ctrl+p":  tea.KeyCtrlP,
	"ctrl+q":  tea.KeyCtrlQ,
	"ctrl+r":  tea.KeyCtrlR,
	"ctrl+s":  tea.KeyCtrlS,
	"ctrl+t":  tea.KeyCtrlT,
	"ctrl+u":  tea.KeyCtrlU,
	"ctrl+v":  tea.KeyCtrlV,
	"ctrl+w":  tea.KeyCtrlW,
	"ctrl+x":  tea.KeyCtrlX,
	"ctrl+y":  tea.KeyCtrlY,
	"ctrl+z":  tea.KeyCtrlZ,
	"ctrl+@":  tea.KeyCtrlAt,
	"ctrl+[":  tea.KeyCtrlOpenBracket,
	"ctrl+\\": tea.KeyCtrlBackslash,
	"ctrl+]":  tea.KeyCtrlCloseBracket,
	"ctrl+^":  tea.KeyCtrlCaret,
	"ctrl+_":  tea.KeyCtrlUnderscore,
	"ctrl+?":  tea.KeyCtrlQuestionMark,
}

// Key builds a tea.KeyMsg from a human-readable string.
//
// Supported formats:
//   - Special keys: "enter", "tab", "esc", "space", "backspace", "up", "down",
//     "left", "right", "home", "end", "pgup", "pgdown", "delete", "insert"
//   - F-keys: "f1" through "f20"
//   - Ctrl combos: "ctrl+c", "ctrl+a", "ctrl+z" etc.
//   - Alt combos: "alt+h", "alt+enter", "alt+a"
//   - Single runes: "a", "b", "1", "/", etc.
func Key(k string) tea.KeyMsg {
	lower := strings.ToLower(k)

	// Handle alt+... prefix
	if strings.HasPrefix(lower, "alt+") {
		inner := lower[4:] // strip "alt+"
		msg := resolveKey(inner)
		msg.Alt = true
		return msg
	}

	return resolveKey(lower)
}

// resolveKey resolves a key string (without alt prefix) to a tea.KeyMsg.
func resolveKey(k string) tea.KeyMsg {
	// Check ctrl combos first
	if kt, ok := ctrlKeyMap[k]; ok {
		return tea.KeyMsg{Type: kt}
	}

	// Check special keys
	if kt, ok := specialKeyMap[k]; ok {
		// Space is a special case: bubbletea uses KeySpace type with a space rune
		if kt == tea.KeySpace {
			return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
		}
		return tea.KeyMsg{Type: kt}
	}

	// Single rune
	runes := []rune(k)
	if len(runes) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: runes}
	}

	// Fallback: treat the whole string as runes (multi-rune input)
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: runes}
}

// Keys builds a slice of tea.Msg from multiple key strings.
// This is a convenience for sending a sequence of keypresses to a model's Update.
//
// Example:
//
//	msgs := Keys("h", "e", "l", "l", "o", "enter")
//	for _, msg := range msgs {
//	    m, _ = m.Update(msg)
//	}
func Keys(keys ...string) []tea.Msg {
	msgs := make([]tea.Msg, len(keys))
	for i, k := range keys {
		msgs[i] = Key(k)
	}
	return msgs
}

// WindowSize builds a tea.WindowSizeMsg with the given dimensions.
func WindowSize(w, h int) tea.WindowSizeMsg {
	return tea.WindowSizeMsg{
		Width:  w,
		Height: h,
	}
}

// MouseClick builds a tea.MouseMsg for a left-button click (press) at (x, y).
func MouseClick(x, y int) tea.MouseMsg {
	return tea.MouseMsg{
		X:      x,
		Y:      y,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
		Type:   tea.MouseLeft,
	}
}

// MouseClickRight builds a tea.MouseMsg for a right-button click (press) at (x, y).
func MouseClickRight(x, y int) tea.MouseMsg {
	return tea.MouseMsg{
		X:      x,
		Y:      y,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonRight,
		Type:   tea.MouseRight,
	}
}

// scrollDirToButton maps ScrollDir to the corresponding mouse button.
var scrollDirToButton = map[ScrollDir]tea.MouseButton{
	ScrollUp:    tea.MouseButtonWheelUp,
	ScrollDown:  tea.MouseButtonWheelDown,
	ScrollLeft:  tea.MouseButtonWheelLeft,
	ScrollRight: tea.MouseButtonWheelRight,
}

// scrollDirToType maps ScrollDir to the deprecated MouseEventType for backwards compat.
var scrollDirToType = map[ScrollDir]tea.MouseEventType{
	ScrollUp:    tea.MouseWheelUp,
	ScrollDown:  tea.MouseWheelDown,
	ScrollLeft:  tea.MouseWheelLeft,
	ScrollRight: tea.MouseWheelRight,
}

// MouseScroll builds a tea.MouseMsg for a scroll event in the given direction.
// The position defaults to (0, 0).
func MouseScroll(dir ScrollDir) tea.MouseMsg {
	return tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: scrollDirToButton[dir],
		Type:   scrollDirToType[dir],
	}
}

// MouseRelease builds a tea.MouseMsg for a mouse button release at (x, y).
func MouseRelease(x, y int) tea.MouseMsg {
	return tea.MouseMsg{
		X:      x,
		Y:      y,
		Action: tea.MouseActionRelease,
		Button: tea.MouseButtonNone,
		Type:   tea.MouseRelease,
	}
}
