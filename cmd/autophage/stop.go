package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop attempt-id",
		Short: "Stop a running attempt",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			cl, err := apiClient(cmd)
			if err != nil {
				return err
			}
			var out struct {
				AttemptID string `json:"attempt_id"`
				Case      struct {
					Repository string `json:"repository"`
					Number     int    `json:"number"`
				} `json:"case"`
				State string `json:"state"`
			}
			if err := cl.PostJSON(cmd.Context(), fmt.Sprintf("/api/attempts/%s/stop", id), nil, &out); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "stopped %s (%s#%d): %s\n", out.AttemptID, out.Case.Repository, out.Case.Number, out.State)
			return nil
		},
	}
}
