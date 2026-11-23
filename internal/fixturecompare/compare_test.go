package fixturecompare

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These fixture bytes exercise file/hash/comparison contracts only. Native
// signature enforcement is covered separately with self-created Mach-O images.
func fixturePair(t *testing.T, suffix string) string {
	t.Helper()
	dir := t.TempDir()
	var manifest strings.Builder
	for _, name := range names {
		b := []byte("synthetic fixture " + name + suffix)
		if err := os.WriteFile(filepath.Join(dir, name), b, 0755); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&manifest, "%s  %s\n", hash(b), name)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(manifest.String()), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func signatureFixture(path string) (signature, error) {
	return signature{raw: "complete " + filepath.Base(path), compiled: []byte("complete AST " + filepath.Base(path)), timestamp: true, runtime: true}, nil
}

func findings(report Report, code string) []Finding {
	var out []Finding
	for _, finding := range report.Findings {
		if finding.Code == code {
			out = append(out, finding)
		}
	}
	return out
}

func readReport(t *testing.T, out string) Report {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(out, "fixture-comparison.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report Report
	if err := json.Unmarshal(b, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

// Paired equal controls and full-byte deltas on either side distinguish exact
// compiled-DR comparison from length-only, prefix and raw-text comparisons.
func TestCompiledRequirements(t *testing.T) {
	substitute := func(b []byte) []byte { b[len(b)-1] ^= 1; return b }
	for _, delta := range []struct {
		name   string
		side   string
		change func([]byte) []byte
	}{
		{"next-clause", "1", func(b []byte) []byte { return append(b, []byte(" and extra term")...) }},
		{"old-clause", "0", func(b []byte) []byte { return append(b, []byte(" and extra term")...) }},
		{"next-same-length", "1", substitute},
		{"old-same-length", "0", substitute},
		{"next-truncated", "1", func(b []byte) []byte { return b[:len(b)-1] }},
		{"old-extended", "0", func(b []byte) []byte { return append(b, 0) }},
	} {
		for _, name := range names {
			for _, changed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/changed=%t", delta.name, name, changed), func(t *testing.T) {
					old, next := fixturePair(t, "N"), fixturePair(t, "N+1")
					out := filepath.Join(t.TempDir(), "evidence")
					calls := 0
					var expected [2]string
					report, err := compare(old, next, out, func(path string) (signature, error) {
						calls++
						sig, _ := signatureFixture(path)
						side := filepath.Base(filepath.Dir(path))
						if filepath.Base(path) == name {
							if changed && side == delta.side {
								sig.compiled = delta.change(sig.compiled)
							}
							expected[int(side[0]-'0')] = hash(sig.compiled)
						}
						return sig, nil
					})
					if err != nil || !report.Completed || calls != 4 {
						t.Fatalf("complete comparison: %+v, %v, calls=%d", report, err, calls)
					}
					diff := findings(report, "requirements-differ")
					if !changed {
						if len(diff) != 0 || len(report.Findings) != 0 || expected[0] != expected[1] {
							t.Fatalf("equal requirements: %+v", report.Findings)
						}
					} else if len(diff) != 1 || diff[0].Name != name || diff[0].Old == nil || diff[0].New == nil ||
						diff[0].Old.Name != name || diff[0].New.Name != name ||
						diff[0].Old.CompiledDR != expected[0] || diff[0].New.CompiledDR != expected[1] ||
						diff[0].Old.CompiledDR == diff[0].New.CompiledDR || diff[0].Old.SHA256 == diff[0].New.SHA256 ||
						diff[0].Old.DR != diff[0].New.DR {
						t.Fatalf("difference must retain both ordered sides: %+v", diff)
					}
					saved := readReport(t, out)
					if !saved.Completed || saved.ReleaseVerified || saved.Label != Label {
						t.Fatalf("not a release pass: %+v", saved)
					}
					for side := range 2 {
						for _, id := range saved.Pairs[side] {
							b, err := os.ReadFile(filepath.Join(out, fmt.Sprint(side), id.Name+".requirement.bin"))
							if err != nil || hash(b) != id.CompiledDR || !id.HashVerified || !id.StrictSignatureVerified {
								t.Fatalf("raw evidence missing or unbound: %+v, %v", id, err)
							}
						}
					}
				})
			}
		}
	}
}

// Timestamp and ad-hoc findings are retained per side/image, without preventing
// complete DR comparison. Timestamped/non-ad-hoc controls must omit them.
func TestSignatureFindings(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			report, err := compare(fixturePair(t, "N"), fixturePair(t, "N+1"), filepath.Join(t.TempDir(), "evidence"),
				func(path string) (signature, error) {
					sig, _ := signatureFixture(path)
					sig.timestamp = !missing
					sig.adHoc = missing && filepath.Base(path) == names[1]
					return sig, nil
				})
			if err != nil || !report.Completed || report.ReleaseVerified || report.Label != Label {
				t.Fatalf("diagnostic failed: %+v, %v", report, err)
			}
			wantTimes, wantAdHoc := 0, 0
			if missing {
				wantTimes, wantAdHoc = 4, 2
			}
			if len(findings(report, "timestamp-missing")) != wantTimes || len(findings(report, "ad-hoc-companion")) != wantAdHoc {
				t.Fatalf("findings lost: %+v", report.Findings)
			}
			for _, f := range findings(report, "ad-hoc-companion") {
				if f.Name != names[1] || (f.Side != "N" && f.Side != "N+1") {
					t.Fatalf("companion finding misbound: %+v", f)
				}
			}
		})
	}
}

