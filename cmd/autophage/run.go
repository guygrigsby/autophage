package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run owner/repo#N",
		Short: "Manually start an existing issue: create the case if needed and approve it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, n, err := parseRef(args[0])
			if err != nil {
				return err
			}
			cl, err := apiClient(cmd)
			if err != nil {
				return err
			}
			var out struct {
				Repository string `json:"repository"`
				Number     int    `json:"number"`
				State      string `json:"state"`
			}
			if err := cl.PostJSON(cmd.Context(), fmt.Sprintf("/api/cases/%s/%d/run", repo, n), nil, &out); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s#%d: %s\n", out.Repository, out.Number, out.State)
			return nil
		},
	}
}
