package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v88/github"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
	"github.com/guygrigsby/autophage/internal/upkeep"
)

// Rollup is the combined state of every check run and commit status for one
// head sha, in Upkeep's vocabulary rather than GitHub's.
type Rollup struct {
	// Conclusive is false while any run is queued or in progress, and the
	// caller then records nothing at all. A single check_suite.completed says
	// one suite finished; a head can have several and the payload never says
	// how many, so the suite event is only ever a prompt to come and ask.
	Conclusive      bool
	Conclusion      upkeep.CheckConclusion
	FailingContexts []string
	DetailsURL      string
}

// Roller reads the rollup for a head sha. Client implements it; the
// translator takes the interface so the rollup can be scripted in tests.
type Roller interface {
	CheckRollup(ctx context.Context, repository, headSha string) (Rollup, error)
}

// pullRequest translates a pull_request delivery. Everything that is not a
// dependabot pull request on a watched repository is ignored rather than
// rejected: nothing is wrong, autophage is simply not doing that one.
func (t *Translator) pullRequest(ctx context.Context, d store.Delivery, e *gh.PullRequestEvent) (string, error) {
	repo := e.GetRepo().GetFullName()
	number := e.GetNumber()
	pr := e.GetPullRequest()
	if login := pr.GetUser().GetLogin(); login != t.DependabotLogin {
		return fmt.Sprintf("pull request by %s", login), errUnsubscribed
	}
	switch e.GetAction() {
	case "opened", "reopened":
		return t.openBump(ctx, e)
	case "synchronize":
		head := pr.GetHead().GetSHA()
		_, err := t.Store.UpdateBump(ctx, repo, number, func(b *upkeep.Bump) error {
			return b.AdvanceHead(head, t.Clock.Now())
		})
		if errors.Is(err, store.ErrNotFound) {
			return "synchronize for unknown bump", err
		}
		return fmt.Sprintf("HeadAdvanced %s#%d to %s", repo, number, head), err
	case "closed":
		kind := upkeep.Discarded
		if pr.GetMerged() {
			kind = upkeep.Merged
		}
		cl := upkeep.BumpClosure{Kind: kind, DeliveryID: d.ID, ClosedAt: t.Clock.Now()}
		_, err := t.Store.UpdateBump(ctx, repo, number, func(b *upkeep.Bump) error { return b.Close(cl) })
		if errors.Is(err, store.ErrNotFound) {
			return "closed for unknown bump", err
		}
		return fmt.Sprintf("BumpClosed %s#%d (%s)", repo, number, kind), err
	default:
		return fmt.Sprintf("pull_request.%s", e.GetAction()), errUnsubscribed
	}
}

// openBump creates the bump. An unenrolled or unwatched repository and an
// existing bump are all ignored, not errors.
func (t *Translator) openBump(ctx context.Context, e *gh.PullRequestEvent) (string, error) {
	repo := e.GetRepo().GetFullName()
	number := e.GetNumber()
	pr := e.GetPullRequest()
	b, err := upkeep.NewBump(repo, number, pr.GetHead().GetRef(), pr.GetBase().GetRef(), pr.GetHead().GetSHA(), t.Clock.Now())
	if err != nil {
		return "BumpOpened", err
	}
	switch err := t.Store.CreateBump(ctx, b); {
	case errors.Is(err, store.ErrConflict):
		return "bump exists", err
	case errors.Is(err, store.ErrNotFound):
		// Not enrolled, or enrolled and not watched. Both mean the same to a
		// delivery: not our repository.
		return "repository not watched for upkeep", store.ErrNotFound
	case err != nil:
		return "BumpOpened", err
	}
	return fmt.Sprintf("BumpOpened %s#%d on %s", repo, number, b.Branch()), nil
}

// checks translates a check_suite or workflow_run completion. The event is a
// prompt, not a verdict: the translator asks GitHub for the combined rollup
// and records a verdict only when nothing is still running.
func (t *Translator) checks(ctx context.Context, repository, headSha string, numbers []int) (string, error) {
	if len(numbers) == 0 {
		return "check completion with no pull request", errUnsubscribed
	}
	details := make([]string, 0, len(numbers))
	for _, number := range numbers {
		b, err := t.Store.GetBump(ctx, repository, number)
		if errors.Is(err, store.ErrNotFound) {
			details = append(details, fmt.Sprintf("#%d is not a bump", number))
			continue
		}
		if err != nil {
			return "checks", err
		}
		// Asked only once a bump is known to exist, so a repository full of
		// human pull requests costs no API calls at all.
		if b.HeadSha() != headSha {
			details = append(details, fmt.Sprintf("#%d moved past %s", number, headSha))
			continue
		}
		roll, err := t.Rollup.CheckRollup(ctx, repository, headSha)
		if err != nil {
			return "check rollup", err
		}
		if !roll.Conclusive {
			details = append(details, fmt.Sprintf("#%d rollup still running", number))
			continue
		}
		v := upkeep.CheckVerdict{
			HeadSha:         headSha,
			Conclusion:      roll.Conclusion,
			FailingContexts: strings.Join(roll.FailingContexts, "\n"),
			DetailsURL:      roll.DetailsURL,
			ConcludedAt:     t.Clock.Now(),
		}
		_, err = t.Store.UpdateBump(ctx, repository, number, func(b *upkeep.Bump) error {
			return b.RecordVerdict(v, t.RoundCap)
		})
		// A refusal here is ordinary, not a fault: CI finishing on a head a
		// round is already working, or a force-push winning the race between
		// the head check above and the row lock. Recording either as a
		// rejected delivery would put the normal course of events in the
		// ledger as a failure, which is autophage-2lq.
		if errors.Is(err, resolution.ErrRefused) {
			details = append(details, fmt.Sprintf("#%d refused the verdict: %v", number, err))
			continue
		}
		if err != nil {
			return fmt.Sprintf("#%d verdict %s", number, roll.Conclusion), err
		}
		details = append(details, fmt.Sprintf("VerdictRecorded %s#%d %s", repository, number, roll.Conclusion))
	}
	detail := strings.Join(details, "; ")
	if !strings.Contains(detail, "VerdictRecorded") {
		return detail, errUnsubscribed
	}
	return detail, nil
}

// prNumbers pulls the pull request numbers off a check_suite or workflow_run
// payload. GitHub omits them for a push to a branch with no open pull
// request, which is most of them.
func prNumbers(prs []*gh.PullRequest) []int {
	out := make([]int, 0, len(prs))
	for _, p := range prs {
		if n := p.GetNumber(); n > 0 {
			out = append(out, n)
		}
	}
	return out
}
