package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/daemon"
	"github.com/relux-works/a2a-swarm/internal/wakemute"
)

// daemonHandler constructs a fresh App for every authenticated invocation.
// Neither selection/cache fields nor daemon environment/session state are copied.
func (a *App) daemonHandler(home string) daemon.Handler {
	requiredVersion := ""
	if a.Getenv != nil {
		requiredVersion = a.Getenv(wakemute.EnvRequiredVersion)
	} // Immutable service-start admission, never station input.
	return func(ctx context.Context, inv daemon.Invocation) int {
		refuse := func(message string) int { _, _ = io.WriteString(inv.Stderr, message); return ExitError }
		if inv.Request.Session != "" && !daemon.ValidSession(inv.Request.Session) {
			return refuse("daemon: not_allowed\n")
		}
		plan, err := daemon.Authorize(inv.Request.Argv, inv.Identity)
		if err != nil {
			return refuse("daemon: not_allowed\n")
		}
		if config.ValidateName(inv.Identity) != nil {
			return refuse("daemon: daemon_unavailable\n")
		}
		root, err := daemon.IdentityHome(home, inv.Identity)
		if err != nil {
			return refuse("daemon: daemon_unavailable\n")
		}
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil || canonical != root {
			return refuse("daemon: daemon_unavailable\n")
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			return refuse("daemon: daemon_unavailable\n")
		}
		request := &App{Stdout: inv.Stdout, Stderr: inv.Stderr, Stdin: bytes.NewReader(inv.Request.Stdin), HTTP: a.HTTP, Now: a.Now, as: inv.Identity, ctx: ctx, daemonEngine: true, daemonFiles: map[string]daemon.InputFile{}}
		defer request.closeAdmission()
		request.Getenv = func(k string) string {
			if k == wakemute.EnvRequiredVersion {
				return requiredVersion
			}
			if k == config.EnvConfigDir {
				return root
			}
			if k == EnvSessionID {
				return inv.Request.Session
			}
			return ""
		}
		// Dependency injection is explicit; this is a stateless backend adapter, not
		// a selected identity, connection cache, environment or previous request.
		request.connectionSecretStore = a.connectionSecretStore
		request.daemonOwner = a.daemonOwner
		request.daemonOwnerEpoch = a.daemonOwnerEpoch
		request.Stdout = daemonOutput{request, inv.Stdout}
		request.Stderr = daemonOutput{request, inv.Stderr}
		if request.Now == nil {
			request.Now = time.Now
		}
		c, err := request.selectedConnection()
		if err != nil {
			return refuse("daemon: daemon_unavailable\n")
		}
		fingerprint := ""
		if c != nil {
			b, e := c.Binding(inv.Identity, false)
			if e != nil {
				return refuse("daemon: daemon_unavailable\n")
			}
			fingerprint = b.Fingerprint()
		}
		if plan.Selection.Profile != "" && plan.Selection.Profile != request.profile || plan.Selection.Fingerprint != "" && plan.Selection.Fingerprint != fingerprint {
			return refuse("daemon: not_allowed\n")
		}
		paths, err := request.paths()
		if err != nil {
			return refuse("daemon: daemon_unavailable\n")
		}
		if _, err = request.credentialMetadata(paths, inv.Identity); err != nil {
			return refuse("daemon: daemon_unavailable\n")
		}
		if len(plan.Inputs) != len(inv.Request.Files) {
			return refuse("daemon: not_allowed\n")
		}
		argv := append([]string(nil), inv.Request.Argv...)
		for index, attachment := range plan.Inputs {
			file, ok := inv.Request.Files[index]
			if !ok || safeFileName(file.Name) != nil {
				return refuse("daemon: not_allowed\n")
			}
			ref := "daemon-input-" + strconv.Itoa(index)
			request.daemonFiles[ref] = file
			if attachment {
				argv[index] = ref
			} else {
				prefix := ""
				if name, _, eq := strings.Cut(argv[index], "="); eq && strings.HasPrefix(name, "-") {
					prefix = name + "="
				}
				argv[index] = prefix + "@" + ref
			}
		}
		plan, err = daemon.Authorize(argv, inv.Identity)
		if err != nil {
			return refuse("daemon: not_allowed\n")
		}
		// inbox show without a streamed --from could reopen a saved station path
		// retained in state by an older local run. It is explicitly refused.
		if plan.Command == "inbox" && len(plan.Args) > 0 && plan.Args[0] == "show" && len(request.daemonFiles) == 0 {
			return refuse("daemon: not_allowed\n")
		}
		request.afterPrint = inv.Delivered
		for _, cmd := range commands() {
			if cmd.name != plan.Command {
				continue
			}
			err := cmd.run(ctx, request, plan.Args)
			if errors.Is(err, config.ErrMonitorConflict) || request.daemonLeaseConflict {
				if inv.Refuse != nil {
					return inv.Refuse(daemon.LeaseConflict)
				}
				return refuse("daemon: daemon_lease_conflict\n")
			}
			if err != nil {
				// Error values may include peer message bodies, private paths or backend
				// diagnostics. Do not send them to the daemon log or expose their bytes.
				return refuse("daemon: daemon_unavailable\n")
			}
			return ExitOK
		}
		return refuse("daemon: not_allowed\n")
	}
}

type inputFile interface {
	io.Reader
	io.Seeker
	io.Closer
	Stat() (os.FileInfo, error)
}
type memoryFile struct {
	*bytes.Reader
	name string
	size int64
}

func (f *memoryFile) Close() error               { return nil }
func (f *memoryFile) Stat() (os.FileInfo, error) { return f, nil }
func (f *memoryFile) Name() string               { return f.name }
func (f *memoryFile) Size() int64                { return f.size }
func (f *memoryFile) Mode() os.FileMode          { return 0600 }
func (f *memoryFile) ModTime() time.Time         { return time.Time{} }
func (f *memoryFile) IsDir() bool                { return false }
func (f *memoryFile) Sys() any                   { return nil }
func (a *App) openInputFile(path string) (inputFile, error) {
	if !a.daemonEngine {
		return os.Open(path)
	}
	f, ok := a.daemonFiles[path]
	if !ok {
		return nil, errors.New("daemon: refused")
	}
	return &memoryFile{bytes.NewReader(f.Data), f.Name, int64(len(f.Data))}, nil
}
func (a *App) inputName(path string) string {
	if a.daemonEngine {
		if f, ok := a.daemonFiles[path]; ok {
			return f.Name
		}
	}
	return filepath.Base(path)
}
func (a *App) savedInboxInput(path string) ([]EventRecord, error) {
	if !a.daemonEngine {
		return readSavedInbox(path)
	}
	f, err := a.openInputFile(trimInputMarker(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseSavedInbox(f)
}
func trimInputMarker(s string) string {
	if len(s) > 0 && s[0] == '@' {
		return s[1:]
	}
	return s
}

// compile-time check keeps the in-memory reader on the same seek/read boundary.
var _ inputFile = (*memoryFile)(nil)

// The command's own output is kept byte-exact except for its known secrets.
// Scrub before transport chunking so a token spanning transport frames stays hidden.
type daemonOutput struct {
	app    *App
	writer io.Writer
}

func (w daemonOutput) Write(data []byte) (int, error) {
	clean := []byte(w.app.secrets.Scrub(string(data)))
	n, err := w.writer.Write(clean)
	if err == nil && n != len(clean) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return 0, err
	}
	return len(data), nil
}
