package cmd

import (
	"fmt"

	"github.com/re-cinq/shift-log/internal/cli"
	"github.com/re-cinq/shift-log/internal/git"
	"github.com/spf13/cobra"
)

var remapCmd = &cobra.Command{
	Use:     "remap",
	Short:   "Remap orphaned notes to rebased commits",
	GroupID: "hooks",
	Long: `Detects conversation notes on commits that are no longer on any branch
(orphaned after a GitHub rebase merge) and copies them to matching commits
using git patch-id.

This runs automatically in the post-merge hook after 'shiftlog sync pull'.
You can also run it manually after pulling a rebase-merged PR.`,
	RunE: runRemap,
}

func init() {
	rootCmd.AddCommand(remapCmd)
}

func runRemap(cmd *cobra.Command, args []string) error {
	if err := git.RequireGitRepo(); err != nil {
		return err
	}

	cli.LogDebug("remap: scanning for orphaned notes")

	// Find notes whose commits are not on any branch
	orphaned, err := git.FindOrphanedNotes()
	if err != nil {
		return fmt.Errorf("failed to find orphaned notes: %w", err)
	}

	if len(orphaned) == 0 {
		fmt.Println("No orphaned notes found")
		return nil
	}

	cli.LogDebug("remap: found %d orphaned notes", len(orphaned))

	// Compute patch-ids for orphaned commits
	orphanPatchIDs := make(map[string]string) // patch-id → commit SHA
	for commitSHA := range orphaned {
		patchID, err := git.PatchID(commitSHA)
		if err != nil || patchID == "" {
			cli.LogDebug("remap: could not compute patch-id for %s: %v", commitSHA[:7], err)
			continue
		}
		orphanPatchIDs[patchID] = commitSHA
		cli.LogDebug("remap: orphaned %s patch-id=%s", commitSHA[:7], patchID[:12])
	}

	if len(orphanPatchIDs) == 0 {
		fmt.Printf("Found %d orphaned notes but could not compute patch-ids (commits may have been garbage collected)\n", len(orphaned))
		return nil
	}

	// Get candidate commits to match against.
	// Try ORIG_HEAD..HEAD first (set by git pull/merge), fall back to all branch commits.
	candidates, err := git.ListCommitsInRange("ORIG_HEAD..HEAD")
	if err != nil || len(candidates) == 0 {
		cli.LogDebug("remap: ORIG_HEAD..HEAD not available, scanning all branch commits")
		candidates, err = git.ListAllBranchCommits()
		if err != nil {
			return fmt.Errorf("failed to list branch commits: %w", err)
		}
	}

	// Compute patch-ids for candidates and match
	remapped := 0
	for _, candidateSHA := range candidates {
		patchID, err := git.PatchID(candidateSHA)
		if err != nil || patchID == "" {
			continue
		}

		if orphanSHA, ok := orphanPatchIDs[patchID]; ok {
			// Skip if old and new SHA are the same
			if orphanSHA == candidateSHA {
				continue
			}

			cli.LogDebug("remap: matched %s → %s (patch-id=%s)", orphanSHA[:7], candidateSHA[:7], patchID[:12])

			if err := git.CopyNote(orphanSHA, candidateSHA); err != nil {
				cli.LogWarning("failed to copy note from %s to %s: %v", orphanSHA[:7], candidateSHA[:7], err)
				continue
			}
			remapped++
			delete(orphanPatchIDs, patchID)
		}
	}

	// Report results
	unmatched := len(orphanPatchIDs)
	if remapped > 0 {
		fmt.Printf("Remapped %d note(s) to rebased commits\n", remapped)
	}
	if unmatched > 0 {
		fmt.Printf("%d orphaned note(s) could not be matched\n", unmatched)
	}
	if remapped == 0 && unmatched == 0 {
		fmt.Println("No orphaned notes found")
	}

	return nil
}
