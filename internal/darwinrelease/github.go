package darwinrelease

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type githubAPI struct {
	base, token string
	client      *http.Client
}

func newGitHub(token string) *githubAPI {
	return &githubAPI{"https://api.github.com", token, &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("API redirect refused") }}}
}
func (g *githubAPI) request(ctx context.Context, method, path string, body any, out any) (int, error) {
	var data []byte
	var e error
	if body != nil {
		data, e = json.Marshal(body)
		if e != nil {
			return 0, errors.New("api-body")
		}
	}
	req, e := http.NewRequestWithContext(ctx, method, g.base+path, bytes.NewReader(data))
	if e != nil {
		return 0, errors.New("api-request")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	r, e := g.client.Do(req)
	if e != nil {
		return 0, errors.New("api-read")
	}
	defer r.Body.Close()
	if r.StatusCode == 404 {
		return 404, nil
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return r.StatusCode, errors.New("api-status")
	}
	if out != nil {
		b, e := io.ReadAll(io.LimitReader(r.Body, 4<<20+1))
		if e != nil || len(b) > 4<<20 || json.Unmarshal(b, out) != nil {
			return r.StatusCode, errors.New("api-malformed")
		}
	}
	return r.StatusCode, nil
}
func (g *githubAPI) get(ctx context.Context, path string, out any) error {
	s, e := g.request(ctx, "GET", path, nil, out)
	if e != nil {
		return e
	}
	if s != 200 {
		return errors.New("api-missing")
	}
	return nil
}

func (g *githubAPI) validate(ctx context.Context, p Policy, a *Admission) error {
	prefix := "/repos/" + p.Repository
	var ref struct{ Object struct{ SHA, Type string } }
	if e := g.get(ctx, prefix+"/git/ref/tags/"+url.PathEscape(a.Tag), &ref); e != nil {
		return e
	}
	if ref.Object.Type != "tag" || ref.Object.SHA != a.TagObject {
		return errors.New("remote-tag-moved")
	}
	var main struct {
		Name   string
		Commit struct{ SHA string }
	}
	if e := g.get(ctx, prefix+"/branches/main", &main); e != nil {
		return e
	}
	if main.Name != "main" || main.Commit.SHA != a.Commit {
		return errors.New("off-current-main")
	}
	required := map[string]int64{}
	for _, name := range p.RequiredChecks {
		if name == "" {
			return ErrPolicy
		}
		required[name] = 15368
	} // GitHub Actions' public app ID.
	// Query the ref's legacy rule without the administration-only REST
	// protection endpoint. GraphQL errors/missing selected fields refuse.
	const query = `query { repository(owner:"relux-works", name:"a2a-swarm") { nameWithOwner ref(qualifiedName:"refs/heads/main") { target { ... on Commit { oid } } branchProtectionRule { requiresStatusChecks requiredStatusCheckContexts requiredStatusChecks { context app { databaseId } } } } } }`
	var graph struct {
		Errors []json.RawMessage
		Data   struct {
			Repository *struct {
				Name string `json:"nameWithOwner"`
				Ref  *struct {
					Target struct{ OID string }
					Rule   json.RawMessage `json:"branchProtectionRule"`
				}
			}
		}
	}
	code, e := g.request(ctx, "POST", "/graphql", map[string]string{"query": query}, &graph)
	if e != nil {
		return e
	}
	if code != 200 || len(graph.Errors) != 0 || graph.Data.Repository == nil || graph.Data.Repository.Name != p.Repository || graph.Data.Repository.Ref == nil || graph.Data.Repository.Ref.Target.OID != a.Commit || len(graph.Data.Repository.Ref.Rule) == 0 {
		return errors.New("required-policy-read")
	}
	if string(graph.Data.Repository.Ref.Rule) != "null" {
		var rule struct {
			Requires *bool    `json:"requiresStatusChecks"`
			Contexts []string `json:"requiredStatusCheckContexts"`
			Checks   []struct {
				Context string
				App     *struct {
					ID int64 `json:"databaseId"`
				}
			} `json:"requiredStatusChecks"`
		}
		if json.Unmarshal(graph.Data.Repository.Ref.Rule, &rule) != nil || rule.Requires == nil {
			return errors.New("required-policy-malformed")
		}
		if *rule.Requires && len(rule.Contexts) == 0 && len(rule.Checks) == 0 {
			return errors.New("required-policy-malformed")
		}
		if *rule.Requires {
			for _, check := range rule.Checks {
				if check.Context == "" {
					return errors.New("required-policy-malformed")
				}
				app := int64(15368)
				if check.App != nil {
					app = check.App.ID
				}
				if e = addRequired(required, check.Context, app); e != nil {
					return e
				}
			}
			for _, name := range rule.Contexts {
				if name == "" {
					return errors.New("required-policy-malformed")
				}
				if _, exists := required[name]; !exists {
					required[name] = 15368
				}
			}

		}
	}
	type branchRule struct {
		Type       string
		Parameters struct {
			Checks []struct {
				Context string
				AppID   int64 `json:"integration_id"`
			} `json:"required_status_checks"`
		}
	}
	for page := 1; page <= 20; page++ {
		var rules []branchRule
		if e = g.get(ctx, fmt.Sprintf("%s/rules/branches/main?per_page=100&page=%d", prefix, page), &rules); e != nil {
			return e
		}
		if rules == nil {
			return errors.New("required-policy-malformed")
		}
		for _, rule := range rules {
			if rule.Type == "" {
				return errors.New("required-policy-malformed")
			}
			switch rule.Type {
			case "required_status_checks":
			case "creation", "update", "deletion", "non_fast_forward", "required_signatures", "required_linear_history", "pull_request", "required_deployments", "commit_message_pattern", "commit_author_email_pattern", "committer_email_pattern", "commit_committer_email_pattern", "branch_name_pattern":
				continue
			default:
				return errors.New("required-policy-unsupported")
			}
			if rule.Type == "required_status_checks" {
				if len(rule.Parameters.Checks) == 0 {
					return errors.New("required-policy-malformed")
				}
				for _, check := range rule.Parameters.Checks {
					if check.Context == "" || check.AppID < 0 {
						return errors.New("required-policy-malformed")
					}
					app := check.AppID
					if app == 0 {
						app = 15368
					}
					if e = addRequired(required, check.Context, app); e != nil {
						return e
					}
				}
			}
		}
		if len(rules) < 100 {
			break
		}
		if page == 20 {
			return errors.New("required-policy-incomplete")
		}
	}
	type checkRun struct {
		ID                       int64
		Name, Status, Conclusion string
		HeadSHA                  string `json:"head_sha"`
		Details                  string `json:"details_url"`
		App                      struct{ ID int64 }
	}
	all := []checkRun{}
	for page := 1; page <= 20; page++ {
		var runs struct {
			Total  *int       `json:"total_count"`
			Checks []checkRun `json:"check_runs"`
		}
		if e = g.get(ctx, fmt.Sprintf("%s/commits/%s/check-runs?filter=latest&per_page=100&page=%d", prefix, a.Commit, page), &runs); e != nil {
			return e
		}
		if runs.Total == nil || *runs.Total < 0 {
			return errors.New("checks-malformed")
		}
		all = append(all, runs.Checks...)
		if len(all) == *runs.Total {
			break
		}
		if len(runs.Checks) != 100 || page == 20 || len(all) > *runs.Total {
			return errors.New("checks-incomplete")
		}
	}
	a.Checks = map[string]int64{}
	for name, app := range required {
		n := 0
		for _, check := range all {
			if check.Name == name && check.App.ID == app {
				n++
				if check.ID <= 0 || check.HeadSHA != a.Commit || check.Status != "completed" || check.Conclusion != "success" {
					return errors.New("required-check-not-success")
				}
				if app == 15368 {
					if e = g.actionsJob(ctx, p.Repository, a.Commit, name, check.ID, check.Details); e != nil {
						return e
					}
				}
				a.Checks[name] = check.ID
			}
		}
		if n != 1 {
			return errors.New("required-check-missing-or-ambiguous")
		}
	}
	type pull struct {
		Number   int
		MergedAt *string `json:"merged_at"`
		Base     struct {
			Ref  string
			Repo struct {
				FullName string `json:"full_name"`
			}
		}
		Head struct{ SHA string }
	}
	var pulls []pull
	if e = g.get(ctx, prefix+"/commits/"+a.Commit+"/pulls?per_page=100", &pulls); e != nil {
		return e
	}
	if len(pulls) >= 100 {
		return errors.New("review-incomplete")
	}
	for _, pr := range pulls {
		if pr.Number <= 0 || pr.MergedAt == nil || *pr.MergedAt == "" || pr.Base.Ref != "main" || pr.Base.Repo.FullName != p.Repository || pr.Head.SHA != a.Commit {
			continue
		}
		type review struct {
			ID          int64
			State, Body string
			Commit      string `json:"commit_id"`
			User        struct{ Login string }
		}
		var reviews []review
		if e = g.get(ctx, fmt.Sprintf("%s/pulls/%d/reviews?per_page=100", prefix, pr.Number), &reviews); e != nil {
			return e
		}
		if len(reviews) >= 100 {
			return errors.New("review-incomplete")
		}
		// Latest verdict by the recorded reviewer is authoritative. COMMENTED accepts
		// an exact explicit marker so the bot never invents an author approval.
		var latest review
		for _, r := range reviews {
			if r.User.Login == p.Reviewer && r.Commit == a.Commit && r.ID > latest.ID {
				latest = r
			}
		}
		marker := "Release-Review: ACCEPT " + a.Commit
		accepted := latest.State == "APPROVED"
		if latest.State == "COMMENTED" {
			for _, line := range strings.Split(latest.Body, "\n") {
				if line == marker {
					accepted = true
				}
			}
		}
		if latest.ID > 0 && accepted {
			a.ReviewID = latest.ID
			return nil
		}
	}
	return errors.New("exact-main-review-missing")
}

func addRequired(required map[string]int64, name string, app int64) error {
	if name == "" || app <= 0 {
		return errors.New("required-policy-malformed")
	}
	if prior, exists := required[name]; exists && prior != app {
		return errors.New("required-policy-conflict")
	}
	required[name] = app
	return nil
}

// A check bearing the Actions app ID must also be the actual successful job
// from a successful main-push run at this SHA, not an arbitrary check API write.
func (g *githubAPI) actionsJob(ctx context.Context, repo, sha, name string, checkID int64, details string) error {
	prefix := "https://github.com/" + repo + "/"
	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `(?:actions/runs/([0-9]+)/job/([0-9]+)|runs/([0-9]+)/jobs/([0-9]+))$`)
	match := pattern.FindStringSubmatch(details)
	if match == nil {
		return errors.New("required-check-not-legitimate")
	}
	runText, jobText := match[1], match[2]
	if runText == "" {
		runText, jobText = match[3], match[4]
	}
	runID, e := strconv.ParseInt(runText, 10, 64)
	if e != nil || runID <= 0 {
		return errors.New("required-check-not-legitimate")
	}
	jobID, e := strconv.ParseInt(jobText, 10, 64)
	if e != nil || jobID <= 0 {
		return errors.New("required-check-not-legitimate")
	}
	var job struct {
		ID                       int64
		RunID                    int64 `json:"run_id"`
		Name, Status, Conclusion string
		HeadSHA                  string `json:"head_sha"`
		CheckURL                 string `json:"check_run_url"`
	}
	if e = g.get(ctx, fmt.Sprintf("/repos/%s/actions/jobs/%d", repo, jobID), &job); e != nil {
		return e
	}
	if job.ID != jobID || job.RunID != runID || job.Name != name || job.Status != "completed" || job.Conclusion != "success" || job.HeadSHA != sha || job.CheckURL != fmt.Sprintf("https://api.github.com/repos/%s/check-runs/%d", repo, checkID) {
		return errors.New("required-check-not-legitimate")
	}
	var run struct {
		ID                              int64
		HeadSHA                         string `json:"head_sha"`
		Branch                          string `json:"head_branch"`
		Event, Status, Conclusion, Path string
		Repo                            struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		HeadRepo struct {
			FullName string `json:"full_name"`
		} `json:"head_repository"`
	}
	if e = g.get(ctx, fmt.Sprintf("/repos/%s/actions/runs/%d", repo, runID), &run); e != nil {
		return e
	}
	workflow, _, _ := strings.Cut(run.Path, "@")
	if run.ID != runID || run.HeadSHA != sha || run.Branch != "main" || run.Event != "push" || run.Status != "completed" || run.Conclusion != "success" || run.Repo.FullName != repo || run.HeadRepo.FullName != repo || !regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9_-]+\.ya?ml$`).MatchString(workflow) {
		return errors.New("required-check-not-legitimate")
	}
	return nil
}
