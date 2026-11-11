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
