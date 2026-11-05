package tmuxwake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type RuntimeState string
type RuntimeReason string

const (
	RuntimeReady               RuntimeState  = "ready"
	RuntimeRefused             RuntimeState  = "refused"
	RuntimeUnavailable         RuntimeState  = "unavailable"
	RuntimeNone                RuntimeReason = "none"
	RuntimeTargetUnavailable   RuntimeReason = "target_unavailable"
	RuntimeRootUnavailable     RuntimeReason = "root_unavailable"
	RuntimeRegistryUnavailable RuntimeReason = "registry_unavailable"
	RuntimeIdentityMismatch    RuntimeReason = "identity_mismatch"
	RuntimeHeadless            RuntimeReason = "headless"
	RuntimeBusy                RuntimeReason = "busy"
	RuntimeStatusUnqualified   RuntimeReason = "status_unqualified"
	RuntimeVersionUnqualified  RuntimeReason = "version_unqualified"
	RuntimeSchemaUnqualified   RuntimeReason = "schema_unqualified"
)

// RuntimeStateFor is a closed projection. Stored telemetry never supplies
// readiness; unknown and historical reasons project unavailable.
func RuntimeStateFor(reason RuntimeReason) RuntimeState {
	switch reason {
	case RuntimeNone:
		return RuntimeReady
	case RuntimeIdentityMismatch, RuntimeHeadless, RuntimeBusy, RuntimeStatusUnqualified:
		return RuntimeRefused
	default:
		return RuntimeUnavailable
	}
}
func ValidRuntimeReason(reason RuntimeReason) bool {
	switch reason {
	case RuntimeNone, RuntimeTargetUnavailable, RuntimeRootUnavailable, RuntimeRegistryUnavailable, RuntimeIdentityMismatch, RuntimeHeadless, RuntimeBusy, RuntimeStatusUnqualified, RuntimeVersionUnqualified, RuntimeSchemaUnqualified:
		return true
	default:
		return false
	}
}

type RuntimeAssessment struct {
	State       RuntimeState  `json:"state"`
	Reason      RuntimeReason `json:"reason"`
	Version     string        `json:"version,omitempty"`
	UnknownKeys string        `json:"unknown_keys,omitempty"`
}

func runtimeResult(reason RuntimeReason, version string) RuntimeAssessment {
	return RuntimeAssessment{State: RuntimeStateFor(reason), Reason: reason, Version: SafeRegistryVersion(version)}
}

var registryVersion = regexp.MustCompile(`^[0-9]{1,5}\.[0-9]{1,5}\.[0-9]{1,5}$`)

func SafeRegistryVersion(version string) string {
	if registryVersion.MatchString(version) {
		return version
	}
	return ""
}

const maxRegistryBytes = 64 * 1024

type registryIdentity struct {
	PID             int
	Birth           time.Time
	SessionID, Pane string
}
type registryAssessment struct {
	RuntimeAssessment
	SessionID, Status string
	StatusAt          int64
}

// decodeObject rejects duplicate keys at every nesting level, bounded depth,
// trailing JSON and oversized records. It does not log any source values.
func decodeObject(data []byte) (map[string]any, error) {
	if len(data) > maxRegistryBytes {
		return nil, errors.New("registry unavailable")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		if depth > 16 {
			return nil, errors.New("registry unavailable")
		}
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		switch tok {
		case json.Delim('{'):
			m := map[string]any{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return nil, e
				}
				s, ok := k.(string)
				if !ok {
					return nil, errors.New("registry unavailable")
				}
				if _, exists := m[s]; exists {
					return nil, errors.New("registry unavailable")
				}
				v, e := read(depth + 1)
				if e != nil {
					return nil, e
				}
				m[s] = v
			}
			_, err = d.Token()
			return m, err
		case json.Delim('['):
			var a []any
			for d.More() {
				v, e := read(depth + 1)
				if e != nil {
					return nil, e
				}
				a = append(a, v)
			}
			_, err = d.Token()
			return a, err
		default:
			if _, ok := tok.(json.Delim); ok {
				return nil, errors.New("registry unavailable")
			}
			return tok, nil
		}
	}
	v, err := read(0)
	if err != nil {
		return nil, err
	}
	if _, e := d.Token(); e != io.EOF {
		return nil, errors.New("registry unavailable")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("registry unavailable")
	}
	return m, nil
}
func registryInt(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil && i >= 0
}
func recordString(m map[string]any, key string) string { s, _ := m[key].(string); return s }

