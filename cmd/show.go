package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/re-cinq/shift-log/internal/agent"
	_ "github.com/re-cinq/shift-log/internal/agent/claude"   // register Claude agent
	_ "github.com/re-cinq/shift-log/internal/agent/codex"    // register Codex agent
	_ "github.com/re-cinq/shift-log/internal/agent/copilot"  // register Copilot agent
	_ "github.com/re-cinq/shift-log/internal/agent/gemini"   // register Gemini agent
	_ "github.com/re-cinq/shift-log/internal/agent/opencode" // register OpenCode agent
	"github.com/re-cinq/shift-log/internal/git"
	"github.com/re-cinq/shift-log/internal/storage"
	"github.com/spf13/cobra"
)

var showFull bool

var showCmd = &cobra.Command{
	Use:     "show [ref]",
	Short:   "Show conversation history for a commit",
	GroupID: "human",
	Long: `Displays the conversation history stored for a commit.

By default, shows only the conversation since the last commit (incremental view).
Use --full to see the complete session history.

If no ref is provided, shows the conversation for HEAD.

Examples:
  shiftlog show           # Show conversation since last commit
  shiftlog show --full    # Show full session history
  shiftlog show abc1234   # Show conversation for specific commit
  shiftlog show HEAD~1    # Show conversation for previous commit`,
	Args: cobra.MaximumNArgs(1),
	RunE: runShow,
}

func init() {
	showCmd.Flags().BoolVarP(&showFull, "full", "f", false, "Show full session history instead of incremental")
	rootCmd.AddCommand(showCmd)
}

func runShow(cmd *cobra.Command, args []string) error {
	// Verify we're in a git repository
	if err := git.RequireGitRepo(); err != nil {
		return err
	}

	// Determine the ref to show
	ref := "HEAD"
	if len(args) > 0 {
		ref = args[0]
	}

	// Resolve the reference to a full SHA
	fullSHA, err := git.ResolveRef(ref)
	if err != nil {
		return fmt.Errorf("could not resolve reference '%s': not a valid commit", ref)
	}

	// Get the stored conversation
	stored, err := storage.GetStoredConversation(fullSHA)
	if err != nil {
		return fmt.Errorf("could not read conversation: %w", err)
	}
	if stored == nil {
		return fmt.Errorf("no conversation found for commit %s", fullSHA[:7])
	}

	// Resolve agent for tool aliases
	agentName := stored.Agent
	if agentName == "" {
		agentName = "claude"
	}
	var toolAliases map[string]string
	if ag, err := agent.Get(agent.Name(agentName)); err == nil {
		toolAliases = ag.ToolAliases()
	}

	// Parse the transcript
	transcript, err := stored.ParseTranscript()
	if err != nil {
		return fmt.Errorf("could not parse transcript: %w", err)
	}

	// Find parent conversation boundary (unless --full is specified)
	var parentSHA string
	var lastEntryUUID string
	var isIncremental bool

	if !showFull {
		parentSHA, lastEntryUUID = storage.FindParentConversationBoundary(fullSHA, stored.SessionID)
		isIncremental = lastEntryUUID != ""
	}

	// Get entries to display
	var entries []agent.TranscriptEntry
	if isIncremental {
		entries = transcript.GetEntriesSince(lastEntryUUID)
	} else {
		entries = transcript.Entries
	}

	// Print header
	message, date, _ := git.GetCommitInfo(fullSHA)
	fmt.Printf("Conversation for %s (%s)\n", fullSHA[:7], date[:10])
	fmt.Printf("Commit: %s\n", message)

	if isIncremental {
		fmt.Printf("Showing: %d entries since %s\n", len(entries), parentSHA[:7])
	} else {
		fmt.Printf("Showing: %d entries (full session)\n", len(entries))
	}

	fmt.Println(strings.Repeat("─", 60))
	fmt.Println()

	// Render the entries
	renderer := agent.NewRenderer(os.Stdout, toolAliases)
	return renderer.RenderEntries(entries)
}