// An ad-hoc CLI and missing runtime are findings too, with explicit per-image
// binding. A timestamped, non-ad-hoc hardened fixture omits all three findings.
func TestOtherSignatureFindings(t *testing.T) {
	for _, missing := range []bool{false, true} {
		report, err := compare(fixturePair(t, "N"), fixturePair(t, "N+1"), filepath.Join(t.TempDir(), "evidence"), func(path string) (signature, error) {
			sig, _ := signatureFixture(path)
			sig.runtime = !missing
			sig.adHoc = missing
			return sig, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		wantRuntime, wantCLI := 0, 0
		if missing {
			wantRuntime, wantCLI = 4, 2
		}
		if len(findings(report, "hardened-runtime-missing")) != wantRuntime || len(findings(report, "ad-hoc-signature")) != wantCLI {
			t.Fatalf("findings lost: %+v", report.Findings)
		}
		for _, f := range findings(report, "ad-hoc-signature") {
			if f.Name != names[0] {
				t.Fatal("CLI finding misbound")
			}
		}
	}
}

// Each image on either side is hash-pinned before any native call or evidence
// creation. A successful native stub cannot hide one altered input byte.
func TestHashRefusal(t *testing.T) {
	for side := range 2 {
		for _, name := range names {
			t.Run(fmt.Sprintf("%d/%s", side, name), func(t *testing.T) {
				dirs := [2]string{fixturePair(t, "N"), fixturePair(t, "N+1")}
				if err := os.WriteFile(filepath.Join(dirs[side], name), []byte("altered"), 0555); err != nil {
					t.Fatal(err)
				}
				calls := 0
				out := filepath.Join(t.TempDir(), "evidence")
				report, err := compare(dirs[0], dirs[1], out, func(path string) (signature, error) {
					calls++
					return signatureFixture(path)
				})
				if err == nil || err.Error() != "fixture-hash-mismatch" || report.Completed || calls != 0 {
					t.Fatalf("hash mismatch admitted: %+v %v calls=%d", report, err, calls)
				}
				if _, err := os.Stat(out); !os.IsNotExist(err) {
					t.Fatal("evidence written before hash admission", err)
				}
			})
		}
	}
}

// A strict-signature refusal at any of the four positions terminates diagnostics
// with incomplete evidence; it must never be downgraded to a finding.
func TestStrictSignatureRefusal(t *testing.T) {
	for failedCall := 1; failedCall <= 4; failedCall++ {
		t.Run(fmt.Sprint(failedCall), func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "evidence")
			calls := 0
			_, err := compare(fixturePair(t, "N"), fixturePair(t, "N+1"), out, func(path string) (signature, error) {
				calls++
				if calls == failedCall {
					return signature{}, errors.New("fixture-strict-signature")
				}
				return signatureFixture(path)
			})
			saved := readReport(t, out)
			if err == nil || !strings.Contains(err.Error(), "fixture-strict-signature") || saved.Completed || saved.ReleaseVerified ||
				saved.Error == "" || calls != failedCall {
				t.Fatalf("strict failure downgraded: %+v %v calls=%d", saved, err, calls)
			}
		})
	}
}

// A missing/extra image on either side fails closed, including a substituted
// filename, symlink, directory, or nonexecutable member.
func TestFixtureMemberRefusals(t *testing.T) {
	for side := range 2 {
		for _, kind := range []string{"missing", "extra", "substitute", "symlink", "directory", "nonexecutable"} {
			t.Run(fmt.Sprintf("%d/%s", side, kind), func(t *testing.T) {
				dirs := [2]string{fixturePair(t, "N"), fixturePair(t, "N+1")}
				path := filepath.Join(dirs[side], names[1])
				switch kind {
				case "missing":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "extra":
					if err := os.WriteFile(filepath.Join(dirs[side], "extra-image"), []byte("extra"), 0555); err != nil {
						t.Fatal(err)
					}
				case "substitute":
					if err := os.Rename(path, path+".other"); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Join(dirs[1-side], names[1]), path); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				case "nonexecutable":
					if err := os.Chmod(path, 0444); err != nil {
						t.Fatal(err)
					}
				}
				calls := 0
				_, err := compare(dirs[0], dirs[1], filepath.Join(t.TempDir(), "evidence"), func(path string) (signature, error) {
					calls++
					return signatureFixture(path)
				})
				if err == nil || calls != 0 {
					t.Fatalf("bad member admitted: %v calls=%d", err, calls)
				}
			})
		}
	}
}

