package github

import (
	"context"
	"errors"
	"fmt"
	"log"

	gh "github.com/google/go-github/v88/github"

	"github.com/guygrigsby/autophage/internal/resolution"
	"github.com/guygrigsby/autophage/internal/store"
)

// Translator turns stored deliveries into Resolution commands and records
// exactly one processing row per delivery.
type Translator struct {
	Store         *store.Store
	Clock         resolution.Clock
	ApprovedLabel string
	BotLogin      string
}

// ProcessPending processes every delivery without a processing row, oldest
// first. One delivery's failure to be recorded is logged and does not stop
// the others.
func (t *Translator) ProcessPending(ctx context.Context) error {
	pending, err := t.Store.UnprocessedDeliveries(ctx)
	if err != nil {
		return err
	}
	for _, d := range pending {
		if err := t.Process(ctx, d); err != nil {
			log.Printf("translate: delivery %s: %v", d.ID, err)
		}
	}
	return nil
}

// Process translates one delivery. Domain refusals become a rejected
// processing row; unknown events and own events become ignored; only a
// store failure is returned, so the delivery is retried on the next scan.
func (t *Translator) Process(ctx context.Context, d store.Delivery) error {
	result, detail := t.translate(ctx, d)
	if result == "" {
		return errors.New(detail)
	}
	return t.Store.RecordProcessing(ctx, d.ID, result, detail)
}

func (t *Translator) translate(ctx context.Context, d store.Delivery) (result, detail string) {
	if d.SenderLogin != "" && d.SenderLogin == t.BotLogin {
		return "ignored", "own event"
	}
	ev, err := gh.ParseWebHook(d.Event, d.Payload)
	if err != nil {
		return "ignored", fmt.Sprintf("unparseable %s: %v", d.Event, err)
	}
	var cmdErr error
	switch e := ev.(type) {
	case *gh.IssuesEvent:
		detail, cmdErr = t.issue(ctx, d, e)
	case *gh.InstallationEvent:
		detail, cmdErr = t.installation(ctx, e)
	case *gh.InstallationRepositoriesEvent:
		detail, cmdErr = t.installationRepositories(ctx, e)
	default:
		return "ignored", fmt.Sprintf("unsubscribed %s.%s", d.Event, d.Action)
	}
	switch {
	case cmdErr == nil:
		return "translated", detail
	case errors.Is(cmdErr, errUnsubscribed), errors.Is(cmdErr, store.ErrNotFound), errors.Is(cmdErr, store.ErrConflict):
		return "ignored", detail
	case errors.Is(cmdErr, resolution.ErrRefused), errors.Is(cmdErr, resolution.ErrInvalid):
		return "rejected", cmdErr.Error()
	default:
		// A store failure: report it so Process leaves no processing row and
		// the delivery is retried on the next scan.
		return "", fmt.Sprintf("%s: %v", detail, cmdErr)
	}
}

func (t *Translator) issue(ctx context.Context, d store.Delivery, e *gh.IssuesEvent) (string, error) {
	repo := e.GetRepo().GetFullName()
	number := e.GetIssue().GetNumber()
	switch e.GetAction() {
	case "opened":
		return t.open(ctx, e)
	case "labeled":
		if e.GetLabel().GetName() != t.ApprovedLabel {
			return fmt.Sprintf("label %q", e.GetLabel().GetName()), errUnsubscribed
		}
		if _, err := t.Store.GetCase(ctx, repo, number); errors.Is(err, store.ErrNotFound) {
			if detail, err := t.open(ctx, e); err != nil {
				return detail, err
			}
		} else if err != nil {
			return "LabelAdded", err
		}
		approver, err := resolution.NewRequester(e.GetSender().GetLogin(), resolution.AssociationCollaborator)
		if err != nil {
			return "LabelAdded", err
		}
		a := resolution.Approval{Approver: approver, Source: resolution.SourceLabel, ApprovedAt: t.Clock.Now(), DeliveryID: d.ID}
		_, err = t.Store.UpdateCase(ctx, repo, number, func(c *resolution.Case) error { return c.RecordApproval(a) })
		return fmt.Sprintf("LabelAdded %s#%d by %s (association recorded as collaborator: GitHub let them label)", repo, number, approver.Login), err
	case "closed":
		cl := resolution.Closure{DeliveryID: d.ID, ClosedAt: t.Clock.Now()}
		_, err := t.Store.UpdateCase(ctx, repo, number, func(c *resolution.Case) error { return c.Close(cl) })
		if errors.Is(err, store.ErrNotFound) {
			return "IssueClosed for unknown case", err
		}
		return fmt.Sprintf("IssueClosed %s#%d", repo, number), err
	default:
		return fmt.Sprintf("issues.%s", e.GetAction()), errUnsubscribed
	}
}

