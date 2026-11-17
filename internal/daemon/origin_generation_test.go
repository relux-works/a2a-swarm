//go:build linux

package daemon

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/runorigin"
)

func originGenerationState(t *testing.T, root string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		state[name] = info.Mode().String()
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			state[name] += ":" + string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func unchangedOriginGenerationState(t *testing.T, dir string, before map[string]string, p *originProducer) {
	t.Helper()
	if p.opens.Load() != 0 || p.prepares.Load() != 0 || p.validates.Load() != 0 || p.closes.Load() != 0 {
		t.Fatal("generation refusal reached producer/session work")
	}
	if after := originGenerationState(t, dir); !reflect.DeepEqual(after, before) {
		t.Fatal("generation refusal changed control-directory state")
	}
}

// TestSourceOriginGenerationWire proves invalid generation types/spellings close with no prepared result, authority re-read, producer call or control-directory change.
func TestSourceOriginGenerationWire(t *testing.T) {
	t.Run("valid", validOriginControl)
	cases := map[string]any{
		"number-zero": json.RawMessage(`0`), "number-integer": json.RawMessage(`19`),
		"number-negative": json.RawMessage(`-1`), "number-fraction": json.RawMessage(`1.5`),
		"number-exponent": json.RawMessage(`1e2`), "number-max-uint64": json.RawMessage(`18446744073709551615`),
		"number-128-bit": json.RawMessage(`11111111111111111111111111111111`),
		"uppercase":      strings.ToUpper(inputGeneration), "mixed-case": "Fedcba9876543210fedcba9876543210",
		"short": inputGeneration[:31], "long": inputGeneration + "0",
		"non-hex": inputGeneration[:31] + "g", "zero": strings.Repeat("0", 32),
		"empty": "", "null": nil, "boolean": true, "object": map[string]any{}, "array": []any{},
	}
	for name, generation := range cases {
		t.Run(name, func(t *testing.T) {
			p := fakeOriginProducer()
			f := startOrigin(t, p, nil)
			c := f.client(t)
			before := originGenerationState(t, f.dir)
			calls := f.generation.calls.Load()
			params := prepareParams()
			params["input_principal"] = map[string]any{"uid": 4242, "generation": generation}
			sendOrigin(t, c, originPrepare, params)
			wantOriginCode(t, c, runorigin.Refused, f.s.incarnation)
			if f.generation.calls.Load() != calls {
				t.Fatal("invalid generation reached current authority after hello")
			}
			unchangedOriginGenerationState(t, f.dir, before, p)
		})
	}
}

// TestSourceOriginGenerationAuthority proves one-character mismatch and invalid authority spellings refuse hello/prepare without prepared results, producer calls or control-directory changes.
func TestSourceOriginGenerationAuthority(t *testing.T) {
	t.Run("valid", validOriginControl)
	cases := map[string]string{
		"one-character-mismatch": nextLauncherGeneration,
		"uppercase":              strings.ToUpper(launcherGeneration),
		"mixed-case":             "0123456789abcdef0123456789abcdeF",
		"zero":                   strings.Repeat("0", 32), "empty": "",
		"short": launcherGeneration[:31], "long": launcherGeneration + "0",
		"non-hex": launcherGeneration[:31] + "g",
	}
	for name, generation := range cases {
		for _, phase := range []string{"hello", "prepare"} {
			t.Run(name+"/"+phase, func(t *testing.T) {
				p := fakeOriginProducer()
				f := startOrigin(t, p, nil)
				if phase == "hello" {
					f.generation.generation.Store(generation)
					before := originGenerationState(t, f.dir)
					wantOriginCode(t, f.dial(t), runorigin.Refused, f.s.incarnation)
					unchangedOriginGenerationState(t, f.dir, before, p)
				} else {
					c := f.client(t)
					f.generation.generation.Store(generation)
					before := originGenerationState(t, f.dir)
					sendOrigin(t, c, originPrepare, prepareParams())
					wantOriginCode(t, c, runorigin.Refused, f.s.incarnation)
					unchangedOriginGenerationState(t, f.dir, before, p)
				}
			})
		}
	}
	// A valid immutable launcher pin makes every malformed authority unequal.
	// Bypass construction only in this fixture, before any goroutine starts, to
	// prove authority validation independently of that equality/constructor gate.
	t.Run("matching-invalid-authority", func(t *testing.T) {
		p := fakeOriginProducer()
		f := startOriginBeforeServe(t, p, nil, func(s *OriginService) {
			s.cfg.Launcher.Generation = strings.Repeat("0", 32)
			s.cfg.Generations.(*originAuthority).generation.Store(s.cfg.Launcher.Generation)
		})
		before := originGenerationState(t, f.dir)
		wantOriginCode(t, f.dial(t), runorigin.Refused, f.s.incarnation)
		if f.generation.calls.Load() != 1 {
			t.Fatal("authority validation fixture did not reach current authority")
		}
		unchangedOriginGenerationState(t, f.dir, before, p)
	})
}

// TestSourceOriginGenerationConfiguration proves malformed launcher generations return no service and leave authority, producer and control-directory state untouched.
func TestSourceOriginGenerationConfiguration(t *testing.T) {
	t.Run("valid", validOriginControl)
	for name, generation := range map[string]string{
		"zero": strings.Repeat("0", 32), "empty": "",
		"uppercase":  strings.ToUpper(launcherGeneration),
		"mixed-case": "0123456789abcdef0123456789abcdeF",
		"short":      launcherGeneration[:31], "long": launcherGeneration + "0",
		"non-hex": launcherGeneration[:31] + "g",
	} {
		t.Run(name, func(t *testing.T) {
			dir := shortOriginDir(t)
			p := fakeOriginProducer()
			g := &originAuthority{}
			g.generation.Store(launcherGeneration)
			cfg := OriginConfig{ControlDir: dir, Launcher: runorigin.Principal{UID: uint32(os.Geteuid()) + 1, Generation: generation}, Generations: g, Producer: p}
			before := originGenerationState(t, dir)
			service, err := NewOriginService(cfg)
			if service != nil || !errors.Is(err, runorigin.ErrRefused) {
				t.Fatalf("invalid launcher started a service: service=%v err=%v", service != nil, err)
			}
			if g.calls.Load() != 0 {
				t.Fatal("invalid launcher reached authority")
			}
			unchangedOriginGenerationState(t, dir, before, p)
		})
	}
}