// Only two exact named lowercase SHA256 records are admitted, in either order;
// duplicates, traversal, omitted/extra records and malformed digests are refused.
func TestPinManifest(t *testing.T) {
	a, b := strings.Repeat("a", 64)+"  a2a", strings.Repeat("b", 64)+"  a2a-keychain-status"
	for _, valid := range []string{a + "\n" + b + "\n", b + "\n" + a, a + "\n" + b} {
		if _, err := parsePins([]byte(valid)); err != nil {
			t.Fatal(err)
		}
	}
	for _, invalid := range []string{
		a, a + "\n" + a, a + "\n" + b + "\n" + a, a + "\n" + b + "\n\n", a + "\n" + strings.Replace(b, "a2a-keychain-status", "../a2a-keychain-status", 1),
		strings.ToUpper(a) + "\n" + b, "bad  a2a\n" + b, strings.Replace(a, "  ", " ", 1) + "\n" + b,
	} {
		if _, err := parsePins([]byte(invalid)); err == nil {
			t.Fatalf("manifest admitted: %q", invalid)
		}
	}
}

type fileWitness struct {
	hash  string
	mtime time.Time
	mode  os.FileMode
}

func witness(t *testing.T, dir string) map[string]fileWitness {
	t.Helper()
	out := map[string]fileWitness{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		out[entry.Name()] = fileWitness{hash(b), st.ModTime(), st.Mode()}
	}
	return out
}

func assertWitness(t *testing.T, dir string, want map[string]fileWitness) {
	t.Helper()
	got := witness(t, dir)
	if len(got) != len(want) {
		t.Fatal("input members changed")
	}
	for name, before := range want {
		after := got[name]
		if before.hash != after.hash || !before.mtime.Equal(after.mtime) || before.mode != after.mode {
			t.Fatalf("input changed: %s", name)
		}
	}
}

// Input bytes, mode and mtime remain identical on success and native refusal.
// The native boundary sees only exact byte copies under the evidence directory.
func TestReadOnlyInputs(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			dirs := [2]string{fixturePair(t, "N"), fixturePair(t, "N+1")}
			before := [2]map[string]fileWitness{witness(t, dirs[0]), witness(t, dirs[1])}
			out := filepath.Join(t.TempDir(), "evidence")
			_, err := compare(dirs[0], dirs[1], out, func(path string) (signature, error) {
				resolved, err := filepath.EvalSymlinks(out)
				if err != nil {
					t.Fatal(err)
				}
				rel, err := filepath.Rel(resolved, path)
				if err != nil || strings.HasPrefix(rel, "..") {
					t.Fatal("native inspection outside evidence", path)
				}
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				side := 0
				if filepath.Base(filepath.Dir(path)) == "1" {
					side = 1
				}
				if hash(b) != before[side][filepath.Base(path)].hash {
					t.Fatal("snapshot differs")
				}
				if fail {
					return signature{}, errors.New("fixture-strict-signature")
				}
				return signatureFixture(path)
			})
			if (err != nil) != fail {
				t.Fatal(err)
			}
			for side, dir := range dirs {
				assertWitness(t, dir, before[side])
			}
		})
	}
}

