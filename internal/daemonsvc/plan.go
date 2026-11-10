// Package daemonsvc owns the host service boundary, never the monitor pipeline.
package daemonsvc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/config"
)

var ErrRefused = errors.New("daemon: not_allowed")
var ErrUnresolved = errors.New("daemon: daemon_unavailable; service unresolved")

const Label = "works.relux.a2a.a2ad"
const HeartbeatSeconds = 30
const StaleSeconds = 90

// Plan is public installation data. No credentials, environment or sockets can
// be supplied. The operator pins both the executable and the immutable policy.
type Plan struct {
	Version      int    `json:"version"`
	Platform     string `json:"platform"`
	Image        string `json:"image"`
	ImageSHA256  string `json:"image_sha256"`
	Home         string `json:"home"`
	Policy       string `json:"owner_pin"`
	PolicySHA256 string `json:"owner_pin_sha256"`
	UID          uint32 `json:"uid"`
	GID          uint32 `json:"gid"`
	SweepSeconds int    `json:"sweep_seconds"`
	SoakDays     int    `json:"soak_days"`
}

var pathRE = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)
var hashRE = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validPath(s string) bool { return s != "/" && pathRE.MatchString(s) && filepath.Clean(s) == s }
func DecodePlan(b []byte) (Plan, error) {
	var p Plan
	// The shared strict decoder rejects duplicate, case-folded and unknown keys.
	if err := strictJSON(b, &p); err != nil {
		return p, ErrRefused
	}
	return p, p.Validate()
}
func strictJSON(b []byte, v any) error {
	// Reuse the bounded repository decoder for duplicate keys, unknown fields,
	// excessive nesting and trailing data. Check exact top-level spelling below.
	if config.DecodeConnectionJSON(b, v) != nil {
		return ErrRefused
	}
	encoded, _ := json.Marshal(v)
	var a, c map[string]json.RawMessage
	if json.Unmarshal(b, &a) != nil || json.Unmarshal(encoded, &c) != nil {
		return ErrRefused
	}
	for k, val := range a {
		if _, ok := c[k]; !ok || bytes.Equal(bytes.TrimSpace(val), []byte("null")) {
			return ErrRefused
		}
	}
	return nil
}
func (p Plan) Validate() error {
	if p.Version != 1 || (p.Platform != "darwin" && p.Platform != "linux") || !validPath(p.Image) || !validPath(p.Home) || !validPath(p.Policy) || !hashRE.MatchString(p.ImageSHA256) || !hashRE.MatchString(p.PolicySHA256) || p.UID == 0 || p.GID == 0 || p.SweepSeconds < 30 || p.SweepSeconds > 86400 || p.SoakDays < 1 || p.SoakDays > 365 {
		return ErrRefused
	}
	if p.Platform == "darwin" && (p.UID != 401 || p.GID != 401) {
		return ErrRefused
	}
	return nil
}
func Digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func (p Plan) SocketDir() string {
	if p.Platform == "darwin" {
		return "/private/var/run/a2a"
	}
	return "/run/a2a"
}
func (p Plan) UnitDir() string {
	if p.Platform == "darwin" {
		return "/Library/LaunchDaemons"
	}
	return "/etc/systemd/system"
}
func (p Plan) User() string {
	if p.Platform == "darwin" {
		return "_a2a"
	}
	return "a2a"
}
func (p Plan) args(command string) []string {
	return []string{p.Image, "daemon", command, "--home", p.Home, "--owner-pin", p.Policy, "--owner-pin-sha256", p.PolicySHA256, "--image-sha256", p.ImageSHA256, "--service-gid", fmt.Sprint(p.GID)}
}

// Render is the only unit grammar. ValidateUnits compares against this closed
// grammar, so secrets, extra sockets, argv or directives always fail closed.
func Render(p Plan) (map[string][]byte, error) {
	if p.Validate() != nil {
		return nil, ErrRefused
	}
	files := map[string][]byte{}
	if p.Platform == "darwin" {
		plist := func(label string, args []string, extra string, user string) []byte {
			var b strings.Builder
			b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict>\n")
			fmt.Fprintf(&b, "<key>Label</key><string>%s</string>\n<key>UserName</key><string>%s</string>\n<key>ProgramArguments</key><array>", label, user)
			for _, a := range args {
				fmt.Fprintf(&b, "<string>%s</string>", a)
			}
			b.WriteString("</array>\n<key>ThrottleInterval</key><integer>30</integer>\n<key>ExitTimeOut</key><integer>10</integer>\n")
			b.WriteString(extra)
			b.WriteString("</dict></plist>\n")
			return []byte(b.String())
		}
		files[Label+".plist"] = plist(Label, p.args("serve"), "<key>RunAtLoad</key><true/>\n<key>KeepAlive</key><true/>\n", p.User())
		files[Label+".prepare.plist"] = plist(Label+".prepare", []string{p.Image, "daemon", "prepare-run-dir", "--image-sha256", p.ImageSHA256}, "<key>RunAtLoad</key><true/>\n<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>\n", "root")
		files[Label+".sweep.plist"] = plist(Label+".sweep", p.args("sweep"), fmt.Sprintf("<key>RunAtLoad</key><true/>\n<key>StartInterval</key><integer>%d</integer>\n", p.SweepSeconds), p.User())
	} else {
		files[Label+".service"] = []byte("[Unit]\nDescription=a2a daemon\nAfter=network.target\n[Service]\nType=simple\nUser=a2a\nGroup=a2a\nRuntimeDirectory=a2a\nRuntimeDirectoryMode=0711\nRuntimeDirectoryPreserve=yes\nRestart=on-failure\nRestartSec=30\nTimeoutStopSec=10\nUMask=0077\nExecStart=" + strings.Join(p.args("serve"), " ") + "\n[Install]\nWantedBy=multi-user.target\n")
		files[Label+".sweep.service"] = []byte("[Unit]\nDescription=a2ad read-only sweep\n[Service]\nType=oneshot\nUser=a2a\nGroup=a2a\nUMask=0077\nExecStart=" + strings.Join(p.args("sweep"), " ") + "\n")
		files[Label+".sweep.timer"] = []byte(fmt.Sprintf("[Unit]\nDescription=a2ad sweep cadence\n[Timer]\nOnBootSec=30\nOnUnitActiveSec=%ds\nAccuracySec=1s\nUnit=%s.sweep.service\n[Install]\nWantedBy=timers.target\n", p.SweepSeconds, Label))
	}
	return files, nil
}
func ValidateUnits(p Plan, files map[string][]byte) error {
	want, e := Render(p)
	if e != nil || len(want) != len(files) {
		return ErrRefused
	}
	for name, b := range want {
		if !bytes.Equal(b, files[name]) {
			return ErrRefused
		}
	}
	return nil
}
