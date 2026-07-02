package cmd

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/re-cinq/shift-log/internal/agent"
	"github.com/re-cinq/shift-log/internal/cli"
	"github.com/re-cinq/shift-log/internal/config"
	"github.com/re-cinq/shift-log/internal/git"
	"github.com/spf13/cobra"
)

var deinitCmd = &cobra.Command{
	Use:     "deinit",
	Short:   "Remove shiftlog hooks and settings from the current repository",
	GroupID: "human",
	Long: `Removes shiftlog's hooks and git configuration from the current repository.

This command:
- Removes agent-specific hooks/plugins (Claude, Gemini, Copilot, OpenCode)
- Removes shiftlog-managed git hook sections (pre-push, post-merge, post-checkout, post-commit)
- Unsets git config settings for notes visibility

Does NOT remove:
- The .shiftlog/ directory (contains session data; remove manually if desired)
- The .gitignore entry for .shiftlog/
- Git notes data (notes are committed data and preserved)`,
	RunE: runDeinit,
}

func init() {
	rootCmd.AddCommand(deinitCmd)
}

func runDeinit(cmd *cobra.Command, args []string) error {
	if err := git.RequireGitRepo(); err != nil {
		return err
	}

	repoRoot, err := git.GetRepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repository root: %w", err)
	}

	// Read agent from config (default to "claude" if missing)
	agentName := "claude"
	cfg, err := config.Read()
	if err == nil && cfg.Agent != "" {
		agentName = cfg.Agent
	}

	ag, err := agent.Get(agent.Name(agentName))
	if err != nil {
		return fmt.Errorf("unsupported agent %q: %w", agentName, err)
	}

	// Remove agent-specific hooks
	cli.LogDebug("deinit: removing %s hooks", ag.DisplayName())
	if err := ag.RemoveHooks(repoRoot); err != nil {
		return fmt.Errorf("failed to remove %s hooks: %w", ag.DisplayName(), err)
	}
	fmt.Printf("Removed %s hooks\n", ag.DisplayName())

	// Remove git hooks
	cli.LogDebug("deinit: removing git hooks")
	gitDir, err := git.EnsureGitDir()
	if err != nil {
		return fmt.Errorf("failed to find git directory: %w", err)
	}

	if err := git.RemoveAllHooks(gitDir); err != nil {
		return fmt.Errorf("failed to remove git hooks: %w", err)
	}
	fmt.Println("Removed git hooks (pre-push, post-merge, post-checkout, post-commit)")

	// Remove git config settings
	cli.LogDebug("deinit: removing git config settings")
	if err := removeGitSettings(git.NotesRef); err != nil {
		return fmt.Errorf("failed to remove git settings: %w", err)
	}
	fmt.Println("Removed git notes settings (displayRef, rewriteRef)")

	fmt.Println()
	fmt.Println("Shiftlog has been removed from this repository.")
	fmt.Println("Git notes data has been preserved.")

	return nil
}

// removeGitSettings unsets notes.displayRef and notes.rewriteRef if they
// match the shiftlog notes ref. Does not touch settings set to other values.
func removeGitSettings(notesRef string) error {
	for _, key := range []string{"notes.displayRef", "notes.rewriteRef"} {
		out, err := exec.Command("git", "config", key).Output()
		if err != nil {
			// Not set — nothing to remove
			continue
		}
		if strings.TrimSpace(string(out)) == notesRef {
			if err := exec.Command("git", "config", "--unset", key).Run(); err != nil {
				return fmt.Errorf("failed to unset %s: %w", key, err)
			}
		}
	}
	return nil
}
