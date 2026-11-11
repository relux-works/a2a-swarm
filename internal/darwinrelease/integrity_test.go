package darwinrelease

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The same signed objects, tag and anchor must reject an unsigned intermediate
// with and without a graft, including a linked worktree's common metadata.
// This does not model arbitrary concurrent filesystem mutation.
func TestReleaseHistoryGraphIntegrity(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name := "ordinary"
		if linked {
			name = "linked-worktree"
		}
		t.Run(name, func(t *testing.T) {
			f := newGitFixture(t)
			if _, err := verifyGit(f.root, "v0.1.0", "0.1.0", "", f.p, localGit(f.root)); err != nil {
				t.Fatal("clean signed control", err)
			}
			fixtureCommand(t, f.root, "git", "commit", "-q", "--no-gpg-sign", "--allow-empty", "-m", "unsigned intermediate")
			fixtureCommand(t, f.root, "git", "commit", "-q", "-S", "--allow-empty", "-m", "approved descendant")
			head := fixtureCommand(t, f.root, "git", "rev-parse", "HEAD")
			fixtureCommand(t, f.root, "git", "tag", "-d", "v0.1.0")
			fixtureCommand(t, f.root, "git", "tag", "-s", "v0.1.0", "-m", "signed tag")
			root := f.root
			if linked {
				root = filepath.Join(t.TempDir(), "linked")
				fixtureCommand(t, f.root, "git", "worktree", "add", "--detach", root, "HEAD")
			}
			check := func(want string) {
				t.Helper()
				_, err := verifyGit(root, "v0.1.0", "0.1.0", "", f.p, localGit(root))
				if err == nil || err.Error() != want {
					t.Fatalf("history guard wanted %s, got %v", want, err)
				}
			}
			check("commit-signer")
			object := fixtureCommand(t, root, "git", "cat-file", "commit", head)
			tag := fixtureCommand(t, root, "git", "rev-parse", "refs/tags/v0.1.0")
			common := fixtureCommand(t, root, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
			graft := filepath.Join(common, "info", "grafts")
			planted := []byte(head + " " + f.p.HistoryBase + "\n")
			if err := os.WriteFile(graft, planted, 0600); err != nil {
				t.Fatal(err)
			}
			check("source-graph-metadata")
			after, err := os.ReadFile(graft)
			afterObject, objectErr := localGit(root)("cat-file", "commit", head)
			afterTag, tagErr := localGit(root)("rev-parse", "refs/tags/v0.1.0")
			if err != nil || objectErr != nil || tagErr != nil || !bytes.Equal(after, planted) || afterObject != object || afterTag != tag {
				t.Fatal("refusal changed metadata or signed objects", err)
			}
			if err := os.Remove(graft); err != nil {
				t.Fatal(err)
			}
			check("commit-signer")
		})
	}
}

// Shallow boundaries are refused at resolved/common paths, while the same
// complete signed checkout is valid before and after the isolated plant.
func TestReleaseShallowHistoryRefusal(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name := "ordinary"
		if linked {
			name = "linked-worktree"
		}
		t.Run(name, func(t *testing.T) {
			f := newGitFixture(t)
			root := f.root
			if linked {
				root = filepath.Join(t.TempDir(), "linked")
				fixtureCommand(t, f.root, "git", "worktree", "add", "--detach", root, "HEAD")
			}
			if _, err := verifyGit(root, "v0.1.0", "0.1.0", "", f.p, localGit(root)); err != nil {
				t.Fatal("clean signed control", err)
			}
			common := fixtureCommand(t, root, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
			path := filepath.Join(common, "shallow")
			plant := []byte(f.commit + "\n")
			if err := os.WriteFile(path, plant, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := verifyGit(root, "v0.1.0", "0.1.0", "", f.p, localGit(root)); err == nil || err.Error() != "source-graph-metadata" {
				t.Fatal("shallow history admitted or unrelated rejection", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, plant) {
				t.Fatal("shallow refusal changed metadata", err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if _, err := verifyGit(root, "v0.1.0", "0.1.0", "", f.p, localGit(root)); err != nil {
				t.Fatal("restored signed control", err)
			}
		})
	}
}

// Snapshot only tar members, deliberately stopping at tar EOF. This independent
// fixture check establishes that each corruption preserves all member bytes.
func fixtureArchiveMembers(t *testing.T, data []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	members := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return members
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		members[h.Name] = string(b)
	}
}

// The real writer is accepted; unchanged tar members cannot hide a bad CRC,
// missing trailer or trailing data/member. Independent full gzip reads validate
// the corruption plant; these inert archives are not native signing evidence.
func TestReleaseArchiveStreamIntegrity(t *testing.T) {
	payload := fixturePayload(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "valid.tar.gz")
	if err := makeArchive(payload, path); err != nil {
		t.Fatal(err)
	}
	if err := verifyArchive(path, payload); err != nil {
		t.Fatal("real writer control", err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	members := fixtureArchiveMembers(t, original)
	var second bytes.Buffer
	w := gzip.NewWriter(&second)
	if _, err := w.Write([]byte("second member")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, shape := range []string{"bad-crc", "truncated-trailer", "trailing-garbage", "second-member"} {
		t.Run(shape, func(t *testing.T) {
			changed := append([]byte{}, original...)
			want := "archive-gzip-integrity"
			switch shape {
			case "bad-crc":
				changed[len(changed)-8] ^= 1
			case "truncated-trailer":
				changed = changed[:len(changed)-8]
			case "trailing-garbage":
				changed = append(changed, []byte("not-a-gzip-member")...)
				want = "archive-gzip-trailing-data"
			case "second-member":
				changed = append(changed, second.Bytes()...)
				want = "archive-gzip-trailing-data"
			}
			if !reflect.DeepEqual(fixtureArchiveMembers(t, changed), members) {
				t.Fatal("plant changed tar member contents")
			}
			reference, err := gzip.NewReader(bytes.NewReader(changed))
			if err != nil {
				t.Fatal(err)
			}
			decoded, readErr := io.ReadAll(reference)
			closeErr := reference.Close()
			if closeErr != nil {
				t.Fatal("reference close", closeErr)
			}
			switch shape {
			case "bad-crc":
				if !errors.Is(readErr, gzip.ErrChecksum) {
					t.Fatal("CRC plant did not reach checksum error", readErr)
				}
			case "truncated-trailer":
				if !errors.Is(readErr, io.ErrUnexpectedEOF) {
					t.Fatal("trailer plant did not reach truncation", readErr)
				}
			case "trailing-garbage":
				if !errors.Is(readErr, gzip.ErrHeader) {
					t.Fatal("tail plant did not reach invalid header", readErr)
				}
			case "second-member":
				if readErr != nil || !bytes.HasSuffix(decoded, []byte("second member")) {
					t.Fatal("second member control invalid", readErr)
				}
			}
			corrupt := filepath.Join(dir, shape+".tar.gz")
			if err := os.WriteFile(corrupt, changed, 0600); err != nil {
				t.Fatal(err)
			}
			if err := verifyArchive(corrupt, payload); err == nil || err.Error() != want {
				t.Fatalf("stream guard wanted %s, got %v", want, err)
			}
			after, err := os.ReadFile(corrupt)
			if err != nil || !bytes.Equal(after, changed) {
				t.Fatal("refusal changed archive", err)
			}
		})
	}
	if err := verifyArchive(path, payload); err != nil {
		t.Fatal("unchanged positive after refusals", err)
	}
}
