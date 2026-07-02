package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/re-cinq/shift-log/internal/cli"
	"github.com/re-cinq/shift-log/internal/git"
	"github.com/spf13/cobra"
)

var migrateDryRun bool

var migrateCmd = &cobra.Command{
	Use:     "migrate",
	Short:   "Migrate an existing claudit repository to shiftlog",
	GroupID: "human",
	Long: `Migrates a repository previously set up with claudit to use shiftlog.

This command:
- Renames .claudit/ → .shiftlog/ (preserving all contents)
- Updates .gitignore entries from .claudit/ to .shiftlog/
- Upgrades git hooks from # claudit-managed to # shiftlog-managed markers
- Renames .github/hooks/claudit.json → shiftlog.json (Copilot users)

The command is idempotent — safe to run multiple times.
Use --dry-run to preview changes without applying them.`,
	RunE: runMigrate,
}

func init() {
	migrateCmd.Flags().BoolVar(&migrateDryRun, "dry-run", false, "Preview changes without applying them")
	rootCmd.AddCommand(migrateCmd)
}

func runMigrate(cmd *cobra.Command, args []string) error {
	if err := git.RequireGitRepo(); err != nil {
		return err
	}

	repoRoot, err := git.GetRepoRoot()
	if err != nil {
		return fmt.Errorf("failed to get repository root: %w", err)
	}

	gitDir, err := git.EnsureGitDir()
	if err != nil {
		return fmt.Errorf("failed to find git directory: %w", err)
	}

	if migrateDryRun {
		fmt.Println("Dry run — no changes will be made.")
	}

	changed := 0

	// 1. Rename .claudit/ → .shiftlog/
	n, err := migrateConfigDir(repoRoot)
	if err != nil {
		return err
	}
	changed += n

	// 2. Update .gitignore
	n, err = migrateGitignore(repoRoot)
	if err != nil {
		return err
	}
	changed += n

	// 3. Upgrade git hooks (claudit-managed → shiftlog-managed)
	n, err = migrateGitHooks(gitDir)
	if err != nil {
		return err
	}
	changed += n

	// 4. Rename Copilot hooks file
	n, err = migrateCopilotHooks(repoRoot)
	if err != nil {
		return err
	}
	changed += n

	// 5. Migrate notes ref: refs/notes/claude-conversations → refs/notes/shiftlog
	n, err = migrateNotesRef()
	if err != nil {
		return err
	}
	changed += n

	fmt.Println()
	if changed == 0 {
		fmt.Println("Nothing to migrate — repository is already up to date.")
	} else if migrateDryRun {
		fmt.Printf("%d change(s) would be applied. Run without --dry-run to apply.\n", changed)
	} else {
		fmt.Printf("Migration complete. %d change(s) applied.\n", changed)
	}
	return nil
}

// migrateConfigDir renames .claudit/ → .shiftlog/ if it exists and .shiftlog/ does not.
func migrateConfigDir(repoRoot string) (int, error) {
	oldDir := filepath.Join(repoRoot, ".claudit")
	newDir := filepath.Join(repoRoot, ".shiftlog")

	oldExists := dirExists(oldDir)
	newExists := dirExists(newDir)

	switch {
	case !oldExists && !newExists:
		cli.LogDebug("migrate: no .claudit/ or .shiftlog/ found")
		return 0, nil

	case !oldExists && newExists:
		fmt.Println("✓ .shiftlog/ already exists (skipping rename)")
		return 0, nil

	case oldExists && newExists:
		fmt.Println("⚠ Both .claudit/ and .shiftlog/ exist — skipping rename (remove .claudit/ manually after verifying .shiftlog/)")
		return 0, nil

	default: // oldExists && !newExists
		fmt.Printf("  .claudit/ → .shiftlog/\n")
		if !migrateDryRun {
			if err := os.Rename(oldDir, newDir); err != nil {
				return 0, fmt.Errorf("failed to rename .claudit/ to .shiftlog/: %w", err)
			}
		}
		return 1, nil
	}
}

// migrateGitignore replaces .claudit/ entries with .shiftlog/ in .gitignore.
func migrateGitignore(repoRoot string) (int, error) {
	gitignorePath := filepath.Join(repoRoot, ".gitignore")

	data, err := os.ReadFile(gitignorePath)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to read .gitignore: %w", err)
	}

	oldEntry := ".claudit/"
	newEntry := ".shiftlog/"

	lines := strings.Split(string(data), "\n")
	changed := 0
	for i, line := range lines {
		if strings.TrimSpace(line) == oldEntry {
			lines[i] = newEntry
			changed++
		}
	}

	if changed == 0 {
		return 0, nil
	}

	fmt.Printf("  .gitignore: %s → %s\n", oldEntry, newEntry)
	if !migrateDryRun {
		if err := os.WriteFile(gitignorePath, []byte(strings.Join(lines, "\n")), 0644); err != nil {
			return 0, fmt.Errorf("failed to write .gitignore: %w", err)
		}
	}
	return changed, nil
}

