package cmd

import (
	"time"

	"github.com/re-cinq/shift-log/internal/cli"
	"github.com/re-cinq/shift-log/internal/session"
	"github.com/spf13/cobra"
)

// SessionStartInput represents the SessionStart hook JSON input from the coding agent
type SessionStartInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
}

var sessionStartCmd = &cobra.Command{
	Use:     "session-start",
	Short:   "Handle coding agent SessionStart hook",
	GroupID: "hooks",
	Long: `Reads SessionStart hook JSON from stdin and records the active session.

This command is designed to be called by the coding agent's SessionStart hook.`,
	RunE: runSessionStart,
}

func init() {
	rootCmd.AddCommand(sessionStartCmd)
}

func runSessionStart(cmd *cobra.Command, args []string) error {
	cli.LogDebug("session-start: reading hook input")
	var hook SessionStartInput
	if err := cli.ReadHookInput(&hook); err != nil {
		cli.LogDebug("session-start: failed to read hook input: %v", err)
		return nil // Exit silently to not disrupt workflow
	}

	cli.LogDebug("session-start: session=%s cwd=%s transcript=%s", hook.SessionID, hook.Cwd, hook.TranscriptPath)

	// Validate required fields
	if hook.SessionID == "" || hook.TranscriptPath == "" {
		cli.LogWarning("missing required fields in hook data")
		return nil
	}

	// Create active session record
	activeSession := &session.ActiveSession{
		SessionID:      hook.SessionID,
		TranscriptPath: hook.TranscriptPath,
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
		ProjectPath:    hook.Cwd,
	}

	// Write active session file
	if err := session.WriteActiveSession(activeSession); err != nil {
		// Log but don't fail - don't disrupt user's workflow
		cli.LogWarning("failed to write active session: %v", err)
		return nil
	}

	cli.LogInfo("session started: %s", hook.SessionID[:8])
	return nil
}
