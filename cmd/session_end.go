package cmd

import (
	"github.com/re-cinq/shift-log/internal/cli"
	"github.com/re-cinq/shift-log/internal/session"
	"github.com/spf13/cobra"
)

// SessionEndInput represents the SessionEnd hook JSON input from the coding agent
type SessionEndInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	Reason         string `json:"reason"`
}

var sessionEndCmd = &cobra.Command{
	Use:     "session-end",
	Short:   "Handle coding agent SessionEnd hook",
	GroupID: "hooks",
	Long: `Reads SessionEnd hook JSON from stdin and clears the active session.

This command is designed to be called by the coding agent's SessionEnd hook.`,
	RunE: runSessionEnd,
}

func init() {
	rootCmd.AddCommand(sessionEndCmd)
}

func runSessionEnd(cmd *cobra.Command, args []string) error {
	cli.LogDebug("session-end: reading hook input")
	var hook SessionEndInput
	if err := cli.ReadHookInput(&hook); err != nil {
		cli.LogDebug("session-end: failed to read hook input: %v", err)
		return nil // Exit silently to not disrupt workflow
	}

	cli.LogDebug("session-end: session=%s reason=%s", hook.SessionID, hook.Reason)

	// Clear active session file
	if err := session.ClearActiveSession(); err != nil {
		// Log but don't fail - don't disrupt user's workflow
		cli.LogWarning("failed to clear active session: %v", err)
		return nil
	}

	cli.LogInfo("session ended: %s (%s)", hook.SessionID[:8], hook.Reason)
	return nil
}
