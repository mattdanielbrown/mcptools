package commands

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// ResourcesCmd creates the resources command.
func ResourcesCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "resources [command args...]",
		Short:              "List available resources on the MCP server",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		Run: func(thisCmd *cobra.Command, args []string) {
			if len(args) == 1 && (args[0] == FlagHelp || args[0] == FlagHelpShort) {
				_ = thisCmd.Help()
				return
			}

			parsedArgs := ProcessFlags(args)

			mcpClient, err := CreateClientFunc(parsedArgs)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				fmt.Fprintf(os.Stderr, "Example: mcp resources npx -y @modelcontextprotocol/server-filesystem ~\n")
				os.Exit(1)
			}

			resp, listErr := mcpClient.ListResources()
			if formatErr := FormatAndPrintResponse(thisCmd, resp, listErr); formatErr != nil {
				fmt.Fprintf(os.Stderr, "%v\n", formatErr)
				os.Exit(1)
			}
		},
	}
}
