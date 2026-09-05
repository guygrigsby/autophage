package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newWhyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "why attempt-id",
		Short: "Show the jess ledger chain for an attempt's run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			cl, err := apiClient(cmd)
			if err != nil {
				return err
			}
			var out map[string]any
			if err := cl.GetJSON(cmd.Context(), fmt.Sprintf("/api/attempts/%s/why", id), &out); err != nil {
				return err
			}
			printJSON(cmd, out)
			return nil
		},
	}
}