var requiredRegistryKeys = map[string]string{"cwd": "s", "entrypoint": "s", "kind": "s", "messagingSocketPath": "s", "name": "s", "nameSince": "n", "nameSource": "s", "peerFeatures": "a", "peerProtocol": "n", "pid": "n", "pidDomain": "s", "procStart": "s", "sessionId": "s", "startedAt": "n", "status": "s", "statusUpdatedAt": "n", "updatedAt": "n", "version": "s"}

func registrySchema(m map[string]any) bool {
	for k := range requiredRegistryKeys {
		if _, ok := m[k]; !ok {
			return false
		}
	}
	for k, v := range m {
		switch requiredRegistryKeys[k] {
		case "s":
			if _, ok := v.(string); !ok {
				return false
			}
		case "n":
			if _, ok := registryInt(v); !ok {
				return false
			}
		case "a":
			a, ok := v.([]any)
			if !ok {
				return false
			}
			for _, x := range a {
				if _, ok := x.(string); !ok {
					return false
				}
			}
		default:
			switch k {
			case "asksForConvertedSkills":
				if _, ok := v.(bool); !ok {
					return false
				}
			case "tmux", "bridgeSessionId":
				if _, ok := v.(string); !ok {
					return false
				}
			case "formerNames":
				a, ok := v.([]any)
				if !ok {
					return false
				}
				for _, x := range a {
					o, ok := x.(map[string]any)
					if !ok || len(o) != 3 {
						return false
					}
					if _, ok := o["name"].(string); !ok {
						return false
					}
					if _, ok := o["sessionId"].(string); !ok {
						return false
					}
					if _, ok := registryInt(o["until"]); !ok {
						return false
					}
				}
			default:
				// Additional top-level fields do not change structural admission.
			}
		}
	}
	return true
}

// SafeRegistryUnknownKeys projects names only: at most 16 comma-separated
// ASCII tokens of at most 64 bytes. Replacement/truncation is deliberately lossy.
// This diagnostic must never supply readiness authority.
func SafeRegistryUnknownKeys(names string) string {
	var out []string
	for _, name := range strings.SplitN(names, ",", 17) {
		if name == "" {
			continue
		}
		var b strings.Builder
		for i := 0; i < len(name) && i < 64; i++ {
			c := name[i]
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.') {
				c = '_'
			}
			b.WriteByte(c)
		}
		out = append(out, b.String())
		if len(out) == 16 {
			break
		}
	}
	return strings.Join(out, ",")
}

func registryUnknownKeys(m map[string]any) string {
	var names []string
	for k := range m {
		if _, known := requiredRegistryKeys[k]; known {
			continue
		}
		switch k {
		case "asksForConvertedSkills", "tmux", "bridgeSessionId", "formerNames":
			continue
		}
		names = append(names, k)
	}
	sort.Strings(names)
	// Sanitize individually so a comma in a source name cannot create entries.
	for i, name := range names {
		if name == "" {
			name = "_"
		}
		names[i] = SafeRegistryUnknownKeys(strings.ReplaceAll(name, ",", "_"))
	}
	return SafeRegistryUnknownKeys(strings.Join(names, ","))
}

func parseUTCBirth(s string) (time.Time, error) {
	return time.Parse("Mon Jan 2 15:04:05 2006", strings.Join(strings.Fields(s), " "))
}

var registryPane = regexp.MustCompile(`^.+:@[0-9]+\.(%[0-9]+)$`)

