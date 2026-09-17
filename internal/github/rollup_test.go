package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/guygrigsby/autophage/internal/upkeep"
)

// checksServer answers the combined status and check-runs endpoints for one
// sha with whatever the test scripts.
func checksServer(t *testing.T, status map[string]any, runs map[string]any) *httptest.Server {
	t.Helper()
	f, srv := newFakeGitHub(t)
	_ = f
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/guy/repo/commits/{sha}/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(status)
	})
	mux.HandleFunc("GET /repos/guy/repo/commits/{sha}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(runs)
	})
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_test", "expires_at": "2099-01-01T00:00:00Z"})
	})
	mux.HandleFunc("GET /repos/guy/repo/pulls/11", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 11, "state": "open", "merged": false,
			"user": map[string]any{"login": "dependabot[bot]"},
			"head": map[string]any{"ref": "dependabot/x", "sha": headA},
			"base": map[string]any{"ref": "main"},
		})
	})
	srv.Config.Handler = mux
	return srv
}

func emptyRuns() map[string]any {
	return map[string]any{"total_count": 0, "check_runs": []any{}}
}

func TestCheckRollupIsPendingWhileAnythingRuns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status map[string]any
		runs   map[string]any
	}{
		{"a status is pending",
			map[string]any{"state": "pending", "statuses": []any{map[string]any{"context": "ci/build", "state": "pending"}}},
			emptyRuns()},
		{"a check run has not completed",
			map[string]any{"state": "success", "statuses": []any{}},
			map[string]any{"total_count": 1, "check_runs": []any{map[string]any{"name": "test", "status": "in_progress"}}}},
		{"a check run is queued",
			map[string]any{"state": "success", "statuses": []any{}},
			map[string]any{"total_count": 1, "check_runs": []any{map[string]any{"name": "test", "status": "queued"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := checksServer(t, tc.status, tc.runs)
			c := newTestClient(t, srv)
			roll, err := c.CheckRollup(t.Context(), "guy/repo", headA)
			if err != nil {
				t.Fatal(err)
			}
			if roll.Conclusive {
				t.Errorf("rollup = %+v, want inconclusive", roll)
			}
		})
	}
}

// GitHub has eight conclusions; Upkeep has two. Anything that did not run is
// not a failure, and anything cancelled leaves the rollup undecided rather
// than calling a build red that nobody finished.
func TestCheckRollupCollapsesGitHubsConclusions(t *testing.T) {
	for _, tc := range []struct {
		conclusion string
		conclusive bool
		want       upkeep.CheckConclusion
	}{
		{"success", true, upkeep.CheckSuccess},
		{"skipped", true, upkeep.CheckSuccess},
		{"neutral", true, upkeep.CheckSuccess},
		{"failure", true, upkeep.CheckFailure},
		{"timed_out", true, upkeep.CheckFailure},
		{"action_required", true, upkeep.CheckFailure},
		{"cancelled", false, ""},
		{"stale", false, ""},
	} {
		t.Run(tc.conclusion, func(t *testing.T) {
			srv := checksServer(t,
				map[string]any{"state": "success", "statuses": []any{}},
				map[string]any{"total_count": 1, "check_runs": []any{
					map[string]any{"name": "test (1.26)", "status": "completed", "conclusion": tc.conclusion, "html_url": "https://x"},
				}})
			c := newTestClient(t, srv)
			roll, err := c.CheckRollup(t.Context(), "guy/repo", headA)
			if err != nil {
				t.Fatal(err)
			}
			if roll.Conclusive != tc.conclusive {
				t.Fatalf("conclusive = %v, want %v", roll.Conclusive, tc.conclusive)
			}
			if !tc.conclusive {
				return
			}
			if roll.Conclusion != tc.want {
				t.Errorf("conclusion = %s, want %s", roll.Conclusion, tc.want)
			}
			if tc.want == upkeep.CheckFailure {
				if len(roll.FailingContexts) != 1 || roll.FailingContexts[0] != "test (1.26)" {
					t.Errorf("failing contexts = %v, want the run name", roll.FailingContexts)
				}
			} else if len(roll.FailingContexts) != 0 {
				t.Errorf("failing contexts = %v, want none on success", roll.FailingContexts)
			}
		})
	}
}

// A repository with no checks at all concludes nothing, so the bump waits and
// the sweeper's deadline is what ends it. Calling "no checks" success would
// declare every unverified bump green.
func TestCheckRollupWithNoChecksIsInconclusive(t *testing.T) {
	srv := checksServer(t, map[string]any{"state": "pending", "statuses": []any{}}, emptyRuns())
	c := newTestClient(t, srv)
	roll, err := c.CheckRollup(t.Context(), "guy/repo", headA)
	if err != nil {
		t.Fatal(err)
	}
	if roll.Conclusive {
		t.Errorf("rollup = %+v, want inconclusive", roll)
	}
}

func TestCheckRollupNamesEveryFailingRun(t *testing.T) {
	srv := checksServer(t,
		map[string]any{"state": "failure", "statuses": []any{
			map[string]any{"context": "ci/legacy", "state": "failure", "target_url": "https://legacy"},
		}},
		map[string]any{"total_count": 2, "check_runs": []any{
			map[string]any{"name": "test (1.26)", "status": "completed", "conclusion": "failure", "html_url": "https://x"},
			map[string]any{"name": "lint", "status": "completed", "conclusion": "success"},
		}})
	c := newTestClient(t, srv)
	roll, err := c.CheckRollup(t.Context(), "guy/repo", headA)
	if err != nil {
		t.Fatal(err)
	}
	if !roll.Conclusive || roll.Conclusion != upkeep.CheckFailure {
		t.Fatalf("rollup = %+v", roll)
	}
	want := map[string]bool{"test (1.26)": true, "ci/legacy": true}
	if len(roll.FailingContexts) != 2 {
		t.Fatalf("failing contexts = %v, want both the run and the status", roll.FailingContexts)
	}
	for _, c := range roll.FailingContexts {
		if !want[c] {
			t.Errorf("failing context %q not expected", c)
		}
	}
	if roll.DetailsURL == "" {
		t.Error("no details url for a human to follow")
	}
}

func TestGetPullRequestReadsTheHead(t *testing.T) {
	srv := checksServer(t, map[string]any{"state": "success", "statuses": []any{}}, emptyRuns())
	c := newTestClient(t, srv)
	pr, err := c.GetPullRequest(t.Context(), "guy/repo", 11)
	if err != nil {
		t.Fatal(err)
	}
	if pr.HeadSha != headA || pr.HeadBranch != "dependabot/x" || pr.BaseBranch != "main" {
		t.Errorf("pull request = %+v", pr)
	}
	if pr.AuthorLogin != "dependabot[bot]" || !pr.Open || pr.Merged {
		t.Errorf("pull request = %+v", pr)
	}
}
