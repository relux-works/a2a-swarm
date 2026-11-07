package voice

import (
	"os"
	"os/exec"
)

// Status is a read-only local observation, not a synthesis or delivery probe.
// Values are closed tokens: paths, file contents and underlying errors are not
// diagnostics. No attempt history is persisted by the voice path.
type Status struct {
	Config      string `json:"config"`
	Native      string `json:"native"`
	Model       string `json:"model"`
	Encoder     string `json:"encoder"`
	Synthesis   string `json:"synthesis"`
	LastAttempt string `json:"last_attempt"`
	MaxSeconds  int    `json:"max_seconds"`
}

// Inspect never enables voice, installs assets, runs a process, opens a native
// engine, or contacts a service. Presence alone does not authenticate model
// assets or establish ffmpeg codec support.
func Inspect(base string) Status {
	s := Status{Config: "unavailable", Native: "unavailable", Model: "unavailable", Encoder: "unavailable", Synthesis: "not_checked", LastAttempt: "not_recorded", MaxSeconds: MaxSeconds}
	if nativeSynthesis {
		s.Native = "compiled"
	}
	st, err := os.Lstat(ConfigPath(base))
	switch {
	case os.IsNotExist(err):
		s.Config = "missing"
	case err != nil:
	case !st.Mode().IsRegular():
		s.Config = "invalid"
	default:
		if _, err := Load(base); err == nil {
			s.Config = "enabled"
		} else {
			s.Config = "invalid"
		}
	}
	st, err = os.Lstat(ModelDir(base))
	switch {
	case os.IsNotExist(err):
		s.Model = "missing"
	case err != nil:
	case !st.IsDir():
		s.Model = "invalid"
	default:
		s.Model = "present_unverified"
	}
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		s.Encoder = "found_unverified"
	}
	return s
}
