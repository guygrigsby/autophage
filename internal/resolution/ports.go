package resolution

import (
	"context"
	"errors"
	"time"
)

// ErrIssueNotFound marks GetIssue finding no such issue on GitHub (a 404),
// as opposed to any other upstream failure. Callers map it to not_found
// rather than upstream_unavailable.
var ErrIssueNotFound = errors.New("issue not found")

// IssueDetail is what the brief needs from GitHub at attempt time.
type IssueDetail struct {
	Title     string
	Body      string
	Requester Requester
	Open      bool
}

// Token is an installation token scoped to one repository.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// GitHub is the outbound port; internal/github implements it.
type GitHub interface {
	// GetIssue wraps ErrIssueNotFound when the issue does not exist on
	// GitHub; every other failure (network, auth, rate limit) is returned
	// unwrapped.
	GetIssue(ctx context.Context, repository string, number int) (IssueDetail, error)
	MintToken(ctx context.Context, repo Repository) (Token, error)
	PostComment(ctx context.Context, repository string, number int, body string) (commentID int64, err error)
	OpenPullRequest(ctx context.Context, repository, head, base, title, body string) (prNumber int, err error)
	EnsureLabel(ctx context.Context, repository, name string) error
}

// Triager sizes a trusted case; internal/agent implements it with one model
// call on the triage tier.
type Triager interface {
	Classify(ctx context.Context, title, body string) (Triage, error)
}

// Runner executes one started attempt end to end and records its run and
// outcome through the store. The dispatcher calls it in a bounded pool.
type Runner interface {
	Run(ctx context.Context, attemptID string)
}

// Clock is time.Now behind an interface so tests pin timestamps.
type Clock interface {
	Now() time.Time
}

// SystemClock is the real clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }
