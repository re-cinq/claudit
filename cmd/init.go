package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
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
	"github.com/spf13/cobra"
)

var agentFlag string

var initCmd = &cobra.Command{
	Use:     "init",
	Short:   "Initialize shiftlog in the current repository",
	GroupID: "human",
	Long: `Configures the current git repository for conversation capture.

This command:
- Uses refs/notes/shiftlog for note storage
- Configures hooks for the specified coding agent (default: claude)
- Installs git hooks for automatic note syncing
- Configures git settings for notes visibility`,
	RunE: runInit,
}

func init() {
	initCmd.Flags().StringVar(&agentFlag, "agent", "claude", "Coding agent to configure (claude, codex, copilot, gemini, opencode)")
	rootCmd.AddCommand(initCmd)
}

func runInit(cmd *cobra.Command, args []string) error {
	// Verify we're in a git repository
	if err := git.RequireGitRepo(); err != nil {
		return err
	}

	repoRoot, err := git.GetRepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repository root: %w", err)
	}

	// Resolve agent
	ag, err := agent.Get(agent.Name(agentFlag))
	if err != nil {
		return fmt.Errorf("unsupported agent %q (supported: %s)", agentFlag, agent.SupportedNames())
	}

	// Configure git settings for notes visibility
	cli.LogDebug("init: configuring git settings for notes ref %s", git.NotesRef)
	if err := configureGitSettings(git.NotesRef); err != nil {
		return fmt.Errorf("failed to configure git settings: %w", err)
	}

	fmt.Printf("✓ Configured notes ref: %s\n", git.NotesRef)
	fmt.Println("✓ Configured git notes settings (displayRef, rewriteRef)")

	// Configure agent-specific hooks
	cli.LogDebug("init: configuring %s hooks", ag.DisplayName())
	if err := ag.ConfigureHooks(repoRoot); err != nil {
		return fmt.Errorf("failed to configure %s hooks: %w", ag.DisplayName(), err)
	}

	fmt.Printf("✓ Configured %s hooks\n", ag.DisplayName())

	// Install git hooks
	cli.LogDebug("init: installing git hooks")
	gitDir, err := git.EnsureGitDir()
	if err != nil {
		return fmt.Errorf("failed to find git directory: %w", err)
	}

	if err := git.InstallAllHooks(gitDir); err != nil {
		return fmt.Errorf("failed to install git hooks: %w", err)
	}

	fmt.Println("✓ Installed git hooks (pre-push, post-merge, post-checkout, post-commit)")

	// Add .shiftlog/ to .gitignore
	cli.LogDebug("init: ensuring .shiftlog/ is in .gitignore")
	if err := ensureGitignoreEntry(repoRoot, ".shiftlog/"); err != nil {
		return fmt.Errorf("failed to update .gitignore: %w", err)
	}

	fmt.Println("✓ Added .shiftlog/ to .gitignore")

	// Save agent to config
	cfg, err := config.Read()
	if err != nil {
		cfg = &config.Config{}
	}
	cfg.Agent = string(ag.Name())
	if err := config.Write(cfg); err != nil {
		cli.LogDebug("init: failed to write config: %v", err)
	}

	// Check if shiftlog is in PATH
	if _, err := exec.LookPath("shiftlog"); err != nil {
		fmt.Println()
		fmt.Println("⚠ Warning: 'shiftlog' is not in your PATH.")
		fmt.Println("  The hook will not work until shiftlog is installed.")
		fmt.Println("  Install with: go install github.com/re-cinq/shift-log@latest")
	}

	fmt.Println()
	fmt.Println("Shiftlog is now configured! Conversations will be stored")
	fmt.Printf("as git notes on %s when commits are made via %s.\n", git.NotesRef, ag.DisplayName())

	return nil
}

// ensureGitignoreEntry adds an entry to the repo's .gitignore if not already present.
func ensureGitignoreEntry(repoRoot, entry string) error {
	gitignorePath := filepath.Join(repoRoot, ".gitignore")

	// Check if the entry already exists
	if f, err := os.Open(gitignorePath); err == nil {
		defer func() { _ = f.Close() }()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == entry {
				cli.LogDebug("init: .gitignore already contains %s", entry)
				return nil
			}
		}
	}

	// Append the entry
	f, err := os.OpenFile(gitignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// Ensure we start on a new line if the file doesn't end with one
	info, err := f.Stat()
	if err != nil {
		return err
	}
	prefix := ""
	if info.Size() > 0 {
		buf := make([]byte, 1)
		if rf, err := os.Open(gitignorePath); err == nil {
			if _, err := rf.Seek(-1, 2); err == nil {
				if _, err := rf.Read(buf); err == nil {
					if buf[0] != '\n' {
						prefix = "\n"
					}
				}
			}
			_ = rf.Close()
		}
	}

	_, err = fmt.Fprintf(f, "%s%s\n", prefix, entry)
	return err
}

// configureGitSettings configures git settings for notes visibility
func configureGitSettings(notesRef string) error {
	cmd := exec.Command("git", "config", "notes.displayRef", notesRef)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to set notes.displayRef: %w", err)
	}

	cmd = exec.Command("git", "config", "notes.rewriteRef", notesRef)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to set notes.rewriteRef: %w", err)
	}

	return nil
}