func registryIdentityReason(m map[string]any, id registryIdentity) RuntimeReason {
	pid, ok := registryInt(m["pid"])
	birth, err := parseUTCBirth(recordString(m, "procStart"))
	if !ok || pid != int64(id.PID) || err != nil || !birth.Equal(id.Birth) || id.SessionID == "" || recordString(m, "sessionId") != id.SessionID {
		return RuntimeIdentityMismatch
	}
	if recordString(m, "entrypoint") != "cli" || recordString(m, "tmux") == "" {
		return RuntimeHeadless
	}
	pane := registryPane.FindStringSubmatch(recordString(m, "tmux"))
	if len(pane) != 2 || pane[1] != id.Pane {
		return RuntimeIdentityMismatch
	}
	if recordString(m, "pidDomain") != "darwin" {
		return RuntimeIdentityMismatch
	}
	return RuntimeNone
}
func decodeRegistry(data []byte, id registryIdentity) registryAssessment {
	unknown := ""
	result := func(reason RuntimeReason, version string) registryAssessment {
		r := registryAssessment{RuntimeAssessment: runtimeResult(reason, version)}
		r.UnknownKeys = unknown
		return r
	}
	m, err := decodeObject(data)
	if err != nil {
		return result(RuntimeRegistryUnavailable, "")
	}
	unknown = registryUnknownKeys(m)
	version := recordString(m, "version")
	// Missing/malformed string versions cannot identify a readiness key.
	// Present non-string versions remain schema violations.
	_, versionPresent := m["version"]
	_, versionString := m["version"].(string)
	if !versionPresent || (versionString && SafeRegistryVersion(version) == "") {
		return result(RuntimeVersionUnqualified, version)
	}
	if !registrySchema(m) {
		return result(RuntimeSchemaUnqualified, version)
	}
	if reason := registryIdentityReason(m, id); reason != RuntimeNone {
		return result(reason, version)
	}
	status := recordString(m, "status")
	reason := RuntimeNone
	switch status {
	case "idle", "shell":
	case "busy":
		reason = RuntimeBusy
	default:
		reason = RuntimeStatusUnqualified
	}
	r := result(reason, version)
	r.SessionID = id.SessionID
	r.Status = status
	r.StatusAt, _ = registryInt(m["statusUpdatedAt"])
	return r
}

func liveRegistryIdentity(ctx context.Context, t Target, session string) (registryIdentity, error) {
	id := registryIdentity{PID: t.OwnerPID, SessionID: session, Pane: t.Pane}
	if t.OwnerPID <= 1 || t.OwnerBirth == "" {
		return id, errors.New("registry identity unavailable")
	}
	birth, err := Birth(ctx, t.OwnerPID)
	if err != nil || birth != t.OwnerBirth {
		return id, errors.New("registry identity unavailable")
	}
	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(t.OwnerPID), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "TZ=UTC", "LC_ALL=C")
	b, err := cmd.Output()
	if err != nil {
		return id, errors.New("registry identity unavailable")
	}
	id.Birth, err = parseUTCBirth(string(b))
	return id, err
}
func registryBytes(root string, pid int) ([]byte, error) {
	if !filepath.IsAbs(root) || pid <= 1 {
		return nil, errors.New("registry unavailable")
	}
	path := filepath.Join(root, "sessions", strconv.Itoa(pid)+".json")
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("registry unavailable")
	}
	f := os.NewFile(uintptr(fd), "registry")
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() > maxRegistryBytes {
		return nil, errors.New("registry unavailable")
	}
	st, ok := before.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) {
		return nil, errors.New("registry unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRegistryBytes+1))
	if err != nil || len(data) > maxRegistryBytes {
		return nil, errors.New("registry unavailable")
	}
	after, err := f.Stat()
	current, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !current.Mode().IsRegular() || !os.SameFile(before, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("registry unavailable")
	}
	return data, nil
}

// AssociateRegistryRoot is called only by the trusted own bind invocation
// after its ancestry/owner/pane proof. It never requires idle at bind and never
// uses an ambient root. An unqualified version does not erase proven provenance.
func AssociateRegistryRoot(ctx context.Context, t Target, session, root string) string {
	if t.Runtime != "claude" || !filepath.IsAbs(root) {
		return ""
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	if _, _, _, err = t.Inspect(ctx); err != nil {
		return ""
	}
	id, err := liveRegistryIdentity(ctx, t, session)
	if err != nil {
		return ""
	}
	data, err := registryBytes(root, t.OwnerPID)
	if err != nil {
		return ""
	}
	m, err := decodeObject(data)
	if err != nil || registryIdentityReason(m, id) != RuntimeNone {
		return ""
	}
	if _, _, _, err = t.Inspect(ctx); err != nil || ctx.Err() != nil {
		return ""
	}
	return root
}
