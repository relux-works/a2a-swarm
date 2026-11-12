package darwinrelease

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// HTTP fixtures exercise the actual admission client, including malformed/API
// failures and unknown/pending/skipped checks. They do not prove live GitHub policy.
func TestRequiredChecksAndReview(t *testing.T) {
	sha, tagObject := strings.Repeat("a", 40), strings.Repeat("b", 40)
	cases := []struct{ name, want string }{
		{"valid", ""}, {"off-main", "off-current-main"}, {"moved-tag", "remote-tag-moved"},
		{"pending", "required-check-not-success"}, {"failed", "required-check-not-success"}, {"skipped", "required-check-not-success"}, {"unknown", "required-check-not-success"},
		{"missing", "required-check-missing-or-ambiguous"}, {"wrong-app", "required-check-missing-or-ambiguous"}, {"duplicate", "required-check-missing-or-ambiguous"},
		{"api-error", "api-status"}, {"malformed", "api-malformed"}, {"unknown-policy", "required-policy-malformed"}, {"unsupported-policy", "required-policy-unsupported"}, {"no-review", "exact-main-review-missing"}, {"graphql-error", "required-policy-read"}, {"legacy-required", "required-check-missing-or-ambiguous"}, {"legacy-required-valid", ""}, {"rules-required", "required-check-missing-or-ambiguous"}, {"rules-required-valid", ""}, {"check-sha", "required-check-not-success"}, {"checks-incomplete", "checks-incomplete"}, {"review-withdrawn", "exact-main-review-missing"}, {"fake-actions-check", "required-check-not-legitimate"}, {"workflow-off-main", "required-check-not-legitimate"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.name == "api-error" {
					w.WriteHeader(503)
					return
				}
				if c.name == "malformed" {
					w.Write([]byte("{"))
					return
				}
				var body any
				switch {
				case strings.Contains(r.URL.Path, "/git/ref/"):
					object := tagObject
					if c.name == "moved-tag" {
						object = strings.Repeat("c", 40)
					}
					body = map[string]any{"object": map[string]any{"sha": object, "type": "tag"}}
				case r.URL.Path == "/repos/relux-works/a2a-swarm/branches/main":
					head := sha
					if c.name == "off-main" {
						head = strings.Repeat("c", 40)
					}
					body = map[string]any{"name": "main", "commit": map[string]string{"sha": head}}
				case r.URL.Path == "/graphql":
					body = map[string]any{"data": map[string]any{"repository": map[string]any{"nameWithOwner": "relux-works/a2a-swarm", "ref": map[string]any{"target": map[string]string{"oid": sha}, "branchProtectionRule": nil}}}}
					if strings.HasPrefix(c.name, "legacy-required") {
						body = map[string]any{"data": map[string]any{"repository": map[string]any{"nameWithOwner": "relux-works/a2a-swarm", "ref": map[string]any{"target": map[string]string{"oid": sha}, "branchProtectionRule": map[string]any{"requiresStatusChecks": true, "requiredStatusCheckContexts": []string{"additional"}, "requiredStatusChecks": []any{map[string]any{"context": "additional", "app": map[string]any{"databaseId": 15368}}}}}}}}
					}
					if c.name == "graphql-error" {
						body = map[string]any{"errors": []any{map[string]string{"message": "access refused"}}}
					}
				case strings.Contains(r.URL.Path, "/rules/branches/"):
					body = []any{}
					if strings.HasPrefix(c.name, "rules-required") {
						body = []any{map[string]any{"type": "required_status_checks", "parameters": map[string]any{"required_status_checks": []any{map[string]any{"context": "additional", "integration_id": 15368}}}}}
					}
					if c.name == "unsupported-policy" {
						body = []any{map[string]any{"type": "future-required-workflows"}}
					}
					if c.name == "unknown-policy" {
						body = []any{map[string]any{"type": "required_status_checks"}}
					}
				case strings.HasSuffix(r.URL.Path, "/check-runs"):
					status, conclusion, app := "completed", "success", int64(15368)
					if c.name == "pending" {
						status = "in_progress"
					}
					if c.name == "failed" {
						conclusion = "failure"
					}
					if c.name == "skipped" {
						conclusion = "skipped"
					}
					if c.name == "unknown" {
						conclusion = "unknown"
					}
					if c.name == "wrong-app" {
						app = 999
					}
					checks := []any{map[string]any{"id": 1, "name": "control", "status": status, "conclusion": conclusion, "head_sha": sha, "details_url": "https://github.com/relux-works/a2a-swarm/actions/runs/1/job/11", "app": map[string]any{"id": app}}}
					if c.name == "missing" {
						checks = nil
					}
					if c.name == "duplicate" {
						checks = append(checks, checks[0])
					}
					if c.name == "legacy-required-valid" || c.name == "rules-required-valid" {
						checks = append(checks, map[string]any{"id": 2, "name": "additional", "status": "completed", "conclusion": "success", "head_sha": sha, "details_url": "https://github.com/relux-works/a2a-swarm/actions/runs/1/job/12", "app": map[string]any{"id": 15368}})
					}
					if c.name == "check-sha" {
						checks[0].(map[string]any)["head_sha"] = strings.Repeat("d", 40)
					}
					total := len(checks)
					if c.name == "checks-incomplete" {
						total = 101
					}
					body = map[string]any{"total_count": total, "check_runs": checks}
				case strings.Contains(r.URL.Path, "/actions/jobs/"):
					id, check, name := 11, 1, "control"
					if strings.HasSuffix(r.URL.Path, "/12") {
						id, check, name = 12, 2, "additional"
					}
					if c.name == "fake-actions-check" {
						check = 999
					}
					body = map[string]any{"id": id, "run_id": 1, "name": name, "status": "completed", "conclusion": "success", "head_sha": sha, "check_run_url": fmt.Sprintf("https://api.github.com/repos/relux-works/a2a-swarm/check-runs/%d", check)}
				case strings.Contains(r.URL.Path, "/actions/runs/"):
					branch := "main"
					if c.name == "workflow-off-main" {
						branch = "feature"
					}
					body = map[string]any{"id": 1, "head_sha": sha, "head_branch": branch, "event": "push", "status": "completed", "conclusion": "success", "path": ".github/workflows/release-controls.yml", "repository": map[string]string{"full_name": "relux-works/a2a-swarm"}, "head_repository": map[string]string{"full_name": "relux-works/a2a-swarm"}}
				case strings.HasSuffix(r.URL.Path, "/pulls"):
					body = []any{map[string]any{"number": 1, "merged_at": "2026-01-01T00:00:00Z", "head": map[string]string{"sha": sha}, "base": map[string]any{"ref": "main", "repo": map[string]string{"full_name": "relux-works/a2a-swarm"}}}}
				case strings.HasSuffix(r.URL.Path, "/reviews"):
					marker := "Release-Review: ACCEPT " + sha
					if c.name == "no-review" {
						marker = "no acceptance"
					}
					body = []any{map[string]any{"id": 2, "state": "COMMENTED", "body": marker, "commit_id": sha, "user": map[string]string{"login": "reluxbot"}}}
					if c.name == "review-withdrawn" {
						body = append(body.([]any), map[string]any{"id": 3, "state": "CHANGES_REQUESTED", "body": "withdrawn", "commit_id": sha, "user": map[string]string{"login": "reluxbot"}})
					}
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					w.WriteHeader(500)
					return
				}
				json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			api := &githubAPI{server.URL, "fixture-token", server.Client()}
			a := Admission{Tag: "v0.1.0", TagObject: tagObject, Commit: sha}
			e := api.validate(context.Background(), Policy{Repository: "relux-works/a2a-swarm", Reviewer: "reluxbot", RequiredChecks: []string{"control"}}, &a)
			if c.want == "" {
				if e != nil || a.ReviewID != 2 || a.Checks["control"] != 1 {
					t.Fatal("valid admission not exercised", e)
				}
			} else if e == nil || e.Error() != c.want {
				t.Fatalf("wanted %s, got %v", c.want, e)
			}
		})
	}
}