// open creates the case for the event's issue. An unenrolled repository or
// an existing case is ignored, not an error.
func (t *Translator) open(ctx context.Context, e *gh.IssuesEvent) (string, error) {
	repo := e.GetRepo().GetFullName()
	number := e.GetIssue().GetNumber()
	if r, err := t.Store.GetRepository(ctx, repo); errors.Is(err, store.ErrNotFound) || (err == nil && !r.Enrolled()) {
		return "repository not enrolled", store.ErrNotFound
	} else if err != nil {
		return "IssueOpened", err
	}
	assoc, err := resolution.ParseAssociation(e.GetIssue().GetAuthorAssociation())
	if err != nil {
		return "IssueOpened", err
	}
	req, err := resolution.NewRequester(e.GetIssue().GetUser().GetLogin(), assoc)
	if err != nil {
		return "IssueOpened", err
	}
	c, err := resolution.NewCase(repo, number, req, t.Clock.Now())
	if err != nil {
		return "IssueOpened", err
	}
	if err := t.Store.CreateCase(ctx, c); errors.Is(err, store.ErrConflict) {
		return "case exists", err
	} else if err != nil {
		return "IssueOpened", err
	}
	return fmt.Sprintf("IssueOpened %s#%d by %s (%s)", repo, number, req.Login, req.Trust), nil
}

func (t *Translator) installation(ctx context.Context, e *gh.InstallationEvent) (string, error) {
	switch e.GetAction() {
	case "created", "unsuspend", "new_permissions_accepted":
		return t.enroll(ctx, e.GetInstallation().GetID(), e.Repositories)
	case "deleted", "suspend":
		return t.remove(ctx, e.Repositories)
	}
	return fmt.Sprintf("installation.%s", e.GetAction()), errUnsubscribed
}

func (t *Translator) installationRepositories(ctx context.Context, e *gh.InstallationRepositoriesEvent) (string, error) {
	added, err := t.enroll(ctx, e.GetInstallation().GetID(), e.RepositoriesAdded)
	if err != nil {
		return added, err
	}
	removed, err := t.remove(ctx, e.RepositoriesRemoved)
	if err != nil {
		return removed, err
	}
	return added + "; " + removed, nil
}

// enroll records each repository. The installation payloads carry no
// default branch; "main" is recorded and refreshed by the outbound client
// when a token is first minted for the repository.
func (t *Translator) enroll(ctx context.Context, installationID int64, repos []*gh.Repository) (string, error) {
	n := 0
	for _, r := range repos {
		branch := r.GetDefaultBranch()
		if branch == "" {
			branch = "main"
		}
		repo, err := resolution.NewRepository(r.GetFullName(), installationID, branch, t.Clock.Now())
		if err != nil {
			return "RepoEnrolled", err
		}
		if err := t.Store.EnrollRepository(ctx, repo); err != nil {
			return "RepoEnrolled", err
		}
		n++
	}
	return fmt.Sprintf("RepoEnrolled x%d", n), nil
}

func (t *Translator) remove(ctx context.Context, repos []*gh.Repository) (string, error) {
	n := 0
	for _, r := range repos {
		if err := t.Store.RemoveRepository(ctx, r.GetFullName(), t.Clock.Now()); err != nil {
			return "RepoRemoved", err
		}
		n++
	}
	return fmt.Sprintf("RepoRemoved x%d", n), nil
}
