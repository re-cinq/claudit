package cmd

import (
	"fmt"

	"github.com/re-cinq/shift-log/internal/git"
	"github.com/re-cinq/shift-log/internal/storage"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:     "list",
	Short:   "List commits with stored conversations",
	GroupID: "human",
	Long: `Lists all commits in the repository that have associated
conversations stored as Git Notes.

Shows:
  - Commit SHA (short)
  - Commit date
  - Commit message (truncated)
  - Number of messages in conversation

Example output:
  abc1234 2024-01-15 feat: add user auth (42 messages)
  def5678 2024-01-14 fix: login bug (15 messages)`,
	RunE: runList,
}

func init() {
	rootCmd.AddCommand(listCmd)
}

func runList(cmd *cobra.Command, args []string) error {
	// Verify we're in a git repository
	if err := git.RequireGitRepo(); err != nil {
		return err
	}

	// Get list of commits with notes
	commits, err := git.ListCommitsWithNotes()
	if err != nil {
		return fmt.Errorf("could not list conversations: %w", err)
	}

	if len(commits) == 0 {
		fmt.Println("no conversations found")
		return nil
	}

	// Display each commit with conversation metadata
	for _, commitSHA := range commits {
		// Get commit info
		message, date, err := git.GetCommitInfo(commitSHA)
		if err != nil {
			// Skip commits we can't get info for
			continue
		}

		// Truncate message
		if len(message) > 50 {
			message = message[:47] + "..."
		}

		// Get conversation metadata
		stored, err := storage.GetStoredConversation(commitSHA)
		if err != nil || stored == nil {
			continue
		}

		// Format date (take just the date part)
		shortDate := date
		if len(date) >= 10 {
			shortDate = date[:10]
		}

		fmt.Printf("%s %s %s (%d messages)\n",
			commitSHA[:7],
			shortDate,
			message,
			stored.MessageCount,
		)
	}

	return nil
}
