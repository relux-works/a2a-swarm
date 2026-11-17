//go:build linux

package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSourceOriginGenerationPlants proves five generation regressions fail named assertions between passing unchanged-source controls without editing source files.
func TestSourceOriginGenerationPlants(t *testing.T) {
	if os.Getenv("A2A_RUN_ORIGIN_PLANTS") != "1" {
		t.Skip("opt-in generation plants")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	const tests = "^Test(ValidGeneration|SourceOriginGeneration(Wire|Authority|Configuration))$"
	plants := []struct{ name, file, from, to, witness string }{
		{"case-folding", "internal/daemon/origin_linux.go",
			"if !runorigin.ValidGeneration(generation) || generation != s.cfg.Launcher.Generation {",
			"generation = strings.ToLower(generation)\n\tif !runorigin.ValidGeneration(generation) || generation != s.cfg.Launcher.Generation {",
			"TestSourceOriginGenerationAuthority/uppercase"},
		{"all-zero", "runorigin/origin.go",
			` && s != "00000000000000000000000000000000"`, "",
			"TestSourceOriginGenerationConfiguration/zero"},
		{"json-number", "internal/daemon/origin_codec.go",
			`if r.input.Generation, ok = originString(p["generation"]); !ok || !runorigin.ValidGeneration(r.input.Generation) {`,
			`r.input.Generation, ok = originString(p["generation"])
	var legacy uint64
	if json.Unmarshal(p["generation"], &legacy) == nil && legacy != 0 {
		r.input.Generation, ok = "11111111111111111111111111111111", true
	}
	if !ok || !runorigin.ValidGeneration(r.input.Generation) {`,
			"TestSourceOriginGenerationWire/number-integer"},
		{"length-off-by-one", "runorigin/origin.go",
			`return LowerHex(s, 32) &&`, `return (LowerHex(s, 32) || LowerHex(s, 31)) &&`,
			"TestSourceOriginGenerationWire/short"},
		{"authority-validation", "internal/daemon/origin_linux.go",
			"!runorigin.ValidGeneration(generation) || ", "",
			"TestSourceOriginGenerationAuthority/matching-invalid-authority"},
	}
	for _, plant := range plants {
		t.Run(plant.name, func(t *testing.T) {
			source := filepath.Join(root, plant.file)
			original, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(original), plant.from) != 1 {
				t.Fatal("plant no longer applies exactly once")
			}
			control := func(phase string) {
				t.Helper()
				output, err := originPlantCommand(root, "test", "./runorigin", "./internal/daemon", "-run", tests, "-count=1", "-v")
				if err != nil {
					t.Fatalf("%s control failed: %v\n%s", phase, err, output)
				}
				t.Logf("%s=%s sha256=%x exit=0\n%s", phase, plant.name, sha256.Sum256(original), output)
			}
			control("before")
			mutated := bytes.Replace(original, []byte(plant.from), []byte(plant.to), 1)
			dir := t.TempDir()
			replacement := filepath.Join(dir, filepath.Base(source))
			if err := os.WriteFile(replacement, mutated, 0600); err != nil {
				t.Fatal(err)
			}
			overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{source: replacement}})
			if err != nil {
				t.Fatal(err)
			}
			overlayPath := filepath.Join(dir, "overlay.json")
			if err := os.WriteFile(overlayPath, overlay, 0600); err != nil {
				t.Fatal(err)
			}
			output, err := originPlantCommand(root, "test", "-overlay", overlayPath, "./runorigin", "./internal/daemon", "-run", tests, "-count=1", "-v")
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 1 || !strings.Contains(string(output), "--- FAIL: "+plant.witness) || bytes.Contains(output, []byte("panic:")) || bytes.Contains(output, []byte("build failed")) {
				t.Fatalf("plant needs intended named failure, err=%v\n%s", err, output)
			}
			t.Logf("mutant=%s sha256=%x exit=1 killed-by=%s\n%s", plant.name, sha256.Sum256(mutated), plant.witness, output)
			control("after")
			after, err := os.ReadFile(source)
			if err != nil || !bytes.Equal(after, original) {
				t.Fatal("plant changed original source")
			}
		})
	}
}
