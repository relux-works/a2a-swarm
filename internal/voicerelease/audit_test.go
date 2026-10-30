package voicerelease

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func stringCommand(kind uint32, name string, header int) []byte {
	b := make([]byte, (header+len(name)+1+7)&^7)
	binary.LittleEndian.PutUint32(b, kind)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)))
	binary.LittleEndian.PutUint32(b[8:], uint32(header))
	copy(b[header:], name)
	return b
}
func machoFixture(t *testing.T, name string, commands [][]byte) string {
	t.Helper()
	b := make([]byte, 32)
	for i, v := range []uint32{0xfeedfacf, 0x100000c, 0, 2, uint32(len(commands))} {
		binary.LittleEndian.PutUint32(b[i*4:], v)
	}
	size := 0
	for _, c := range commands {
		size += len(c)
		b = append(b, c...)
	}
	binary.LittleEndian.PutUint32(b[20:], uint32(size))
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func validCommands(helper bool, name string) [][]byte {
	uuid := make([]byte, 24)
	binary.LittleEndian.PutUint32(uuid, 0x1b)
	binary.LittleEndian.PutUint32(uuid[4:], 24)
	commands := [][]byte{uuid, stringCommand(0xc, "/usr/lib/libSystem.B.dylib", 24)}
	if helper {
		return append(commands, stringCommand(0xc, "@rpath/libonnxruntime.dylib", 24), stringCommand(0xc, "@rpath/libsherpa-onnx-c-api.dylib", 24), stringCommand(0x8000001c, releaseRpath, 12), stringCommand(0xe, "/usr/lib/dyld", 12))
	}
	return append(commands, stringCommand(0xd, "@rpath/"+name, 24))
}

// Actual Mach-O images prove the audit admits worker and both dylib forms and
// refuses every hidden dependency command, foreign path, rpath and absent UUID.
func TestAuditLoadCommands(t *testing.T) {
	for _, name := range []string{"a2a-voice-worker", "libonnxruntime.dylib", "libsherpa-onnx-c-api.dylib"} {
		helper := name == "a2a-voice-worker"
		for _, variant := range []string{"valid", "weak", "reexport", "upward", "lazy", "foreign", "system-lookalike", "rpath", "unknown", "id", "dyld", "environment", "uuid"} {
			if variant == "uuid" && !helper {
				continue
			}
			t.Run(name+"/"+variant, func(t *testing.T) {
				commands := validCommands(helper, name)
				kinds := map[string]uint32{"weak": 0x80000018, "reexport": 0x8000001f, "upward": 0x80000023, "lazy": 0x20, "unknown": 0x777, "environment": 0x27}
				if kind := kinds[variant]; kind != 0 {
					commands = append(commands, stringCommand(kind, "@rpath/libonnxruntime.dylib", 24))
				}
				switch variant {
				case "foreign":
					commands = append(commands, stringCommand(0xc, "/Users/build/libnative.dylib", 24))
				case "system-lookalike":
					commands = append(commands, stringCommand(0xc, "/usr/lib/unapproved.dylib", 24))
				case "rpath":
					commands = append(commands, stringCommand(0x8000001c, "/Users/build/lib", 12))
				case "id":
					commands = append(commands, stringCommand(0xd, "/Users/build/library.dylib", 24))
				case "dyld":
					commands = append(commands, stringCommand(0xe, "/Users/build/dyld", 12))
				case "uuid":
					commands = commands[1:]
				}
				err := Audit(machoFixture(t, name, commands), helper)
				if (variant == "valid") != (err == nil) {
					t.Fatalf("%s: %v", variant, err)
				}
			})
		}
	}
}
