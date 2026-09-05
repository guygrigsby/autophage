package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v88/github"

	"github.com/guygrigsby/autophage/internal/resolution"
)

// ClientConfig configures the App client. BaseURL is for tests; empty means
// api.github.com. Installations resolves a repository to its installation id
// (the daemon wires the store).
type ClientConfig struct {
	AppID         int64
	PrivateKeyPEM []byte
	BaseURL       string
	UserAgent     string
	Installations func(ctx context.Context, repository string) (int64, error)
}

// Client implements resolution.GitHub over the GitHub App. One installation
// transport per installation id and repository name, built lazily and cached.
type Client struct {
	cfg  ClientConfig
	apps *ghinstallation.AppsTransport
	mu   sync.Mutex
	inst map[string]*ghinstallation.Transport
}

var _ resolution.GitHub = (*Client)(nil)

func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.AppID <= 0 || len(cfg.PrivateKeyPEM) == 0 || cfg.Installations == nil {
		return nil, errors.New("github: app id, private key and installation resolver are required")
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "autophage"
	}
	// base carries the User-Agent and the retry policy for every request
	// ghinstallation sends on the wire: the token mint (both MintToken's
	// direct call and any lazy refresh inside a repo call, since
	// NewFromAppsTransport reuses this same base as the installation
	// transport's underlying sender) and the repo API call itself.
	// ghinstallation turns a non-2xx token response into a Go error only
	// after the transport returns it, so the retry must happen in here,
	// before that conversion.
	base := &retryTransport{next: http.DefaultTransport, userAgent: cfg.UserAgent}
	apps, err := ghinstallation.NewAppsTransport(base, cfg.AppID, cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("github: app transport: %w", err)
	}
	if cfg.BaseURL != "" {
		apps.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	}
	return &Client{cfg: cfg, apps: apps, inst: map[string]*ghinstallation.Transport{}}, nil
}

// transport returns the cached installation transport for (id, repoName).
// The token is scoped to that one repository name; ghinstallation caches
// the token inside the transport and renews it on expiry, so one transport
// per repository means one mint per hour, not per call.
func (c *Client) transport(id int64, repoName string) *ghinstallation.Transport {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := fmt.Sprintf("%d/%s", id, repoName)
	if tr, ok := c.inst[key]; ok {
		return tr
	}
	tr := ghinstallation.NewFromAppsTransport(c.apps, id)
	tr.InstallationTokenOptions = &gh.InstallationTokenOptions{Repositories: []string{repoName}}
	if c.cfg.BaseURL != "" {
		tr.BaseURL = strings.TrimRight(c.cfg.BaseURL, "/")
	}
	c.inst[key] = tr
	return tr
}

// api builds a go-github client for the repository's installation with the
// token scoped to that repository. go-github v88 configures everything
// through functional options; WithURLs only validates the URL shape (unlike
// WithEnterpriseURLs it does not append an api/v3 prefix), which is what the
// httptest fake needs.
//
// The installation transport's underlying sender is the same retrying,
// User-Agent-setting base built in NewClient (ghinstallation.
// NewFromAppsTransport reuses it), so the actual repo API call already gets
// the retry policy without wrapping it again here; wrapping it a second
// time would let a sustained rate limit retry the same call from two nested
// layers at once.
func (c *Client) api(ctx context.Context, repository string) (*gh.Client, error) {
	id, err := c.cfg.Installations(ctx, repository)
	if err != nil {
		return nil, err
	}
	_, name := splitRepo(repository)
	opts := []gh.ClientOptionsFunc{
		gh.WithTransport(c.transport(id, name)),
		gh.WithUserAgent(c.cfg.UserAgent),
	}
	if c.cfg.BaseURL != "" {
		base := strings.TrimRight(c.cfg.BaseURL, "/") + "/"
		opts = append(opts, gh.WithURLs(gh.Ptr(base), gh.Ptr(base)))
	}
	client, err := gh.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("github: build client: %w", err)
	}
	return client, nil
}

func splitRepo(full string) (owner, name string) {
	owner, name, _ = strings.Cut(full, "/")
	return owner, name
}

func (c *Client) MintToken(ctx context.Context, repo resolution.Repository) (resolution.Token, error) {
	_, name := repo.OwnerName()
	tr := c.transport(repo.InstallationID, name)
	tok, err := tr.Token(ctx)
	if err != nil {
		return resolution.Token{}, fmt.Errorf("github: mint token for %s: %w", repo.FullName, err)
	}
	exp, _, err := tr.Expiry()
	if err != nil {
		exp = time.Now().Add(55 * time.Minute)
	}
	return resolution.Token{Value: tok, ExpiresAt: exp}, nil
}

// DefaultBranch reads the repository's default branch; the runner refreshes
// the store with it before an attempt.
func (c *Client) DefaultBranch(ctx context.Context, repo resolution.Repository) (string, error) {
	api, err := c.api(ctx, repo.FullName)
	if err != nil {
		return "", err
	}
	owner, name := repo.OwnerName()
	r, _, err := api.Repositories.Get(ctx, owner, name)
	if err != nil {
		return "", fmt.Errorf("github: get %s: %w", repo.FullName, err)
	}
	return r.GetDefaultBranch(), nil
}

