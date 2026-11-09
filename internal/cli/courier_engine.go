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
	"github.com/relux-works/a2a-swarm/internal/courier"
)

// courierHandler constructs a fresh App for every authenticated invocation.
// Neither selection/cache fields nor daemon environment/session state are copied.
func (a *App) courierHandler(home string) courier.Handler {
	return func(ctx context.Context, inv courier.Invocation) int {
		refuse := func(message string) int { _, _ = io.WriteString(inv.Stderr, message); return ExitError }
		if inv.Request.Session != "" && !courier.ValidSession(inv.Request.Session) {
			return refuse("courier: not_allowed\n")
		}
		plan, err := courier.Authorize(inv.Request.Argv, inv.Identity)
		if err != nil {
			return refuse("courier: not_allowed\n")
		}
		if config.ValidateName(inv.Identity) != nil {
			return refuse("courier: courier_unavailable\n")
		}
		root, err := courier.IdentityHome(home, inv.Identity)
		if err != nil {
			return refuse("courier: courier_unavailable\n")
		}
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil || canonical != root {
			return refuse("courier: courier_unavailable\n")
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			return refuse("courier: courier_unavailable\n")
		}
		request := &App{Stdout: inv.Stdout, Stderr: inv.Stderr, Stdin: bytes.NewReader(inv.Request.Stdin), HTTP: a.HTTP, Now: a.Now, as: inv.Identity, ctx: ctx, courierEngine: true, courierFiles: map[string]courier.InputFile{}}
		request.Getenv = func(k string) string {
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
		request.Stdout = courierOutput{request, inv.Stdout}
		request.Stderr = courierOutput{request, inv.Stderr}
		if request.Now == nil {
			request.Now = time.Now
		}
		c, err := request.selectedConnection()
		if err != nil {
			return refuse("courier: courier_unavailable\n")
		}
		fingerprint := ""
		if c != nil {
			b, e := c.Binding(inv.Identity, false)
			if e != nil {
				return refuse("courier: courier_unavailable\n")
			}
			fingerprint = b.Fingerprint()
		}
		if plan.Selection.Profile != "" && plan.Selection.Profile != request.profile || plan.Selection.Fingerprint != "" && plan.Selection.Fingerprint != fingerprint {
			return refuse("courier: not_allowed\n")
		}
		paths, err := request.paths()
		if err != nil {
			return refuse("courier: courier_unavailable\n")
		}
		if _, err = request.credentialMetadata(paths, inv.Identity); err != nil {
			return refuse("courier: courier_unavailable\n")
		}
		if len(plan.Inputs) != len(inv.Request.Files) {
			return refuse("courier: not_allowed\n")
		}
		argv := append([]string(nil), inv.Request.Argv...)
		for index, attachment := range plan.Inputs {
			file, ok := inv.Request.Files[index]
			if !ok || safeFileName(file.Name) != nil {
				return refuse("courier: not_allowed\n")
			}
			ref := "courier-input-" + strconv.Itoa(index)
			request.courierFiles[ref] = file
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
		plan, err = courier.Authorize(argv, inv.Identity)
		if err != nil {
			return refuse("courier: not_allowed\n")
		}
		// inbox show without a streamed --from could reopen a saved station path
		// retained in state by an older local run. It is explicitly refused.
		if plan.Command == "inbox" && len(plan.Args) > 0 && plan.Args[0] == "show" && len(request.courierFiles) == 0 {
			return refuse("courier: not_allowed\n")
		}
		request.afterPrint = inv.Delivered
		for _, cmd := range commands() {
			if cmd.name != plan.Command {
				continue
			}
			err := cmd.run(ctx, request, plan.Args)
			if errors.Is(err, config.ErrMonitorConflict) || request.courierLeaseConflict {
				if inv.Refuse != nil {
					return inv.Refuse(courier.LeaseConflict)
				}
				return refuse("courier: courier_lease_conflict\n")
			}
			if err != nil {
				// Error values may include peer message bodies, private paths or backend
				// diagnostics. Do not send them to the daemon log or expose their bytes.
				return refuse("courier: courier_unavailable\n")
			}
			return ExitOK
		}
		return refuse("courier: not_allowed\n")
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
	if !a.courierEngine {
		return os.Open(path)
	}
	f, ok := a.courierFiles[path]
	if !ok {
		return nil, errors.New("courier: refused")
	}
	return &memoryFile{bytes.NewReader(f.Data), f.Name, int64(len(f.Data))}, nil
}
func (a *App) inputName(path string) string {
	if a.courierEngine {
		if f, ok := a.courierFiles[path]; ok {
			return f.Name
		}
	}
	return filepath.Base(path)
}
func (a *App) savedInboxInput(path string) ([]EventRecord, error) {
	if !a.courierEngine {
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
type courierOutput struct {
	app    *App
	writer io.Writer
}

func (w courierOutput) Write(data []byte) (int, error) {
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