// Evidence must be new and outside both input trees, including aliases. A
// preexisting hard link in evidence cannot cause the comparator to overwrite N.
func TestEvidenceIsolation(t *testing.T) {
	for _, kind := range []string{"fresh", "under-N", "under-next", "alias-N", "existing-hardlink"} {
		t.Run(kind, func(t *testing.T) {
			dirs := [2]string{fixturePair(t, "N"), fixturePair(t, "N+1")}
			before := [2]map[string]fileWitness{witness(t, dirs[0]), witness(t, dirs[1])}
			parent := t.TempDir()
			out := filepath.Join(parent, "evidence")
			switch kind {
			case "under-N":
				out = filepath.Join(dirs[0], "evidence")
			case "under-next":
				out = filepath.Join(dirs[1], "evidence")
			case "alias-N":
				alias := filepath.Join(parent, "alias")
				if err := os.Symlink(dirs[0], alias); err != nil {
					t.Fatal(err)
				}
				out = filepath.Join(alias, "evidence")
			case "existing-hardlink":
				if err := os.Mkdir(out, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(filepath.Join(dirs[0], names[0]), filepath.Join(out, "fixture-comparison.json")); err != nil {
					t.Fatal(err)
				}
			}
			_, err := compare(dirs[0], dirs[1], out, signatureFixture)
			if (err == nil) != (kind == "fresh") {
				t.Fatalf("isolation %s: %v", kind, err)
			}
			for side, dir := range dirs {
				assertWitness(t, dir, before[side])
			}
		})
	}
}

// A concurrent change to original bytes, mtimes or snapshots must invalidate
// completed evidence. This detects mutation; it does not restore external edits.
func TestChangedInputOrSnapshotRefused(t *testing.T) {
	for _, target := range []string{"input-bytes", "input-mtime", "snapshot", "empty-DR"} {
		t.Run(target, func(t *testing.T) {
			dirs := [2]string{fixturePair(t, "N"), fixturePair(t, "N+1")}
			out := filepath.Join(t.TempDir(), "evidence")
			_, err := compare(dirs[0], dirs[1], out, func(path string) (signature, error) {
				sig, _ := signatureFixture(path)
				switch target {
				case "input-bytes":
					if err := os.WriteFile(filepath.Join(dirs[0], names[0]), []byte("changed"), 0555); err != nil {
						t.Fatal(err)
					}
				case "input-mtime":
					at := time.Unix(100, 0)
					if err := os.Chtimes(filepath.Join(dirs[0], names[0]), at, at); err != nil {
						t.Fatal(err)
					}
				case "snapshot":
					if err := os.Chmod(path, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("changed snapshot"), 0500); err != nil {
						t.Fatal(err)
					}
				case "empty-DR":
					sig.compiled = nil
				}
				return sig, nil
			})
			saved := readReport(t, out)
			if err == nil || saved.Completed || saved.Error == "" {
				t.Fatalf("mutation admitted: %+v %v", saved, err)
			}
		})
	}
}

// The production boundary admits ordinary standalone use only; every configured
// CI/release/signing marker, even "false", refuses before opening any inputs.
func TestStandaloneAdmission(t *testing.T) {
	if err := StandaloneEnvironment(func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	keys := []string{"CI", "GITHUB_ACTIONS", "RUNNER_ENVIRONMENT", "RUNNER_TEMP", "RELEASE_ADMITTED_TAG_OBJECT", "RELEASE_TAG", "MACOS_SIGN_P12", "MACOS_SIGN_P12_PASSWORD", "ASC_ISSUER_ID", "ASC_KEY_ID", "ASC_KEY_P8"}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			for _, value := range []string{"true", "false", "fixture-value"} {
				err := StandaloneEnvironment(func(k string) string {
					if k == key {
						return value
					}
					return ""
				})
				if err == nil || !strings.Contains(err.Error(), Label) {
					t.Fatalf("%s=%s admitted: %v", key, value, err)
				}
			}
		})
	}
}

// Byte-identical pinned fixtures are valid diagnostic inputs; release CompareDR
// intentionally imposes a separate distinct-image condition.
func TestIdenticalFixtures(t *testing.T) {
	dir := fixturePair(t, "same")
	report, err := compare(dir, dir, filepath.Join(t.TempDir(), "evidence"), signatureFixture)
	if err != nil || len(report.Findings) != 0 || !report.Completed || report.ReleaseVerified {
		t.Fatalf("%+v %v", report, err)
	}
}

// An inspector failure cannot manufacture successful raw or compiled evidence.
func TestMissingNativeRequirement(t *testing.T) {
	for _, sig := range []signature{{}, {raw: "text"}, {compiled: []byte("AST")}} {
		_, err := compare(fixturePair(t, "N"), fixturePair(t, "N+1"), filepath.Join(t.TempDir(), "evidence"), func(string) (signature, error) { return sig, nil })
		if err == nil || !strings.Contains(err.Error(), "fixture-designated-requirement-missing") {
			t.Fatal(err)
		}
	}
}

// The hash manifest copied into evidence is exactly the supplied independent pin
// set; it is not recomputed to conceal mismatching original hashes.
func TestRetainedPins(t *testing.T) {
	old, next := fixturePair(t, "N"), fixturePair(t, "N+1")
	out := filepath.Join(t.TempDir(), "evidence")
	if _, err := compare(old, next, out, signatureFixture); err != nil {
		t.Fatal(err)
	}
	for i, dir := range []string{old, next} {
		want, _ := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
		got, err := os.ReadFile(filepath.Join(out, fmt.Sprint(i), "SHA256SUMS"))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("pins changed", err)
		}
	}
}
