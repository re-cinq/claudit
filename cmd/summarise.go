package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/re-cinq/shift-log/internal/agent"
	_ "github.com/re-cinq/shift-log/internal/agent/claude"   // register Claude agent
	_ "github.com/re-cinq/shift-log/internal/agent/codex"    // register Codex agent
	_ "github.com/re-cinq/shift-log/internal/agent/copilot"  // register Copilot agent
	_ "github.com/re-cinq/shift-log/internal/agent/gemini"   // register Gemini agent
	_ "github.com/re-cinq/shift-log/internal/agent/opencode" // register OpenCode agent
	"github.com/re-cinq/shift-log/internal/cli"
	"github.com/re-cinq/shift-log/internal/git"
	"github.com/re-cinq/shift-log/internal/storage"
	"github.com/spf13/cobra"
)

const summariseTimeout = 120 * time.Second

var (
	summariseAgent string
	summariseFocus string
)

var summariseCmd = &cobra.Command{
	Use:     "summarise [ref]",
	Aliases: []string{"tldr"},
	Short:   "Summarise a stored conversation using your coding agent",
	GroupID: "human",
	Long: `Generates an LLM-powered summary of a stored conversation by sending
the transcript to your coding agent in non-interactive mode.

The agent must support non-interactive summarisation (currently Claude Code
and Codex). Use --agent to override which agent performs the summarisation.

If no ref is provided, summarises the conversation for HEAD.

Examples:
  shiftlog summarise            # Summarise conversation for HEAD
  shiftlog tldr                 # Same, using the alias
  shiftlog summarise HEAD~1     # Summarise for previous commit
  shiftlog summarise --agent=claude abc123  # Use Claude Code explicitly
  shiftlog tldr --focus="security changes"  # Prioritise security in summary`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSummarise,
}

func init() {
	summariseCmd.Flags().StringVar(&summariseAgent, "agent", "", "Agent to use for summarisation (e.g. claude, codex)")
	summariseCmd.Flags().StringVarP(&summariseFocus, "focus", "f", "", "What to prioritise in the summary (e.g. \"security changes\", \"API design\")")
	rootCmd.AddCommand(summariseCmd)
}

func runSummarise(cmd *cobra.Command, args []string) error {
	if err := git.RequireGitRepo(); err != nil {
		return err
	}

	// Resolve ref
	ref := "HEAD"
	if len(args) > 0 {
		ref = args[0]
	}

	fullSHA, err := git.ResolveRef(ref)
	if err != nil {
		return fmt.Errorf("could not resolve reference '%s': not a valid commit", ref)
	}

	cli.LogDebug("summarise: resolved %s to %s", ref, fullSHA[:8])

	// Get stored conversation
	stored, err := storage.GetStoredConversation(fullSHA)
	if err != nil {
		return fmt.Errorf("could not read conversation: %w", err)
	}
	if stored == nil {
		return fmt.Errorf("no conversation found for commit %s", fullSHA[:7])
	}

	// Parse transcript
	transcript, err := stored.ParseTranscript()
	if err != nil {
		return fmt.Errorf("could not parse transcript: %w", err)
	}

	if len(transcript.Entries) == 0 {
		return fmt.Errorf("transcript is empty for commit %s", fullSHA[:7])
	}

	// Build the summary prompt
	prompt := agent.BuildSummaryPromptWithFocus(transcript.Entries, agent.DefaultMaxPromptChars, summariseFocus)
	if prompt == "" {
		return fmt.Errorf("transcript has no summarisable content for commit %s", fullSHA[:7])
	}

	cli.LogDebug("summarise: built prompt (%d chars) from %d entries", len(prompt), len(transcript.Entries))

	// Determine which agent to use for summarisation
	agentName := summariseAgent
	if agentName == "" {
		agentName = stored.Agent
		if agentName == "" {
			agentName = "claude"
		}
	}

	ag, err := agent.Get(agent.Name(agentName))
	if err != nil {
		return fmt.Errorf("unknown agent %q (supported: %s)", agentName, agent.SupportedNames())
	}

	// Check if agent supports summarisation
	summariser, ok := ag.(agent.Summariser)
	if !ok {
		return fmt.Errorf("agent %q does not support summarisation; try --agent=claude", agentName)
	}

	binary, cmdArgs := summariser.SummariseCommand()

	// Pass prompt as a positional argument (not stdin) — Claude Code v2.1.49+
	// drains stdin before reading the prompt in -p mode.
	cmdArgs = append(cmdArgs, prompt)

	// Check binary exists
	binaryPath, err := exec.LookPath(binary)
	if err != nil {
		return fmt.Errorf("%s not found in PATH; install it or use --agent to specify a different agent", binary)
	}

	cli.LogDebug("summarise: using %s %s", binaryPath, strings.Join(cmdArgs, " "))

	// Run the agent with timeout
	ctx, cancel := context.WithTimeout(context.Background(), summariseTimeout)
	defer cancel()

	agentCmd := exec.CommandContext(ctx, binaryPath, cmdArgs...)

	var stdout, stderr bytes.Buffer
	agentCmd.Stdout = &stdout
	agentCmd.Stderr = &stderr

	// Start spinner
	spinner := cli.NewSpinner("Summarising conversation...")
	spinner.Start()

	err = agentCmd.Run()
	spinner.Stop()

	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("agent timed out after %s", summariseTimeout)
	}
	if err != nil {
		errMsg := fmt.Sprintf("agent failed: %v", err)
		if stderrOut := strings.TrimSpace(stderr.String()); stderrOut != "" {
			errMsg += "\nstderr: " + stderrOut
		}
		return fmt.Errorf("%s", errMsg)
	}

	// Print summary
	summary := strings.TrimSpace(stdout.String())
	if summary == "" {
		return fmt.Errorf("agent returned empty summary")
	}

	fmt.Println(summary)
	return nil
}
