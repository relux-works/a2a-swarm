// A private executable contract fixture. It performs no ASR and proves no
// language/codec/model accuracy. It is never selected by the public API.
package main

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--fixture-child" {
		time.Sleep(time.Minute)
		return
	}
	dir, err := os.Getwd()
	if err != nil || len(os.Args) != 10 {
		os.Exit(81)
	}
	for _, key := range []string{"HOME", "TMPDIR", "XDG_CACHE_HOME"} {
		p := os.Getenv(key)
		st, e := os.Lstat(p)
		if e != nil || !st.IsDir() || st.Mode().Perm() != 0700 || filepath.Dir(p) != dir {
			os.Exit(82)
		}
	}
	if os.Getenv("PATH") != "" || os.Getenv("LD_PRELOAD") != "" || os.Getenv("DYLD_LIBRARY_PATH") != "" {
		os.Exit(83)
	}
	b, err := os.ReadFile("input.wav")
	if err != nil || len(b) < 46 {
		os.Exit(84)
	}
	st, err := os.Stat("input.wav")
	if err != nil || st.Mode().Perm() != 0600 {
		os.Exit(85)
	}
	mode := binary.LittleEndian.Uint16(b[44:46])
	if err := os.WriteFile("ready", nil, 0600); err != nil {
		os.Exit(86)
	}
	switch mode {
	case 1:
		// Diagnostic bytes depend on fresh private synthetic input. They must
		// never occur in an outcome or the public test log.
		s := hex.EncodeToString(b[46:])
		fmt.Fprint(os.Stderr, s)
		fmt.Fprint(os.Stdout, s)
		os.Exit(7)
	case 2:
		fmt.Print(strings.Repeat("x", (64<<10)+1))
		return
	case 3:
		json.NewEncoder(os.Stdout).Encode(map[string]string{"text": strings.Repeat("x", (16<<10)+1), "lang": "en"})
		return
	case 4:
		fmt.Print("{invalid")
		return
	case 5:
		fmt.Print(`{"text":"a","text":"b","lang":"en"}`)
		return
	case 6:
		fmt.Print(`{"text":"machine fixture","lang":"unrecognized"}`)
		return
	case 7:
		time.Sleep(time.Minute)
	case 8:
		exe, err := os.Executable()
		if err != nil {
			os.Exit(87)
		}
		cmd := exec.Command(exe, "--fixture-child")
		cmd.Env = os.Environ()
		if cmd.Start() != nil {
			os.Exit(88)
		}
		if os.WriteFile("descendant", []byte(strconv.Itoa(cmd.Process.Pid)), 0600) != nil {
			os.Exit(89)
		}
		time.Sleep(time.Minute)
	case 9:
		fmt.Print(`{"text":"","lang":"en"}`)
		return
	case 10:
		fmt.Print(`{"text":"a","lang":"en"} {}`)
		return
	}
	fmt.Print(`{"text":"machine fixture","lang":"en","ys_log_probs":[0.95]}`)
}
