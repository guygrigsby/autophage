package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// parseRepo splits owner/repo, which the watch commands take instead of the
// owner/repo#N a bump or a case is named by.
func parseRepo(s string) (string, error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("want owner/repo, got %q", s)
	}
	return s, nil
}

func newBumpsCmd() *cobra.Command {
	var state, repo string
	var limit int
	c := &cobra.Command{
		Use:   "bumps",
		Short: "List dependabot bumps, newest first",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := apiClient()
			if err != nil {
				return err
			}
			q := url.Values{}
			if state != "" {
				q.Set("state", state)
			}
			if repo != "" {
				q.Set("repository", repo)
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			var out struct {
				Bumps []struct {
					Repository       string `json:"repository"`
					Number           int    `json:"number"`
					Branch           string `json:"branch"`
					State            string `json:"state"`
					Rounds           int    `json:"rounds"`
					LatestConclusion string `json:"latest_conclusion"`
				} `json:"bumps"`
				NextCursor string `json:"next_cursor"`
			}
			if err := cl.GetJSON(cmd.Context(), "/api/bumps?"+q.Encode(), &out); err != nil {
				return err
			}
			for _, b := range out.Bumps {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-24s %-18s %-9s rounds %d  %s\n",
					fmt.Sprintf("%s#%d", b.Repository, b.Number), b.State, b.LatestConclusion, b.Rounds, b.Branch)
			}
			if out.NextCursor != "" {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "(more; raise --limit)")
			}
			return nil
		},
	}
	c.Flags().StringVar(&state, "state", "", "filter by bump state")
	c.Flags().StringVar(&repo, "repository", "", "filter by owner/repo")
	c.Flags().IntVar(&limit, "limit", 0, "page size (default 50, max 500)")
	return c
}

func newBumpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bump owner/repo#N",
		Short: "Show one bump: its checks, its repair rounds and why it stopped",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, n, err := parseRef(args[0])
			if err != nil {
				return err
			}
			cl, err := apiClient()
			if err != nil {
				return err
			}
			var out struct {
				Repository string `json:"repository"`
				Number     int    `json:"number"`
				Branch     string `json:"branch"`
				BaseBranch string `json:"base_branch"`
				HeadSha    string `json:"head_sha"`
				State      string `json:"state"`
				Verdicts   []struct {
					HeadSha         string `json:"head_sha"`
					Conclusion      string `json:"conclusion"`
					FailingContexts string `json:"failing_contexts"`
				} `json:"verdicts"`
				Rounds []struct {
					RepairID string `json:"repair_id"`
					Round    int    `json:"round"`
					BaseSha  string `json:"base_sha"`
					Outcome  *struct {
						Kind    string `json:"kind"`
						Summary string `json:"summary"`
					} `json:"outcome"`
				} `json:"rounds"`
				Abandonment *struct {
					Reason string `json:"reason"`
					Detail string `json:"detail"`
				} `json:"abandonment"`
				Closure *struct {
					Kind string `json:"kind"`
				} `json:"closure"`
			}
			if err := cl.GetJSON(cmd.Context(), fmt.Sprintf("/api/bumps/%s/%d", repo, n), &out); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(w, "%s#%d  %s\n", out.Repository, out.Number, out.State)
			_, _ = fmt.Fprintf(w, "branch %s onto %s at %s\n", out.Branch, out.BaseBranch, short(out.HeadSha))
			for _, v := range out.Verdicts {
				line := fmt.Sprintf("  checks %s on %s", v.Conclusion, short(v.HeadSha))
				if v.FailingContexts != "" {
					line += ": " + strings.ReplaceAll(v.FailingContexts, "\n", ", ")
				}
				_, _ = fmt.Fprintln(w, line)
			}
			for _, r := range out.Rounds {
				kind := "running"
				if r.Outcome != nil {
					kind = r.Outcome.Kind
				}
				_, _ = fmt.Fprintf(w, "  round %d from %s: %-16s %s\n", r.Round, short(r.BaseSha), kind, r.RepairID)
			}
			if out.Abandonment != nil {
				_, _ = fmt.Fprintf(w, "gave up (%s): %s\n", out.Abandonment.Reason, out.Abandonment.Detail)
			}
			if out.Closure != nil {
				_, _ = fmt.Fprintf(w, "pull request %s\n", out.Closure.Kind)
			}
			return nil
		},
	}
}

// short is the first seven of a sha, which is what a human reads.
func short(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

func newRetryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retry owner/repo#N",
		Short: "Queue an abandoned or green bump for another repair round",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, n, err := parseRef(args[0])
			if err != nil {
				return err
			}
			cl, err := apiClient()
			if err != nil {
				return err
			}
			var out struct {
				Repository string `json:"repository"`
				Number     int    `json:"number"`
				State      string `json:"state"`
			}
			if err := cl.PostJSON(cmd.Context(), fmt.Sprintf("/api/bumps/%s/%d/retry", repo, n), nil, &out); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s#%d: %s\n", out.Repository, out.Number, out.State)
			return nil
		},
	}
}

func newWatchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "watch owner/repo",
		Short: "Act on the repository's dependabot bumps (enrollment alone is not enough)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := parseRepo(args[0])
			if err != nil {
				return err
			}
			cl, err := apiClient()
			if err != nil {
				return err
			}
			var out struct {
				Repository string `json:"repository"`
				WatchedAt  string `json:"watched_at"`
			}
			if err := cl.PostJSON(cmd.Context(), "/api/repositories/"+repo+"/watch", nil, &out); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "watching %s since %s\n", out.Repository, out.WatchedAt)
			return nil
		},
	}
}

func newUnwatchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unwatch owner/repo",
		Short: "Stop taking new bumps from the repository; the open ones are left alone",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := parseRepo(args[0])
			if err != nil {
				return err
			}
			cl, err := apiClient()
			if err != nil {
				return err
			}
			var out struct {
				Repository string `json:"repository"`
			}
			if err := cl.PostJSON(cmd.Context(), "/api/repositories/"+repo+"/unwatch", nil, &out); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "no longer watching %s\n", out.Repository)
			return nil
		},
	}
}

func newStopRepairCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop-repair repair-id",
		Short: "Stop a running repair round",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := apiClient()
			if err != nil {
				return err
			}
			var out struct {
				RepairID string `json:"repair_id"`
				Bump     struct {
					Repository string `json:"repository"`
					Number     int    `json:"number"`
				} `json:"bump"`
				State string `json:"state"`
			}
			if err := cl.PostJSON(cmd.Context(), fmt.Sprintf("/api/repairs/%s/stop", args[0]), nil, &out); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "stopped %s (%s#%d): %s\n", out.RepairID, out.Bump.Repository, out.Bump.Number, out.State)
			return nil
		},
	}
}
