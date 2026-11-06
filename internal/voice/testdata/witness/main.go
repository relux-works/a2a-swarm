// Executable test double for the worker and ffmpeg. This is never a release payload.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var buildVersion = "intact"

func must(err error) {
	if err != nil {
		os.Exit(7)
	}
}
func main() {
	args := os.Args[1:]
	if filepath.Base(os.Args[0]) == "ffmpeg" {
		out := args[len(args)-1]
		b := bytes.Repeat([]byte{1, 0}, 24000)
		if strings.HasSuffix(out, ".ogg") {
			b = []byte("OggS fixture OpusHead")
		}
		must(os.WriteFile(out, b, 0600))
		return
	}
	if len(args) != 2 {
		os.Exit(7)
	}
	must(func() error { _, err := io.Copy(io.Discard, os.Stdin); return err }())
	exe, err := os.Executable()
	must(err)
	// Reading both siblings witnesses the private payload actually used at exec.
	libraries := []string{}
	for _, name := range []string{"libonnxruntime.dylib", "libsherpa-onnx-c-api.dylib"} {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "lib", name))
		must(err)
		libraries = append(libraries, string(b))
	}
	marker, err := os.OpenFile(filepath.Join(args[0], "execution-marker"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	must(err)
	must(json.NewEncoder(marker).Encode(struct {
		Executable  string
		Libraries   []string
		Version     string
		Environment []string
	}{exe, libraries, buildVersion, os.Environ()}))
	must(marker.Close())
	// A source change after the first invocation must be rechecked for the notice.
	var change struct{ From, To string }
	b, err := os.ReadFile(filepath.Join(args[0], "after-first.json"))
	if err == nil {
		must(json.Unmarshal(b, &change))
		must(os.Rename(change.From, change.To))
		must(os.Remove(filepath.Join(args[0], "after-first.json")))
	}
	must(os.WriteFile(args[1], []byte("fixture source audio"), 0600))
}