func (c *Client) GetIssue(ctx context.Context, repository string, number int) (resolution.IssueDetail, error) {
	api, err := c.api(ctx, repository)
	if err != nil {
		return resolution.IssueDetail{}, err
	}
	owner, name := splitRepo(repository)
	is, _, err := api.Issues.Get(ctx, owner, name, number)
	if err != nil {
		var ghErr *gh.ErrorResponse
		if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound {
			return resolution.IssueDetail{}, fmt.Errorf("github: get issue %s#%d: %w", repository, number, resolution.ErrIssueNotFound)
		}
		return resolution.IssueDetail{}, fmt.Errorf("github: get issue %s#%d: %w", repository, number, err)
	}
	assoc, err := resolution.ParseAssociation(is.GetAuthorAssociation())
	if err != nil {
		return resolution.IssueDetail{}, err
	}
	req, err := resolution.NewRequester(is.GetUser().GetLogin(), assoc)
	if err != nil {
		return resolution.IssueDetail{}, err
	}
	return resolution.IssueDetail{Title: is.GetTitle(), Body: is.GetBody(), Requester: req, Open: is.GetState() == "open"}, nil
}

func (c *Client) PostComment(ctx context.Context, repository string, number int, body string) (int64, error) {
	api, err := c.api(ctx, repository)
	if err != nil {
		return 0, err
	}
	owner, name := splitRepo(repository)
	cm, _, err := api.Issues.CreateComment(ctx, owner, name, number, &gh.IssueComment{Body: gh.Ptr(neutraliseMentions(body))})
	if err != nil {
		return 0, fmt.Errorf("github: comment on %s#%d: %w", repository, number, err)
	}
	return cm.GetID(), nil
}

func (c *Client) OpenPullRequest(ctx context.Context, repository, head, base, title, body string) (int, error) {
	api, err := c.api(ctx, repository)
	if err != nil {
		return 0, err
	}
	owner, name := splitRepo(repository)
	// The title as well as the body: it is built from the issue's own title,
	// which is whatever the requester typed, and a pull request title pings
	// exactly like a body does.
	pr, _, err := api.PullRequests.Create(ctx, owner, name, &gh.NewPullRequest{Title: gh.Ptr(neutraliseMentions(title)), Head: gh.Ptr(head), Base: gh.Ptr(base), Body: gh.Ptr(neutraliseMentions(body))})
	if err != nil {
		return 0, fmt.Errorf("github: open pull request on %s: %w", repository, err)
	}
	return pr.GetNumber(), nil
}

// EnsureLabel creates the label when it is missing. Idempotent.
func (c *Client) EnsureLabel(ctx context.Context, repository, label string) error {
	api, err := c.api(ctx, repository)
	if err != nil {
		return err
	}
	owner, name := splitRepo(repository)
	_, resp, err := api.Issues.GetLabel(ctx, owner, name, label)
	if err == nil {
		return nil
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("github: get label %s on %s: %w", label, repository, err)
	}
	_, _, err = api.Issues.CreateLabel(ctx, owner, name, &gh.Label{Name: gh.Ptr(label), Color: gh.Ptr("0e8a16"), Description: gh.Ptr("autophage may act on this issue")})
	if err != nil {
		return fmt.Errorf("github: create label %s on %s: %w", label, repository, err)
	}
	return nil
}

// neutraliseMentions puts a zero-width space after every @ so a posted body
// cannot ping people.
func neutraliseMentions(s string) string { return strings.ReplaceAll(s, "@", "@\u200b") }

// baseBackoff is the first wait when a throttled response names no time of
// its own. Attempt n waits baseBackoff << n, so 1s then 2s.
const baseBackoff = time.Second

// retryTransport sets a default User-Agent and retries a 403 or 429 up to
// twice, sleeping at most 60s each time, then hands the response back. The
// wait comes from Retry-After, else x-ratelimit-reset, else an exponential
// backoff: GitHub's secondary rate limits answer with neither header, and
// giving up on those turned a throttle into a failed attempt. One instance
// serves both the App's own requests (the token mint, which never gets a
// User-Agent or retry from go-github since it never sees that request) and,
// transitively through ghinstallation, the repo API calls, so the policy
// lives in exactly one place.
type retryTransport struct {
	next      http.RoundTripper
	userAgent string
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.userAgent != "" && req.Header.Get("User-Agent") == "" {
		// Clone rather than mutate req in place: RoundTrip must not modify
		// the request it is given, only consume and close its body.
		req = cloneRequest(req)
		req.Header.Set("User-Agent", t.userAgent)
	}
	for attempt := range 3 {
		// The first attempt drains req.Body over the wire; a retry must
		// rewind it from GetBody (net/http populates this for buffer- and
		// reader-backed bodies) or the resend goes out with an empty body
		// while Content-Length still claims the original size.
		if attempt > 0 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = body
		}
		resp, err := t.next.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		if (resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests) || attempt == 2 {
			return resp, nil
		}
		wait := retryAfter(resp.Header.Get("Retry-After"))
		if wait <= 0 {
			wait = rateLimitReset(resp.Header.Get("X-RateLimit-Reset"), time.Now())
		}
		if wait <= 0 {
			wait = baseBackoff << attempt
		}
		_ = resp.Body.Close()
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(wait):
		}
	}
	return nil, errors.New("unreachable")
}

// cloneRequest shallow-copies a request and deep-copies its header, so a
// transport can set a header without mutating the caller's request.
func cloneRequest(r *http.Request) *http.Request {
	r2 := new(http.Request)
	*r2 = *r
	r2.Header = r.Header.Clone()
	return r2
}

// rateLimitReset reads x-ratelimit-reset, epoch seconds, as a wait from
// now, capped at 60s so a reset an hour out does not park the request for
// an hour. A missing, unparseable or already-past reset is 0, which leaves
// the caller on its backoff.
func rateLimitReset(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	epoch, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return min(max(time.Unix(epoch, 0).Sub(now), 0), 60*time.Second)
}

// retryAfter parses the seconds or HTTP-date forms, capped at 60s.
func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return min(time.Duration(secs)*time.Second, 60*time.Second)
	}
	if at, err := http.ParseTime(v); err == nil {
		return min(max(time.Until(at), 0), 60*time.Second)
	}
	return 0
}
