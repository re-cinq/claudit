package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/re-cinq/shift-log/internal/agent"
	_ "github.com/re-cinq/shift-log/internal/agent/claude"   // register Claude agent
	_ "github.com/re-cinq/shift-log/internal/agent/codex"    // register Codex agent
	_ "github.com/re-cinq/shift-log/internal/agent/copilot"  // register Copilot agent
	_ "github.com/re-cinq/shift-log/internal/agent/gemini"   // register Gemini agent
	_ "github.com/re-cinq/shift-log/internal/agent/opencode" // register OpenCode agent
	"github.com/re-cinq/shift-log/internal/cli"
	"github.com/re-cinq/shift-log/internal/config"
	"github.com/re-cinq/shift-log/internal/git"
	"github.com/re-cinq/shift-log/internal/storage"
	"github.com/spf13/cobra"
)

var (
	manualFlag     bool
	storeAgentFlag string
)

var storeCmd = &cobra.Command{
	Use:     "store",
	Short:   "Store conversation from coding agent hook",
	GroupID: "hooks",
	Long: `Reads hook JSON from stdin and stores the conversation
as a Git Note if a git commit was detected.

This command is designed to be called by a coding agent's hook system.

With --manual flag, discovers the active session and stores its conversation
for the most recent commit. Used by the post-commit git hook.`,
	RunE: runStore,
}

func init() {
	storeCmd.Flags().BoolVar(&manualFlag, "manual", false, "Manual mode: discover session from active session file or recent sessions")
	storeCmd.Flags().StringVar(&storeAgentFlag, "agent", "", "Coding agent (claude, codex, copilot, gemini, opencode). Defaults to configured agent.")
	rootCmd.AddCommand(storeCmd)
}

// resolveAgent resolves the agent from the --agent flag or config.
func resolveAgent(flagValue string) (agent.Agent, error) {
	name := flagValue
	if name == "" {
		cfg, err := config.Read()
		if err == nil && cfg.Agent != "" {
			name = cfg.Agent
		} else {
			name = "claude"
		}
	}
	return agent.Get(agent.Name(name))
}

func runStore(cmd *cobra.Command, args []string) error {
	if manualFlag {
		return runManualStore()
	}
	return runHookStore()
}

// runHookStore handles the hook mode.
func runHookStore() error {
	cli.LogDebug("store: reading hook input from stdin")

	ag, err := resolveAgent(storeAgentFlag)
	if err != nil {
		cli.LogDebug("store: unknown agent: %v", err)
		return nil
	}

	// Read raw stdin
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		cli.LogDebug("store: failed to read stdin: %v", err)
		return nil
	}

	hookData, err := ag.ParseHookInput(raw)
	if err != nil {
		cli.LogWarning("failed to parse hook JSON: %v", err)
		return nil
	}

	cli.LogDebug("store: tool=%s command=%q session=%s", hookData.ToolName, hookData.Command, hookData.SessionID)

	// Check if this is a git commit command
	if !ag.IsCommitCommand(hookData.ToolName, hookData.Command) {
		cli.LogDebug("store: not a git commit command, skipping")
		return nil
	}

	// Verify we're in a git repository
	if !git.IsInsideWorkTree() {
		cli.LogWarning("not inside a git repository")
		return nil
	}

	return storeConversation(ag, hookData.SessionID, hookData.TranscriptPath, hookData.TranscriptData)
}

// runManualStore handles the manual (post-commit hook) mode.
func runManualStore() error {
	cli.LogDebug("store: manual mode")

	if !git.IsInsideWorkTree() {
		cli.LogDebug("store: not inside a git repository, skipping")
		return nil
	}

	projectPath, err := git.GetRepoRoot()
	if err != nil {
		cli.LogDebug("store: failed to get repo root: %v", err)
		return nil
	}

	cli.LogDebug("store: discovering active session in %s", projectPath)

	ag, err := resolveAgent(storeAgentFlag)
	if err != nil {
		cli.LogDebug("store: unknown agent: %v", err)
		return nil
	}

	// Use the agent's own session discovery (each agent knows where its sessions live)
	agentSession, err := ag.DiscoverSession(projectPath)
	if err != nil {
		cli.LogDebug("store: session discovery error: %v", err)
		return nil
	}

	if agentSession == nil {
		cli.LogDebug("store: no active session found")
		return nil
	}

	cli.LogDebug("store: found session %s", agentSession.SessionID)
	return storeConversation(ag, agentSession.SessionID, agentSession.TranscriptPath, agentSession.TranscriptData)
}

