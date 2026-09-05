// The rookery CLI client; it talks to a running autophaged.
package main

import (
	"fmt"
	"os"

	"github.com/guygrigsby/perch/client"
	"github.com/spf13/cobra"
)

// appID is the program id perch uses for env-var and config-path derivation.
const appID = "autophage"

var cliFlags *client.Flags

func newRootCmd() *cobra.Command {
	root, f := client.Root(appID, "autophage CLI", "Talks to a running daemon.", "http://127.0.0.1:8080")
	cliFlags = f
	root.AddCommand(newAuthCmd(), newWhoamiCmd(), newStatusCmd(), newCasesCmd(), newCaseCmd(), newRunCmd(), newStopCmd(), newWhyCmd())
	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
