package cli

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// PublishResult is the stable, opt-in result of a user-invoked publication.
// ReplyTo is the effective Matrix reply target (empty for a new topic).
type PublishResult struct {
	EventID   string `json:"event_id"`
	RoomID    string `json:"room_id"`
	TopicRoot string `json:"topic_root"`
	ReplyTo   string `json:"reply_to"`
}

type resultFlags struct {
	json *bool
	path *string
}

func addResultFlags(fs *flag.FlagSet) resultFlags {
	return resultFlags{
		fs.Bool("json", false, "print one publication result object"),
		fs.String("id-file", "", "write the event id to a new private file (never overwritten)"),
	}
}

func flagPresent(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) { found = found || f.Name == name })
	return found
}

func (r resultFlags) prepare(fs *flag.FlagSet) (*privateOutput, error) {
	if !flagPresent(fs, "id-file") {
		return nil, nil
	}
	return preparePrivateOutput(*r.path)
}

func (r resultFlags) emit(a *App, file *privateOutput, result PublishResult) error {
	if file == nil && !*r.json {
		// Keep the existing default bytes and behavior.
		a.printf("%s\n", result.EventID)
		return nil
	}
	for _, field := range []string{result.EventID, result.RoomID, result.TopicRoot, result.ReplyTo} {
		if len(field) > 4096 || sanitizeLine(field) != field || a.secrets.Scrub(field) != field {
			return fmt.Errorf("publication succeeded but receipt fields could not be safely displayed; do not repost")
		}
	}
	if file != nil {
		if err := file.commit([]byte(result.EventID + "\n")); err != nil {
			return fmt.Errorf("sent %s; could not save event id: %w; do not repost", result.EventID, err)
		}
	}
	line := result.EventID
	if *r.json {
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		line = string(data)
	}
	if err := writeCLIOutput(a.Stdout, line+"\n"); err != nil {
		return fmt.Errorf("sent %s; %w; do not repost", result.EventID, err)
	}
	return nil
}

func writeCLIOutput(w io.Writer, text string) error {
	n, err := io.WriteString(w, text)
	if err == nil && n != len(text) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	if f, ok := w.(interface{ Flush() error }); ok {
		if err := f.Flush(); err != nil {
			return fmt.Errorf("flush output: %w", err)
		}
	}
	return nil
}

// openOutputParent walks and pins directories, refusing symlinks in every path
// component. Operations after this walk are relative to the pinned parent, so a
// parent rename cannot redirect publication. No checked-rename fallback is used.
func openOutputParent(path string) (*os.Root, string, error) {
	if path == "" || len(path) > 4096 {
		return nil, "", usagef("output path must be nonempty, at most 4096 bytes, without control characters")
	}
	for _, r := range path {
		if isUnsafeRune(r) || r == '\u2028' || r == '\u2029' {
			return nil, "", usagef("output path must be nonempty, at most 4096 bytes, without control characters")
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	volume := filepath.VolumeName(abs)
	root, err := os.OpenRoot(volume + string(filepath.Separator))
	if err != nil {
		return nil, "", err
	}
	dir, name := filepath.Split(strings.TrimPrefix(abs, volume+string(filepath.Separator)))
	if name == "" || name == "." || name == ".." {
		root.Close()
		return nil, "", usagef("output path must name a file")
	}
	for _, part := range strings.Split(filepath.Clean(dir), string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		st, err := root.Lstat(part)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			root.Close()
			return nil, "", fmt.Errorf("output parent must exist and contain no symlinks")
		}
		next, err := root.OpenRoot(part)
		if err != nil {
			root.Close()
			return nil, "", err
		}
		opened, err := next.Stat(".")
		root.Close()
		if err != nil || !os.SameFile(st, opened) {
			next.Close()
			return nil, "", fmt.Errorf("output parent changed while opening")
		}
		root = next
	}
	return root, name, nil
}

type privateOutput struct {
	root      *os.Root
	name      string
	tmp       *os.File
	tmpName   string
	closeRoot bool
}

func preparePrivateOutput(path string) (*privateOutput, error) {
	root, name, err := openOutputParent(path)
	if err != nil {
		return nil, err
	}
	p, err := preparePrivateOutputAt(root, name)
	if err != nil {
		root.Close()
		return nil, err
	}
	p.closeRoot = true
	return p, nil
}

func preparePrivateOutputAt(root *os.Root, name string) (*privateOutput, error) {
	if _, err := root.Lstat(name); err == nil {
		return nil, fmt.Errorf("output target already exists; refusing to overwrite")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	tmpName := ".a2a-output-" + rand.Text()
	tmp, err := root.OpenFile(tmpName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	p := &privateOutput{root: root, name: name, tmp: tmp, tmpName: tmpName}
	if err := tmp.Chmod(0o600); err != nil {
		p.close()
		return nil, err
	}
	return p, nil
}

func (p *privateOutput) close() {
	if p == nil {
		return
	}
	p.tmp.Close()
	p.root.Remove(p.tmpName)
	if p.closeRoot {
		p.root.Close()
	}
}

func (p *privateOutput) commit(data []byte) error {
	if n, err := p.tmp.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	return p.publish()
}

func (p *privateOutput) publish() error {
	if err := p.tmp.Sync(); err != nil {
		return err
	}
	if err := p.tmp.Close(); err != nil {
		return err
	}
	if err := p.root.Link(p.tmpName, p.name); err != nil {
		return fmt.Errorf("publish private output without overwrite: %w", err)
	}
	if err := p.root.Remove(p.tmpName); err != nil {
		return err
	}
	dir, err := p.root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
