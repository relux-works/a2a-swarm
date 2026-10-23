package cli

import (
	"github.com/relux-works/a2a-swarm/internal/voice"
	"os"
	"testing"
)

func removeVoiceConfig(t *testing.T, base string) {
	t.Helper()
	if err := os.Remove(voice.ConfigPath(base)); err != nil {
		t.Fatal(err)
	}
}

// Enabling is explicit; missing/cloud/picker options cannot enable or change
// the previous local config. Configuration never contacts Matrix.
func TestVoiceConfigLocalOnly(t *testing.T) {
	e := newEnv(t)
	e.run("voice", "config").failsWith(t, ExitUsage, "explicit --enable")
	if _, err := voice.Load(e.dir); err == nil {
		t.Fatal("implicitly enabled voice")
	}
	e.run("voice", "config", "--enable").ok(t)
	c, err := voice.Load(e.dir)
	if err != nil || !c.Enabled || c.MaxSeconds != 180 {
		t.Fatalf("config=%+v err=%v", c, err)
	}
	for _, args := range [][]string{{"--engine", "cloud"}, {"--voice", "Milena"}, {"--enable=false"}, {}} {
		r := e.run(append([]string{"voice", "config"}, args...)...)
		if r.code != ExitUsage {
			t.Fatalf("accepted invalid option: %+v", r)
		}
		c, err := voice.Load(e.dir)
		if err != nil || !c.Enabled {
			t.Fatalf("changed config=%+v %v", c, err)
		}
	}
	if len(e.hs.Calls()) != 0 {
		t.Fatal("local config used Matrix")
	}
}
