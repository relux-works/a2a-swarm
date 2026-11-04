// Source-only compiler overlay preparation. No fixture or test is executed.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strings"
)

type edit struct{ name, old, new string }

func must(e error) {
	if e != nil {
		panic(e)
	}
}
func output(p string, v any) {
	b, e := json.MarshalIndent(v, "", "  ")
	must(e)
	must(os.WriteFile(p, append(b, '\n'), 0600))
}
func main() {
	if len(os.Args) != 2 {
		panic("usage: prepare output-directory")
	}
	root, e := os.Getwd()
	must(e)
	out, e := filepath.Abs(os.Args[1])
	must(e)
	must(os.MkdirAll(out, 0700))
	changes := map[string]string{}
	edits := []edit{
		{"internal/cli/bind.go", "\"github.com/relux-works/a2a-swarm/internal/config\"", "\"github.com/relux-works/a2a-swarm/internal/config\"\n\"github.com/relux-works/a2a-swarm/internal/n1fixture\""},
		{"internal/cli/bind.go", "for i := 0; i < 64 && pid > 1; i++ {", "for i := 0; i < 64 && pid > 1; i++ {\n if n1fixture.Require(pid)!=nil { return false }"},
		{"internal/cli/bind.go", "\t\tif st.HostEnrollment != nil {", "\t\tif err = n1AfterLoad(ctx, st); err != nil { return err }\n\t\tif st.HostEnrollment != nil {"},
		{"internal/cli/bind.go", "child = exec.Command(exe, append(pin, \"--as\", s.name, \"bind\", \"--run-generation\", b.Generation)...)", "child = n1ConsumerCommand(exe, append(pin, \"--as\", s.name, \"bind\", \"--run-generation\", b.Generation), b.Generation)"},
		{"internal/cli/bind.go", "child.Env = detachedConsumerEnv(os.Environ(), s.paths.ConfigRoot(), b.SessionID)", "child.Env = n1ConsumerEnv(detachedConsumerEnv(os.Environ(), s.paths.ConfigRoot(), b.SessionID), b.Generation)"},
		{"internal/cli/bind.go", "if err = child.Start(); err != nil {", "if err = n1Started(child, b.Generation); err != nil {"},
		{"internal/cli/bind.go", "go child.Wait()", "// Wait owner was installed immediately by n1Started."},
		{"internal/cli/bind.go", "\t\tvar runErr error\n", "\t\tif err = n1fixture.Latch(ctx, os.Getenv(\"N1_ROLE\")+\"-startup\"); err != nil { return err }\n\t\tvar runErr error\n"},
		{"internal/cli/bind_process_darwin.go", "\"golang.org/x/sys/unix\"", "\"github.com/relux-works/a2a-swarm/internal/n1fixture\"\n\"golang.org/x/sys/unix\""},
		{"internal/cli/bind_process_darwin.go", "func bindingArguments(pid int) ([]string, []string, error) {", "func bindingArguments(pid int) ([]string, []string, error) {\n if err:=n1fixture.Require(pid);err!=nil{return nil,nil,err}"},
		{"internal/cli/bind_process_darwin.go", "func bindingImage(ctx context.Context, pid int) (string, error) {", "func bindingImage(ctx context.Context, pid int) (string, error) {\n if err:=n1fixture.Require(pid);err!=nil{return \"\",err}"},
		{"internal/tmuxwake/terminal.go", "\"time\"", "\"time\"\n\"github.com/relux-works/a2a-swarm/internal/n1fixture\""},
		{"internal/tmuxwake/terminal.go", "func Birth(ctx context.Context, pid int) (string, error) {", "func Birth(ctx context.Context, pid int) (string, error) {\n if err:=n1fixture.Require(pid);err!=nil{return \"\",err}"},
		{"internal/cli/identity.go", "\"github.com/relux-works/a2a-swarm/internal/config\"", "\"github.com/relux-works/a2a-swarm/internal/config\"\n\"github.com/relux-works/a2a-swarm/internal/n1fixture\""},
		{"internal/cli/identity.go", "func processGone(pid int) bool {", "func processGone(pid int) bool {\n if gone,err:=n1fixture.Gone(pid);err!=nil{return false}else if gone{return true}"},
		{"internal/cli/bind_stranded.go", "\ta.publishTerminalNotice(ctx, s, generation)", "\tif err:=n1BeforeReport(ctx,generation);err!=nil{return err}\n\ta.publishTerminalNotice(ctx, s, generation)"},
		{"internal/cli/bind_stranded.go", "\treturn nil\n}\n\n// Authority comes only", "\treturn n1AfterReport(ctx,generation)\n}\n\n// Authority comes only"},
	}
	for _, r := range edits {
		s, ok := changes[r.name]
		if !ok {
			b, e := os.ReadFile(filepath.Join(root, r.name))
			must(e)
			s = string(b)
		}
		if strings.Count(s, r.old) != 1 {
			panic(fmt.Sprintf("%s edit count != 1: %s", r.name, r.old))
		}
		changes[r.name] = strings.Replace(s, r.old, r.new, 1)
	}
	p := "internal/cli/bind_process.go"
	b, e := os.ReadFile(filepath.Join(root, p))
	must(e)
	s := string(b)
	start := strings.Index(s, "\traw, err := exec.CommandContext")
	if start < 0 {
		panic("enumerator start missing")
	}
	end := strings.Index(s[start:], "\n}\n") + start
	if end < start {
		panic("enumerator end missing")
	}
	s = s[:start] + "\treturn n1Rows(ctx)" + s[end:]
	for _, line := range []string{"\t\"os/exec\"\n", "\t\"strconv\"\n"} {
		s = strings.Replace(s, line, "", 1)
	}
	changes[p] = s
	freeze := func(label string, content map[string]string) {
		mapping := map[string]string{}
		hashes := map[string]string{}
		for name, s := range content {
			b, e := format.Source([]byte(s))
			must(e)
			path := filepath.Join(out, label, name)
			must(os.MkdirAll(filepath.Dir(path), 0700))
			must(os.WriteFile(path, b, 0600))
			mapping[filepath.Join(root, name)] = path
			h := sha256.Sum256(b)
			hashes[name] = hex.EncodeToString(h[:])
		}
		output(filepath.Join(out, label+"-overlay.json"), map[string]any{"Replace": mapping})
		output(filepath.Join(out, label+"-source.json"), hashes)
	}
	freeze("fixed", changes)
	plant := map[string]string{}
	for k, v := range changes {
		plant[k] = v
	}
	p = "internal/cli/bind.go"
	old := "if err = persistTerminalReplacement(s, b, automatic); err != nil {"
	if strings.Count(plant[p], old) != 1 {
		panic("plant count != 1")
	}
	plant[p] = strings.Replace(plant[p], old, "carryTerminalNoticeEpisode(st.Terminal, b, automatic)\n\t\tif _, err = config.UpdateState(s.paths, s.name, func(current *config.State) error { current.Terminal = b; return nil }); err != nil {", 1)
	freeze("old-snapshot", plant)
	output(filepath.Join(out, "plant.json"), map[string]any{"applications": 1, "path": p, "execution": "NOT RUN", "old": old, "new_semantics": "snapshot carry + whole-binding state assignment; startup fix unchanged"})
	if bytes.Equal([]byte(plant[p]), []byte(changes[p])) {
		panic("plant not applied")
	}
}
