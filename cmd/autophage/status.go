package main

import (
	"encoding/json"
	"fmt"

	"github.com/guygrigsby/perch/client"
	"github.com/spf13/cobra"
)

func apiClient(cmd *cobra.Command) (*client.Client, error) {
	tok, err := client.ResolveToken(appID, cliFlags)
	if err != nil {
		return nil, err
	}
	return client.NewClient(cliFlags.Addr, tok), nil
}

func printJSON(cmd *cobra.Command, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(b))
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Daemon status: cases by state, running attempts, queue depth",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := apiClient(cmd)
			if err != nil {
				return err
			}
			var out map[string]any
			if err := c.GetJSON(cmd.Context(), "/api/status", &out); err != nil {
				return err
			}
			printJSON(cmd, out)
			return nil
		},
	}
}