// storeConversation stores a conversation for the HEAD commit with duplicate detection.
// When transcriptData is non-empty, it is used directly instead of reading from transcriptPath.
func storeConversation(ag agent.Agent, sessionID, transcriptPath string, transcriptData []byte) error {
	headCommit, err := git.GetHeadCommit()
	if err != nil {
		return fmt.Errorf("failed to get HEAD commit: %w", err)
	}

	cli.LogDebug("store: HEAD commit is %s", headCommit[:8])

	// Check for existing note (duplicate detection)
	if git.HasNote(headCommit) {
		cli.LogDebug("store: existing note found for %s, checking for duplicate", headCommit[:8])
		existingNote, err := git.GetNote(headCommit)
		if err == nil {
			existing, err := storage.UnmarshalStoredConversation(existingNote)
			if err == nil && existing.SessionID == sessionID {
				cli.LogInfo("conversation already stored for commit %s", headCommit[:8])
				return nil
			}
			cli.LogDebug("store: different session, will overwrite existing note")
		}
	}

	// Use inline transcript data if provided, otherwise read from path
	if len(transcriptData) == 0 {
		if transcriptPath == "" {
			return fmt.Errorf("no transcript path or inline data provided")
		}

		cli.LogDebug("store: reading transcript from %s", transcriptPath)

		transcriptData, err = readTranscriptData(transcriptPath)
		if err != nil {
			return fmt.Errorf("failed to read transcript: %w", err)
		}
	} else {
		cli.LogDebug("store: using inline transcript data (%d bytes)", len(transcriptData))
	}

	cli.LogDebug("store: transcript size is %d bytes", len(transcriptData))

	transcript, err := ag.ParseTranscript(strings.NewReader(string(transcriptData)))
	if err != nil {
		return fmt.Errorf("failed to parse transcript: %w", err)
	}

	projectPath, _ := git.GetRepoRoot()
	branch, _ := git.GetCurrentBranch()

	cli.LogDebug("store: project=%s branch=%s messages=%d", projectPath, branch, transcript.MessageCount())

	stored, err := storage.NewStoredConversation(
		sessionID,
		projectPath,
		branch,
		transcript.MessageCount(),
		transcriptData,
	)
	if err != nil {
		return fmt.Errorf("failed to create stored conversation: %w", err)
	}

	stored.Agent = string(ag.Name())
	stored.Model = transcript.Model

	// Populate effort metrics from transcript
	stored.Effort = &storage.Effort{
		Turns:                    transcript.Turns,
		InputTokens:              transcript.Usage.InputTokens,
		OutputTokens:             transcript.Usage.OutputTokens,
		CacheCreationInputTokens: transcript.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     transcript.Usage.CacheReadInputTokens,
	}
	if stored.Effort.Turns == 0 && stored.Effort.TotalTokens() == 0 {
		stored.Effort = nil
	}

	noteContent, err := stored.Marshal()
	if err != nil {
		return fmt.Errorf("failed to marshal conversation: %w", err)
	}

	cli.LogDebug("store: note size is %d bytes", len(noteContent))

	if err := git.AddNote(headCommit, noteContent); err != nil {
		return fmt.Errorf("failed to add git note: %w", err)
	}

	cli.LogInfo("stored conversation for commit %s", headCommit[:8])
	return nil
}

// readTranscriptData reads transcript data from a file or directory.
// Some agents (e.g., OpenCode) store messages as individual JSON files
// in a directory rather than a single file. In that case, we read all
// JSON files and combine them into a JSON array.
// JSONL files are split by line so each line becomes a separate message.
func readTranscriptData(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	if !info.IsDir() {
		return os.ReadFile(path)
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	var messages []json.RawMessage
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		isJSON := strings.HasSuffix(name, ".json")
		isJSONL := strings.HasSuffix(name, ".jsonl")
		if !isJSON && !isJSONL {
			continue
		}
		data, err := os.ReadFile(filepath.Join(path, name))
		if err != nil {
			continue
		}
		if isJSONL {
			// JSONL: each line is a separate JSON object
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				messages = append(messages, json.RawMessage(line))
			}
		} else {
			messages = append(messages, json.RawMessage(data))
		}
	}

	return json.Marshal(messages)
}
