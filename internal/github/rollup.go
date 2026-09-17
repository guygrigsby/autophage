package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	gh "github.com/google/go-github/v88/github"

	"github.com/guygrigsby/autophage/internal/upkeep"
)

// ErrPullRequestNotFound marks GetPullRequest finding no such pull request
// (a 404), as opposed to any other upstream failure.
var ErrPullRequestNotFound = errors.New("pull request not found")

// PullRequestDetail is what Upkeep needs from GitHub about one pull request.
type PullRequestDetail struct {
	// Title is dependabot's own text, quoted into the repair brief inside a
	// fence. Untrusted: a version string is influenced by whoever publishes
	// the package.
	Title       string
	HeadBranch  string
	HeadSha     string
	BaseBranch  string
	AuthorLogin string
	Open        bool
	Merged      bool
}

func (c *Client) GetPullRequest(ctx context.Context, repository string, number int) (PullRequestDetail, error) {
	api, err := c.api(ctx, repository)
	if err != nil {
		return PullRequestDetail{}, err
	}
	owner, name := splitRepo(repository)
	pr, _, err := api.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		var ghErr *gh.ErrorResponse
		if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound {
			return PullRequestDetail{}, fmt.Errorf("github: get pull request %s#%d: %w", repository, number, ErrPullRequestNotFound)
		}
		return PullRequestDetail{}, fmt.Errorf("github: get pull request %s#%d: %w", repository, number, err)
	}
	return PullRequestDetail{
		Title:       pr.GetTitle(),
		HeadBranch:  pr.GetHead().GetRef(),
		HeadSha:     pr.GetHead().GetSHA(),
		BaseBranch:  pr.GetBase().GetRef(),
		AuthorLogin: pr.GetUser().GetLogin(),
		Open:        pr.GetState() == "open",
		Merged:      pr.GetMerged(),
	}, nil
}

// CheckRollup combines every check run and every commit status for one sha
// into Upkeep's two-value vocabulary.
//
// Both halves are read because a repository can use either: Actions reports
// check runs, an external CI reports commit statuses, and a repository with
// both would look green from one alone. Nothing is recorded unless every one
// of them has finished, which is why a single check_suite.completed is only
// a prompt to come and ask rather than a verdict in itself.
//
// GitHub's eight conclusions collapse like this. success, skipped and
// neutral are success: a job that was skipped did not fail. failure,
// timed_out and action_required are failure: all three mean a human has to
// look. cancelled and stale leave the rollup inconclusive, because nobody
// finished the work and calling that red would abandon bumps over a
// cancelled build.
func (c *Client) CheckRollup(ctx context.Context, repository, headSha string) (Rollup, error) {
	api, err := c.api(ctx, repository)
	if err != nil {
		return Rollup{}, err
	}
	owner, name := splitRepo(repository)

	var roll Rollup
	seen := 0
	failed := false
	// fail records one failing entry. An empty name would make the joined
	// FailingContexts empty, which the aggregate refuses as "set exactly for
	// a failure", and every delivery for that head would be rejected for
	// ever. A placeholder keeps the verdict recordable and says plainly that
	// GitHub did not name it.
	fail := func(label, url string) {
		if label == "" {
			label = "(unnamed check)"
		}
		failed = true
		roll.FailingContexts = append(roll.FailingContexts, label)
		if roll.DetailsURL == "" {
			roll.DetailsURL = url
		}
	}

	// Both halves are paginated. A single page is how a red bump gets called
	// green: GitHub defaults to 30 statuses a page, a large matrix runs well
	// past 100 check runs, and a failing or still-running entry beyond the
	// first page would simply not be seen.
	statusTotal := 0
	opt := &gh.ListOptions{PerPage: 100}
	for {
		combined, resp, err := api.Repositories.GetCombinedStatus(ctx, owner, name, headSha, opt)
		if err != nil {
			return Rollup{}, fmt.Errorf("github: combined status %s@%s: %w", repository, headSha, err)
		}
		statusTotal = combined.GetTotalCount()
		for _, st := range combined.Statuses {
			seen++
			switch st.GetState() {
			case "success":
			case "failure", "error":
				fail(st.GetContext(), st.GetTargetURL())
			default:
				// pending, or anything GitHub adds later: not finished.
				return Rollup{}, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	statusSeen := seen

	runTotal := 0
	runOpt := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := api.Checks.ListCheckRunsForRef(ctx, owner, name, headSha, runOpt)
		if err != nil {
			return Rollup{}, fmt.Errorf("github: check runs %s@%s: %w", repository, headSha, err)
		}
		runTotal = runs.GetTotal()
		for _, r := range runs.CheckRuns {
			seen++
			if r.GetStatus() != "completed" {
				return Rollup{}, nil
			}
			switch r.GetConclusion() {
			case "success", "skipped", "neutral":
			case "failure", "timed_out", "action_required":
				fail(r.GetName(), r.GetHTMLURL())
			default:
				// cancelled, stale, or an empty conclusion: undecided, so the
				// bump keeps waiting and the sweeper's deadline ends it if
				// nothing ever concludes.
				return Rollup{}, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		runOpt.Page = resp.NextPage
	}

	// GitHub says how many there are. Seeing fewer means a page went
	// missing, and a verdict drawn from a partial view is worse than no
	// verdict: the wait window ends an unconcluded bump, but a wrong
	// "success" ends it wrong and for good.
	if statusSeen < statusTotal || seen-statusSeen < runTotal {
		return Rollup{}, nil
	}
	if seen == 0 {
		// A repository that runs no checks on pull requests concludes
		// nothing. Calling that success would declare every unverified bump
		// green; the wait window is what ends these.
		return Rollup{}, nil
	}
	roll.Conclusive = true
	roll.Conclusion = upkeep.CheckSuccess
	if failed {
		roll.Conclusion = upkeep.CheckFailure
	}
	return roll, nil
}
