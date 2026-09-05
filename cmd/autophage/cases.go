package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// parseRef parses owner/repo#N.
func parseRef(s string) (repo string, number int, err error) {
	repo, n, ok := strings.Cut(s, "#")
	if !ok || !strings.Contains(repo, "/") {
		return "", 0, fmt.Errorf("want owner/repo#N, got %q", s)
	}
	number, err = strconv.Atoi(n)
	if err != nil || number < 1 {
		return "", 0, fmt.Errorf("want owner/repo#N, got %q", s)
	}
	return repo, number, nil
}

func newCasesCmd() *cobra.Command {
	var state, repo string
	var limit int
	c := &cobra.Command{
		Use:   "cases",
		Short: "List cases, newest first",
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
				Cases []struct {
					Repository        string `json:"repository"`
					Number            int    `json:"number"`
					State             string `json:"state"`
					RequesterLogin    string `json:"requester_login"`
					RequesterTrust    string `json:"requester_trust"`
					LatestOutcomeKind string `json:"latest_outcome_kind"`
				} `json:"cases"`
				NextCursor string `json:"next_cursor"`
			}
			if err := cl.GetJSON(cmd.Context(), "/api/cases?"+q.Encode(), &out); err != nil {
				return err
			}
			for _, c := range out.Cases {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-40s %-18s %-12s %s\n", fmt.Sprintf("%s#%d", c.Repository, c.Number), c.State, c.RequesterTrust, c.LatestOutcomeKind)
			}
			if out.NextCursor != "" {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "(more; raise --limit)")
			}
			return nil
		},
	}
	c.Flags().StringVar(&state, "state", "", "filter by state")
	c.Flags().StringVar(&repo, "repository", "", "filter by owner/repo")
	c.Flags().IntVar(&limit, "limit", 50, "page size")
	return c
}

func newCaseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "case owner/repo#N",
		Short: "Show one case with its triage, approvals, attempts and transitions",
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
			var out map[string]any
			if err := cl.GetJSON(cmd.Context(), fmt.Sprintf("/api/cases/%s/%d", repo, n), &out); err != nil {
				return err
			}
			printJSON(cmd, out)
			return nil
		},
	}
}
