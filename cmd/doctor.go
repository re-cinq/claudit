package cmd

import (
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
	"github.com/re-cinq/shift-log/internal/config"
	"github.com/re-cinq/shift-log/internal/git"
	"github.com/spf13/cobra"
)

var doctorCmd = &cobra.Command{
	Use:     "doctor",
	Short:   "Diagnose shiftlog configuration issues",
	GroupID: "human",
	Long: `Checks the shiftlog configuration and reports any issues that might
prevent conversations from being stored.

This command checks:
- Git repository status
- Coding agent hook configuration
- Git notes.rewriteRef config (for rebase support)
- Git hooks installation
- PATH configuration`,
	RunE: runDoctor,
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

func runDoctor(cmd *cobra.Command, args []string) error {
	fmt.Println("Shiftlog Doctor")
	fmt.Println("==============")
	fmt.Println()

	hasErrors := false

	// Check 1: Git repository
	fmt.Print("Checking git repository... ")
	if !git.IsInsideWorkTree() {
		fmt.Println("FAIL")
		fmt.Println("  Not inside a git repository")
		hasErrors = true
	} else {
		repoRoot, _ := git.GetRepoRoot()
		fmt.Println("OK")
		fmt.Printf("  Repository: %s\n", repoRoot)
	}
	fmt.Println()

	// Check 2: shiftlog in PATH
	fmt.Print("Checking shiftlog in PATH... ")
	shiftlogPath, err := exec.LookPath("shiftlog")
	if err != nil {
		fmt.Println("FAIL")
		fmt.Println("  'shiftlog' is not in your PATH")
		fmt.Println("  Coding agent hooks will not be able to find shiftlog")
		fmt.Println("  Install with: go install github.com/re-cinq/shift-log@latest")
		hasErrors = true
	} else {
		fmt.Println("OK")
		fmt.Printf("  Found: %s\n", shiftlogPath)
	}
	fmt.Println()

	// Resolve configured agent
	agentName := "claude"
	cfg, err := config.Read()
	if err == nil && cfg.Agent != "" {
		agentName = cfg.Agent
	}
	ag, agentErr := agent.Get(agent.Name(agentName))

	// Check 3: Agent-specific hook configuration
	repoRoot, _ := git.GetRepoRoot()
	if repoRoot == "" {
		fmt.Print("Checking coding agent hook configuration... SKIP (not in git repo)\n")
	} else if agentErr != nil {
		fmt.Printf("Checking coding agent hook configuration... FAIL\n")
		fmt.Printf("  Unknown agent %q configured\n", agentName)
		hasErrors = true
	} else {
		fmt.Printf("Checking %s hook configuration... ", ag.DisplayName())
		checks := ag.DiagnoseHooks(repoRoot)
		if len(checks) == 0 {
			fmt.Println("OK")
		} else {
			allOK := true
			for _, check := range checks {
				if !check.OK {
					allOK = false
					break
				}
			}
			if allOK {
				fmt.Println("OK")
			} else {
				fmt.Println("FAIL")
				hasErrors = true
			}
			for _, check := range checks {
				if check.OK {
					fmt.Printf("  %s\n", check.Message)
				} else {
					fmt.Printf("  %s: %s\n", check.Name, check.Message)
				}
			}
		}
	}
	fmt.Println()

	// Check 4: notes.rewriteRef config
	fmt.Print("Checking notes.rewriteRef config... ")
	if repoRoot == "" {
		fmt.Println("SKIP (not in git repo)")
	} else {
		rewriteRefCmd := exec.Command("git", "config", "notes.rewriteRef")
		rewriteRefCmd.Dir = repoRoot
		rewriteOut, err := rewriteRefCmd.Output()
		if err != nil || strings.TrimSpace(string(rewriteOut)) != git.NotesRef {
			fmt.Println("FAIL")
			fmt.Printf("  notes.rewriteRef is not set to %s\n", git.NotesRef)
			fmt.Println("  Notes will not follow commits during rebase")
			fmt.Println("  Run 'shiftlog init' to fix")
			hasErrors = true
		} else {
			fmt.Println("OK")
			fmt.Println("  Notes will follow commits during rebase")
		}
	}
	fmt.Println()

	// Check 5: Git hooks
	fmt.Print("Checking git hooks... ")
	if repoRoot == "" {
		fmt.Println("SKIP (not in git repo)")
	} else {
		gitDir, _ := git.EnsureGitDir()
		if gitDir == "" {
			fmt.Println("FAIL")
			fmt.Println("  Could not find .git directory")
			hasErrors = true
		} else {
			missingHooks := []string{}
			for _, hook := range []string{"pre-push", "post-merge", "post-checkout", "post-commit"} {
				hookPath := filepath.Join(gitDir, "hooks", hook)
				data, err := os.ReadFile(hookPath)
				if err != nil {
					missingHooks = append(missingHooks, hook)
				} else if !strings.Contains(string(data), "shiftlog") {
					missingHooks = append(missingHooks, hook+" (no shiftlog)")
				}
			}
			if len(missingHooks) > 0 {
				fmt.Println("FAIL")
				fmt.Printf("  Missing or incomplete hooks: %v\n", missingHooks)
				fmt.Println("  Run 'shiftlog init' to fix")
				hasErrors = true
			} else {
				fmt.Println("OK")
				fmt.Println("  All git hooks installed")
			}
		}
	}
	fmt.Println()

	// Summary
	if hasErrors {
		fmt.Println("Issues found. Run 'shiftlog init' to fix configuration.")
		return fmt.Errorf("configuration issues detected")
	}

	fmt.Println("All checks passed! Shiftlog is properly configured.")
	return nil
}