// migrateGitHooks replaces # claudit-managed markers with # shiftlog-managed
// and updates binary references from `claudit` to `shiftlog` in managed sections.
func migrateGitHooks(gitDir string) (int, error) {
	hooksDir := filepath.Join(gitDir, "hooks")
	hookNames := []string{"pre-push", "post-merge", "post-checkout", "post-commit"}
	oldMarker := "# claudit-managed"
	newMarker := "# shiftlog-managed"

	changed := 0
	for _, name := range hookNames {
		hookPath := filepath.Join(hooksDir, name)
		data, err := os.ReadFile(hookPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return changed, fmt.Errorf("failed to read hook %s: %w", name, err)
		}

		content := string(data)
		if !strings.Contains(content, oldMarker) {
			continue
		}

		// Replace marker and update binary name within managed sections
		updated := strings.ReplaceAll(content, oldMarker, newMarker)
		updated = replaceBinaryInManagedSection(updated, newMarker, "claudit", "shiftlog")

		fmt.Printf("  .git/hooks/%s: claudit-managed → shiftlog-managed\n", name)
		if !migrateDryRun {
			if err := os.WriteFile(hookPath, []byte(updated), 0755); err != nil {
				return changed, fmt.Errorf("failed to write hook %s: %w", name, err)
			}
		}
		changed++
	}
	return changed, nil
}

// replaceBinaryInManagedSection replaces oldBin with newBin only inside
// shiftlog-managed sections to avoid touching user content outside those sections.
func replaceBinaryInManagedSection(content, marker, oldBin, newBin string) string {
	startMarker := marker + " start"
	endMarker := marker + " end"

	var result strings.Builder
	remaining := content
	for {
		startIdx := strings.Index(remaining, startMarker)
		if startIdx == -1 {
			result.WriteString(remaining)
			break
		}
		endIdx := strings.Index(remaining, endMarker)
		if endIdx == -1 {
			result.WriteString(remaining)
			break
		}
		endIdx += len(endMarker)

		result.WriteString(remaining[:startIdx])
		section := remaining[startIdx:endIdx]
		result.WriteString(strings.ReplaceAll(section, oldBin, newBin))
		remaining = remaining[endIdx:]
	}
	return result.String()
}

// migrateCopilotHooks renames .github/hooks/claudit.json → shiftlog.json.
func migrateCopilotHooks(repoRoot string) (int, error) {
	hooksDir := filepath.Join(repoRoot, ".github", "hooks")
	oldPath := filepath.Join(hooksDir, "claudit.json")
	newPath := filepath.Join(hooksDir, "shiftlog.json")

	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		return 0, nil
	}

	if _, err := os.Stat(newPath); err == nil {
		fmt.Println("⚠ .github/hooks/shiftlog.json already exists — skipping Copilot hook rename")
		return 0, nil
	}

	fmt.Printf("  .github/hooks/claudit.json → shiftlog.json\n")
	if !migrateDryRun {
		if err := os.Rename(oldPath, newPath); err != nil {
			return 0, fmt.Errorf("failed to rename claudit.json: %w", err)
		}
	}
	return 1, nil
}

// migrateNotesRef copies refs/notes/claude-conversations → refs/notes/shiftlog
// and updates notes.rewriteRef / notes.displayRef git config if they still
// point to the old ref.
func migrateNotesRef() (int, error) {
	oldRef := git.LegacyNotesRef
	newRef := git.NotesRef

	// Check whether the old ref exists
	if exec.Command("git", "rev-parse", "--verify", oldRef).Run() != nil {
		return 0, nil
	}

	// Check whether the new ref already exists (already migrated)
	if exec.Command("git", "rev-parse", "--verify", newRef).Run() == nil {
		fmt.Printf("✓ %s already exists (skipping notes ref migration)\n", newRef)
		return 0, nil
	}

	fmt.Printf("  %s → %s\n", oldRef, newRef)
	if !migrateDryRun {
		shaOut, err := exec.Command("git", "rev-parse", oldRef).Output()
		if err != nil {
			return 0, fmt.Errorf("failed to resolve %s: %w", oldRef, err)
		}
		sha := strings.TrimSpace(string(shaOut))

		if err := exec.Command("git", "update-ref", newRef, sha).Run(); err != nil {
			return 0, fmt.Errorf("failed to copy notes ref: %w", err)
		}

		// Update git config entries that still point to the old ref
		for _, key := range []string{"notes.rewriteRef", "notes.displayRef"} {
			out, err := exec.Command("git", "config", key).Output()
			if err == nil && strings.TrimSpace(string(out)) == oldRef {
				if err := exec.Command("git", "config", key, newRef).Run(); err != nil {
					return 0, fmt.Errorf("failed to update %s: %w", key, err)
				}
			}
		}
	}
	return 1, nil
}

// dirExists reports whether path is an existing directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

