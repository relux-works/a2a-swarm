package voice

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Clean physical paths pass; parent redirection and all ambiguous spellings
// refuse, including symlink/.. resolution to distinct unverified bytes.
func TestVerifyWorkerPhysicalPath(t *testing.T) {
	for _, variant := range []string{"valid", "parent-link", "dotdot-link", "relative", "dot", "double-slash"} {
		t.Run(variant, func(t *testing.T) {
			c := releaseFixture(t)
			root := filepath.Dir(c.Worker)
			parent := filepath.Dir(root)
			link := filepath.Join(parent, "hop")
			switch variant {
			case "parent-link":
				if err := os.Symlink(parent, link); err != nil {
					t.Fatal(err)
				}
				c.Worker = link + "/" + filepath.Base(root) + "/" + WorkerName
			case "dotdot-link":
				otherParent, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				other := filepath.Join(otherParent, filepath.Base(root))
				sub := filepath.Join(otherParent, "sub")
				for _, dir := range []string{other, sub} {
					if err := os.Mkdir(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(other, WorkerName), []byte("unverified alternate worker"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(sub, link); err != nil {
					t.Fatal(err)
				}
				c.Worker = link + "/../" + filepath.Base(root) + "/" + WorkerName

			case "relative":
				c.Worker = WorkerName
			case "dot":
				c.Worker = root + "/./" + WorkerName
			case "double-slash":
				c.Worker = root + "//" + WorkerName
			}
			got, err := VerifyWorker(context.Background(), c)
			if variant == "valid" {
				if err != nil || got != c.Worker {
					t.Fatalf("positive: %q %v", got, err)
				}
			} else if err == nil {
				t.Fatalf("admitted %s", c.Worker)
			}
		})
	}
}

// Rename source payloads after preparation: the executed pathname and BOTH
// library bytes remain the verified private snapshot until the child exits.
func TestPreparedWorkerRenameIsolation(t *testing.T) {
	for _, variant := range []string{"valid", WorkerName, "lib/libonnxruntime.dylib", "lib/libsherpa-onnx-c-api.dylib", "lib"} {
		t.Run(variant, func(t *testing.T) {
			c := releaseFixture(t)
			w, err := PrepareWorker(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			if w.Path == c.Worker {
				t.Fatal("exec still names source payload")
			}
			if variant != "valid" {
				source := filepath.Join(filepath.Dir(c.Worker), variant)
				if err := os.Rename(source, source+".old"); err != nil {
					t.Fatal(err)
				}
				if variant == "lib" {
					if err := os.Mkdir(source, 0700); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(source, []byte("replacement"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range ReleaseFiles() {
				b, err := os.ReadFile(filepath.Join(filepath.Dir(w.Path), name))
				if err != nil || !bytes.Equal(b, []byte("fixture "+name)) {
					t.Fatalf("snapshot replaced: %s %v", name, err)
				}
			}
			w.Close()
			if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
				t.Fatal("snapshot retained")
			}
		})
	}
}

// A second synthesis failure shares the fixed warning; the long positive
// control executes both calls and preserves the exact spoken truncation notice.
func TestRenderNoticeFailureWarning(t *testing.T) {
	for _, fail := range []int{0, 3} {
		r := &renderRunner{t: t, fail: fail, source: bytes.Repeat([]byte{1, 0}, sampleRate)}
		a, err := (LocalRenderer{Runner: r}).Render(context.Background(), t.TempDir(), Config{Enabled: true, MaxSeconds: 180}, strings.Repeat("Я", 6001))
		if fail == 0 {
			if err != nil || !a.Truncated || len(r.inputs) != 2 || r.inputs[1] != TruncationNotice {
				t.Fatalf("positive: %+v %v", a, err)
			}
		} else if err == nil || err.Error() != "voice synthesis failed or timed out; check the local engine and installed model" {
			t.Fatalf("notice warning: %v", err)
		}
	}
}

type renameWriter struct {
	t       *testing.T
	path    string
	replace bool
}

func (w renameWriter) Write(b []byte) (int, error) {
	if w.replace && len(b) > 0 {
		if err := os.Rename(w.path, w.path+".old"); err != nil {
			w.t.Fatal(err)
		}
		// The replacement has identical bytes, size and permissions, but another inode.
		if err := os.WriteFile(w.path, b, 0600); err != nil {
			w.t.Fatal(err)
		}
	}
	return len(b), nil
}

// A rename DURING descriptor hashing is refused even with equal bytes/size/mode;
// the regular unchanged descriptor remains a positive for the same copy path.
func TestReleaseDescriptorRename(t *testing.T) {
	for _, replace := range []bool{false, true} {
		p := filepath.Join(t.TempDir(), "payload")
		if err := os.WriteFile(p, []byte("intact payload"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := copyReleaseFile(context.Background(), p, renameWriter{t, p, replace}, 4096)
		if (err != nil) != replace {
			t.Fatalf("replace=%v: %v", replace, err)
		}
	}
}
