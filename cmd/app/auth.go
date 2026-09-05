package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/guygrigsby/perch/client"
	"github.com/spf13/cobra"
)

func newAuthCmd() *cobra.Command {
	c := &cobra.Command{Use: "auth", Short: "Manage the local auth token"}
	c.AddCommand(newAuthLoginCmd(), newAuthLogoutCmd())
	return c
}

func newAuthLoginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Mint a token from the local daemon and store it",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := client.NewClient(cliFlags.Addr, "")
			var out struct {
				Token string `json:"token"`
			}
			if err := c.PostJSON(cmd.Context(), "/api/auth/mint", nil, &out); err != nil {
				return err
			}
			path, err := client.TokenPath(appID)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(out.Token), 0o600); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "token written to %s\n", path)
			return nil
		},
	}
}

func newAuthLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the stored token",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := client.TokenPath(appID)
			if err != nil {
				return err
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "logged out")
			return nil
		},
	}
}
