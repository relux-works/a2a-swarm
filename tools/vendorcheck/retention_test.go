package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Execute the committed workflow's complete anchor-check script against local
// Git fixtures. URL rewriting keeps fetches local; no hosted service is used.
func TestRetentionWorkflow(t *testing.T) {
	workflow, err := os.ReadFile("../../.github/workflows/curator-build-retention.yml")
	if err != nil {
		t.Fatal(err)
	}
	blocks := strings.Split(string(workflow), "        run: |\n")
	if len(blocks) != 3 {
		t.Fatal("workflow run blocks changed")
	}
	var script strings.Builder
	for _, line := range strings.Split(blocks[2], "\n") {
		if line != "" && !strings.HasPrefix(line, "          ") {
			break
		}
		script.WriteString(strings.TrimPrefix(line, "          ") + "\n")
	}
	repo := t.TempDir()
	fixtureGit(t, repo, "", "init", "--bare", "--quiet", "--template=")
	base := fixtureCommit(t, repo, nil, "", "base")
	pin := fixtureCommit(t, repo, nil, base, "retained")
	offBranch := fixtureCommit(t, repo, nil, base, "unretained")
	anchorMain := func(content string, exists bool) string {
		files := map[string]fixtureFile{}
		if exists {
			files["docs/curator-build-pins.txt"] = fixtureFile{content, "100644"}
		}
		return fixtureCommit(t, repo, files, base, "anchors")
	}
	goodMain := anchorMain(pin+"\n", true)
	check := func(main, build, want string) {
		t.Helper()
		for ref, oid := range map[string]string{"refs/heads/main": main, "refs/heads/curator-build": build} {
			if oid == "" {
				fixtureGit(t, repo, "", "update-ref", "-d", ref)
			} else {
				fixtureGit(t, repo, "", "update-ref", ref, oid)
			}
		}
		cmd := exec.Command("/bin/bash", "-c", script.String())
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "GIT_") && !strings.HasPrefix(value, "HOME=") && !strings.HasPrefix(value, "RUNNER_TEMP=") {
				cmd.Env = append(cmd.Env, value)
			}
		}
		cmd.Env = append(cmd.Env, "HOME="+t.TempDir(), "RUNNER_TEMP="+t.TempDir(),
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0",
			"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=url.file://"+filepath.ToSlash(repo)+".insteadOf",
			"GIT_CONFIG_VALUE_0=https://github.com/relux-works/a2a-swarm.git")
		output, err := cmd.CombinedOutput()
		t.Logf("monitor output=%q exit=%v", strings.TrimSpace(string(output)), err)
		if want == "" {
			if err != nil || !strings.Contains(string(output), "build_pin_retention_equal pins=1") {
				t.Fatalf("positive: %v %s", err, output)
			}
		} else {
			exit, exited := err.(*exec.ExitError)
			if !exited || exit.ExitCode() != 1 || !strings.Contains(string(output), want) || strings.Contains(string(output), "fatal:") {
				t.Fatalf("want named refusal %q, exit 1: %v %s", want, err, output)
			}
		}
	}
	check(goodMain, pin, "")
	check(anchorMain(pin, true), pin, "") // A retained final line needs no LF.
	for _, plant := range []struct{ main, build, refusal string }{
		{anchorMain(pin+"\n"+offBranch, true), pin, "build_pin_not_on_branch: " + offBranch},
		{anchorMain("", false), pin, "build_anchor_file_unavailable: docs/curator-build-pins.txt"},
		{anchorMain("HEAD", true), pin, "build_anchor_invalid"},
		{anchorMain("# empty\n", true), pin, "build_anchors_missing"},
		{"", pin, "build_main_unavailable"},
		{goodMain, "", "build_branch_unavailable"},
	} {
		check(plant.main, plant.build, plant.refusal)
		check(goodMain, pin, "") // Each plant has a restored positive control.
	}
}
