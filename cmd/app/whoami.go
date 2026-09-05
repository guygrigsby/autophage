package main

import (
	"encoding/json"
	"fmt"

	"github.com/guygrigsby/perch/client"
	"github.com/spf13/cobra"
)

func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Call the daemon's authenticated /api/whoami",
		RunE: func(cmd *cobra.Command, _ []string) error {
			tok, err := client.ResolveToken(appID, cliFlags)
			if err != nil {
				return err
			}
			c := client.NewClient(cliFlags.Addr, tok)
			var out map[string]any
			if err := c.GetJSON(cmd.Context(), "/api/whoami", &out); err != nil {
				return err
			}
			b, _ := json.MarshalIndent(out, "", "  ")
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return nil
		},
	}
}