// Actual-client HTTP fixtures prove main/PR selection and precise refusals, not
// live token readability or a real release. Only GET and policy-query POST are
// served; successful admission retains the exact main check IDs and review.
func TestReleaseChecksAcrossEvents(t *testing.T) {
	const repo = "relux-works/a2a-swarm"
	sha, tag := strings.Repeat("a", 40), strings.Repeat("b", 40)
	type record struct{ check, job, run map[string]any }
	makeRecord := func(id int, name, event string) record {
		branch := "main"
		if event == "pull_request" {
			branch = "feature"
		}
		return record{
			map[string]any{"id": id, "name": name, "head_sha": sha, "status": "completed", "conclusion": "success", "app": map[string]int{"id": 15368}, "details_url": fmt.Sprintf("https://github.com/%s/actions/runs/%d/job/%d", repo, id+100, id+200)},
			map[string]any{"id": id + 200, "run_id": id + 100, "name": name, "head_sha": sha, "status": "completed", "conclusion": "success", "check_run_url": fmt.Sprintf("https://api.github.com/repos/%s/check-runs/%d", repo, id)},
			map[string]any{"id": id + 100, "head_sha": sha, "head_branch": branch, "event": event, "status": "completed", "conclusion": "success", "path": ".github/workflows/go.yml", "repository": map[string]string{"full_name": repo}, "head_repository": map[string]string{"full_name": repo}},
		}
	}
	cases := []struct{ name, want string }{
		{"main-only", ""}, {"mixed", ""}, {"mixed-reversed", ""}, {"full-twelve", ""}, {"full-twelve-reversed", ""},
		{"pr-failure", ""}, {"pr-pending", ""}, {"pr-skipped", ""},
		{"pr-failure-reversed", ""}, {"pr-pending-reversed", ""}, {"pr-skipped-reversed", ""},
		{"pr-only", "required-check-missing-or-ambiguous"}, {"two-main", "required-check-missing-or-ambiguous"},
		{"main-failure", "required-check-not-success"}, {"main-pending", "required-check-not-success"}, {"main-skipped", "required-check-not-success"}, {"main-unknown", "required-check-not-success"},
		{"main-failure-beside-green", "required-check-not-success"}, {"main-pending-beside-green", "required-check-not-success"}, {"main-skipped-beside-green", "required-check-not-success"}, {"main-unknown-beside-green", "required-check-not-success"},
		{"main-job-failure", "required-check-not-legitimate"}, {"main-job-pending", "required-check-not-legitimate"}, {"main-job-skipped", "required-check-not-legitimate"},
		{"main-run-failure", "required-check-not-legitimate"}, {"main-run-pending", "required-check-not-legitimate"}, {"main-run-skipped", "required-check-not-legitimate"},
		{"main-job-failure-beside-green", "required-check-not-legitimate"}, {"main-run-pending-beside-green", "required-check-not-legitimate"},
		{"pr-check-sha", "required-check-not-success"}, {"pr-check-id", "required-check-not-success"},
		{"pr-job-id", "required-check-not-legitimate"}, {"pr-job-run", "required-check-not-legitimate"}, {"pr-job-name", "required-check-not-legitimate"}, {"pr-job-sha", "required-check-not-legitimate"}, {"pr-check-url", "required-check-not-legitimate"},
		{"pr-run-id", "required-check-not-legitimate"}, {"pr-run-sha", "required-check-not-legitimate"}, {"pr-repo", "required-check-not-legitimate"}, {"pr-head-repo", "required-check-not-legitimate"}, {"pr-workflow", "required-check-not-legitimate"}, {"pr-branch", "required-check-not-legitimate"}, {"pr-event", "required-check-not-legitimate"}, {"pr-unknown", "required-check-not-legitimate"}, {"pr-status-mismatch", "required-check-not-legitimate"}, {"pr-forged-url", "required-check-not-legitimate"},
		{"pr-job-api", "api-status"}, {"pr-run-api", "api-status"}, {"pr-job-malformed", "api-malformed"}, {"pr-run-malformed", "api-malformed"}, {"pr-job-missing", "api-missing"}, {"pr-run-incomplete", "required-check-not-legitimate"},
		{"wrong-app", "required-check-missing-or-ambiguous"}, {"withdrawn-review", "exact-main-review-missing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			main, pr := makeRecord(10, "test-linux", "push"), makeRecord(20, "test-linux", "pull_request")
			records := []record{main, pr}
			required := []string{"test-linux"}
			if c.name == "main-only" {
				records = records[:1]
			}
			if c.name == "pr-only" {
				records = records[1:]
			}
			if c.name == "two-main" {
				records[1] = makeRecord(30, "test-linux", "push")
			}
			if strings.HasPrefix(c.name, "full-twelve") {
				for i, name := range []string{"release-controls", "release-native-controls", "darwin-smoke", "test", "voice-native"} {
					a, b := makeRecord(40+i*2, name, "push"), makeRecord(41+i*2, name, "pull_request")
					if i < 2 {
						required = append(required, name)
					} else {
						a.check["conclusion"], a.job["conclusion"] = "skipped", "skipped"
						if name != "darwin-smoke" {
							b.check["conclusion"], b.job["conclusion"] = "skipped", "skipped"
						}
					}
					records = append(records, a, b)
				}
			}
			if strings.HasPrefix(c.name, "main-") && c.name != "main-only" {
				conclusion, status := "failure", "completed"
				if strings.Contains(c.name, "pending") {
					status = "in_progress"
				}
				if strings.Contains(c.name, "skipped") {
					conclusion = "skipped"
				}
				if strings.Contains(c.name, "unknown") {
					conclusion = "unknown"
				}
				bad := main
				if strings.HasSuffix(c.name, "beside-green") {
					bad = makeRecord(30, "test-linux", "push")
					records = append(records, bad)
				}
				// Check-only variants isolate its success gate; job/run variants
				// independently prove actual main execution must succeed too.
				target := bad.check
				if strings.HasPrefix(c.name, "main-job-") {
					target = bad.job
				}
				if strings.HasPrefix(c.name, "main-run-") {
					target = bad.run
				}
				if (strings.HasPrefix(c.name, "main-job-") || strings.HasPrefix(c.name, "main-run-")) && status == "in_progress" {
					conclusion = ""
				}
				target["status"], target["conclusion"] = status, conclusion
			}
			switch c.name {
			case "pr-failure", "pr-skipped", "pr-pending", "pr-failure-reversed", "pr-skipped-reversed", "pr-pending-reversed":
				status, conclusion := "completed", "failure"
				if strings.HasPrefix(c.name, "pr-skipped") {
					conclusion = "skipped"
				}
				if strings.HasPrefix(c.name, "pr-pending") {
					status, conclusion = "in_progress", ""
				}
				for _, m := range []map[string]any{pr.check, pr.job, pr.run} {
					m["status"], m["conclusion"] = status, conclusion
				}
			case "pr-check-sha":
				pr.check["head_sha"] = strings.Repeat("c", 40)
			case "pr-check-id":
				pr.check["id"] = 0
			case "pr-job-id":
				pr.job["id"] = 999
			case "pr-job-run":
				pr.job["run_id"] = 999
			case "pr-job-name":
				pr.job["name"] = "other"
			case "pr-job-sha":
				pr.job["head_sha"] = strings.Repeat("c", 40)
			case "pr-check-url":
				pr.job["check_run_url"] = "https://api.github.com/repos/other/project/check-runs/20"
			case "pr-run-id":
				pr.run["id"] = 999
			case "pr-run-sha":
				pr.run["head_sha"] = strings.Repeat("c", 40)
			case "pr-repo":
				pr.run["repository"] = map[string]string{"full_name": "other/project"}
			case "pr-head-repo":
				pr.run["head_repository"] = map[string]string{"full_name": "other/project"}
			case "pr-workflow":
				pr.run["path"] = "untrusted.yml"
			case "pr-branch":
				pr.run["head_branch"] = ""
			case "pr-event":
				pr.run["event"] = "workflow_dispatch"
			case "pr-unknown":
				pr.run["conclusion"] = "unknown"
			case "pr-status-mismatch":
				pr.check["conclusion"] = "failure"
			case "pr-forged-url":
				pr.check["details_url"] = "https://example.org/actions/runs/120/job/220"
			case "pr-run-incomplete":
				delete(pr.run, "event")
			case "wrong-app":
				main.check["app"] = map[string]int{"id": 999}
			}
			if strings.HasSuffix(c.name, "reversed") {
				for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
					records[i], records[j] = records[j], records[i]
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" && !(r.Method == "POST" && r.URL.Path == "/graphql") {
					t.Errorf("forbidden release/publication effect: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
					return
				}
				var body any
				switch r.URL.Path {
				case "/repos/" + repo + "/git/ref/tags/v0.1.0":
					body = map[string]any{"object": map[string]string{"type": "tag", "sha": tag}}
				case "/repos/" + repo + "/branches/main":
					body = map[string]any{"name": "main", "commit": map[string]string{"sha": sha}}
				case "/graphql":
					body = map[string]any{"data": map[string]any{"repository": map[string]any{"nameWithOwner": repo, "ref": map[string]any{"target": map[string]string{"oid": sha}, "branchProtectionRule": nil}}}}
				case "/repos/" + repo + "/rules/branches/main":
					body = []any{}
				case "/repos/" + repo + "/commits/" + sha + "/check-runs":
					checks := []any{}
					for _, rec := range records {
						checks = append(checks, rec.check)
					}
					body = map[string]any{"total_count": len(checks), "check_runs": checks}
				case "/repos/" + repo + "/commits/" + sha + "/pulls":
					body = []any{map[string]any{"number": 1, "merged_at": "2026-01-01T00:00:00Z", "head": map[string]string{"sha": sha}, "base": map[string]any{"ref": "main", "repo": map[string]string{"full_name": repo}}}}
				case "/repos/" + repo + "/pulls/1/reviews":
					body = []any{map[string]any{"id": 7, "state": "APPROVED", "commit_id": sha, "user": map[string]string{"login": "reluxbot"}}}
					if c.name == "withdrawn-review" {
						body = append(body.([]any), map[string]any{"id": 8, "state": "CHANGES_REQUESTED", "commit_id": sha, "user": map[string]string{"login": "reluxbot"}})
					}
				default:
					for _, rec := range records {
						jobPath := fmt.Sprintf("/repos/%s/actions/jobs/%v", repo, rec.check["id"].(int)+200)
						runPath := fmt.Sprintf("/repos/%s/actions/runs/%v", repo, rec.check["id"].(int)+100)
						if r.URL.Path != jobPath && r.URL.Path != runPath {
							continue
						}
						kind := "job"
						body = rec.job
						if r.URL.Path == runPath {
							kind, body = "run", rec.run
						}
						if rec.run["event"] == "pull_request" || rec.job["id"] == 220 {
							if c.name == "pr-"+kind+"-api" {
								w.WriteHeader(503)
								return
							}
							if c.name == "pr-"+kind+"-malformed" {
								w.Write([]byte("{"))
								return
							}
							if c.name == "pr-"+kind+"-missing" {
								w.WriteHeader(404)
								return
							}
						}
						break
					}
					if body == nil {
						t.Errorf("unexpected endpoint %s", r.URL.Path)
						w.WriteHeader(500)
						return
					}
				}
				json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			a := Admission{Tag: "v0.1.0", TagObject: tag, Commit: sha}
			api := &githubAPI{server.URL, "fixture-token", server.Client()}
			err := api.validate(context.Background(), Policy{Repository: repo, Reviewer: "reluxbot", RequiredChecks: required}, &a)
			if c.want != "" {
				if err == nil || err.Error() != c.want {
					t.Fatalf("wanted %s, got %v", c.want, err)
				}
				if a.ReviewID != 0 {
					t.Fatal("refused candidate retained accepted review")
				}
				return
			}
			if err != nil {
				t.Fatalf("main checks with authenticated PR neighbors must admit: %v", err)
			}
			if a.ReviewID != 7 || len(a.Checks) != len(required) {
				t.Fatalf("lost exact review/check set: %+v", a)
			}
			for _, rec := range records {
				if rec.run["event"] != "push" {
					continue
				}
				name := rec.check["name"].(string)
				for _, need := range required {
					if name == need && a.Checks[name] != int64(rec.check["id"].(int)) {
						t.Fatalf("selected PR/wrong check for %s: %v", name, a.Checks)
					}
				}
			}
		})
	}
}
