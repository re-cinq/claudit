package cmd

import (
	"fmt"

	"github.com/re-cinq/shift-log/internal/git"
	"github.com/re-cinq/shift-log/internal/web"
	"github.com/spf13/cobra"
)

var (
	servePort       int
	serveNoBrowser  bool
)

var serveCmd = &cobra.Command{
	Use:     "serve",
	Short:   "Start the web visualization server",
	GroupID: "human",
	Long: `Starts a local web server for browsing commits and viewing conversation history.

The web interface provides:
  - A commit list showing which commits have conversations
  - A conversation viewer for reading message history
  - The ability to resume sessions directly from the UI

The server binds to localhost (127.0.0.1) for security.

Examples:
  shiftlog serve                 # Start on default port 8080, open browser
  shiftlog serve --port 3000     # Start on custom port
  shiftlog serve --no-browser    # Start without opening browser`,
	RunE: runServe,
}

func init() {
	rootCmd.AddCommand(serveCmd)
	serveCmd.Flags().IntVarP(&servePort, "port", "p", 8080, "Port to listen on")
	serveCmd.Flags().BoolVar(&serveNoBrowser, "no-browser", false, "Don't open browser automatically")
}

func runServe(cmd *cobra.Command, args []string) error {
	// Verify we're in a git repository
	if err := git.RequireGitRepo(); err != nil {
		return err
	}

	repoDir, err := git.GetRepoRoot()
	if err != nil {
		return fmt.Errorf("could not determine repository root: %w", err)
	}

	server := web.NewServer(servePort, repoDir)
	return server.Start(!serveNoBrowser)
}
