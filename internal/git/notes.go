package git

import (
	"errors"
	"os/exec"
	"strings"
)

// NotesRef is the git notes ref used to store conversation notes.
// A custom ref keeps git log clean and avoids collisions with other notes.
const NotesRef = "refs/notes/shiftlog"

// NotesTrackingRef is the ref used to hold fetched remote notes before merging.
const NotesTrackingRef = "refs/notes/shiftlog-remote"

// LegacyNotesRef is the old ref name used before multi-agent support.
// Used by the migrate command to upgrade existing repos.
const LegacyNotesRef = "refs/notes/claude-conversations"

// ErrNonFastForward is returned when a push fails because the remote has diverged.
var ErrNonFastForward = errors.New("non-fast-forward update: remote notes have diverged, run 'shiftlog sync pull' first")

// AddNote adds a note to a commit.
// Content is piped via stdin (-F -) to avoid ARG_MAX limits on large transcripts.
func AddNote(commitSHA string, content []byte) error {
	cmd := exec.Command("git", "notes", "--ref", NotesRef, "add", "-f", "-F", "-", commitSHA)
	cmd.Stdin = strings.NewReader(string(content))
	return cmd.Run()
}

// GetNote retrieves a note from a commit
func GetNote(commitSHA string) ([]byte, error) {
	cmd := exec.Command("git", "notes", "--ref", NotesRef, "show", commitSHA)
	return cmd.Output()
}

// HasNote checks if a commit has a conversation note
func HasNote(commitSHA string) bool {
	cmd := exec.Command("git", "notes", "--ref", NotesRef, "show", commitSHA)
	return cmd.Run() == nil
}

// ListCommitsWithNotes returns a list of commit SHAs that have conversation notes
// sorted in reverse chronological order (matching git log)
func ListCommitsWithNotes() ([]string, error) {
	cmd := exec.Command("git", "notes", "--ref", NotesRef, "list")
	output, err := cmd.Output()
	if err != nil {
		// No notes exist yet - this is not an error
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}

	// Build a set of commits with notes
	commitSet := make(map[string]bool)
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		// Format: "note_sha commit_sha"
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			commitSet[parts[1]] = true
		}
	}

	if len(commitSet) == 0 {
		return nil, nil
	}

	// Use git rev-list to sort commits in reverse chronological order
	// HEAD scopes to the current branch, --topo-order maintains parent-child relationships
	cmd = exec.Command("git", "rev-list", "HEAD", "--topo-order")
	output, err = cmd.Output()
	if err != nil {
		return nil, err
	}

	// Filter to only commits that have notes, preserving git's order
	var commits []string
	for _, sha := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if sha == "" {
			continue
		}
		if commitSet[sha] {
			commits = append(commits, sha)
		}
	}

	return commits, nil
}

// ListAllCommitsWithNotes returns the set of commit SHAs that have conversation
// notes, regardless of branch reachability. Unlike ListCommitsWithNotes it does
// not filter through `git rev-list HEAD`.
// If repoDir is non-empty, the git command runs in that directory.
func ListAllCommitsWithNotes(repoDir string) (map[string]bool, error) {
	cmd := exec.Command("git", "notes", "--ref", NotesRef, "list")
	if repoDir != "" {
		cmd.Dir = repoDir
	}
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}

	commitSet := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			commitSet[parts[1]] = true
		}
	}
	return commitSet, nil
}

// PushNotes pushes notes to the remote.
// Returns ErrNonFastForward if the remote has diverged.
func PushNotes(remote string) error {
	// Use --no-verify to prevent pre-push hook from triggering recursively
	cmd := exec.Command("git", "push", "--no-verify", remote, NotesRef)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "non-fast-forward") ||
			strings.Contains(string(output), "rejected") ||
			strings.Contains(string(output), "fetch first") {
			return ErrNonFastForward
		}
		return err
	}
	return nil
}

// FetchNotesToTracking fetches remote notes to the tracking ref without
// touching the local notes ref. This is the first step of the
// fetch-then-merge sync flow.
func FetchNotesToTracking(remote string) error {
	cmd := exec.Command("git", "fetch", remote, NotesRef+":"+NotesTrackingRef)
	return cmd.Run()
}

// MergeNotes merges the tracking ref into the local notes ref using
// git notes merge. The cat_sort_uniq strategy concatenates notes when
// two developers have annotated the same commit SHA.
func MergeNotes() error {
	cmd := exec.Command("git", "notes", "--ref", NotesRef, "merge", "--strategy=cat_sort_uniq", NotesTrackingRef)
	return cmd.Run()
}

// CopyNote copies a note from one commit to another.
// If the destination already has a note, the copy is forced (overwritten).
func CopyNote(fromSHA, toSHA string) error {
	cmd := exec.Command("git", "notes", "--ref", NotesRef, "copy", "-f", fromSHA, toSHA)
	return cmd.Run()
}

// FindOrphanedNotes returns notes whose commits are not reachable from any branch.
// Returns a map of commit SHA → note blob SHA.
func FindOrphanedNotes() (map[string]string, error) {
	// List all notes
	cmd := exec.Command("git", "notes", "--ref", NotesRef, "list")
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}

	orphaned := make(map[string]string)
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		noteSHA := parts[0]
		commitSHA := parts[1]

		// Check if commit is reachable from any branch
		cmd := exec.Command("git", "branch", "--contains", commitSHA)
		branchOutput, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(branchOutput)) == "" {
			// Not on any branch — check the object still exists
			checkCmd := exec.Command("git", "cat-file", "-t", commitSHA)
			if checkCmd.Run() == nil {
				orphaned[commitSHA] = noteSHA
			}
		}
	}

	return orphaned, nil
}

// PatchID computes the git patch-id for a commit.
// The patch-id is a stable hash of the commit's diff, independent of the SHA.
func PatchID(commitSHA string) (string, error) {
	diffCmd := exec.Command("git", "diff-tree", "-p", commitSHA)
	patchCmd := exec.Command("git", "patch-id")

	pipe, err := diffCmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	patchCmd.Stdin = pipe

	patchOut, err := patchCmd.StdoutPipe()
	if err != nil {
		return "", err
	}

	if err := diffCmd.Start(); err != nil {
		return "", err
	}
	if err := patchCmd.Start(); err != nil {
		return "", err
	}

	buf := make([]byte, 4096)
	n, _ := patchOut.Read(buf)
	output := strings.TrimSpace(string(buf[:n]))

	// Wait for both to finish
	_ = diffCmd.Wait()
	_ = patchCmd.Wait()

	if output == "" {
		return "", nil
	}

	fields := strings.Fields(output)
	if len(fields) < 1 {
		return "", nil
	}

	return fields[0], nil
}

// ListCommitsInRange returns commit SHAs in the given range (e.g. "ORIG_HEAD..HEAD").
func ListCommitsInRange(rangeSpec string) ([]string, error) {
	cmd := exec.Command("git", "rev-list", rangeSpec)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var commits []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line != "" {
			commits = append(commits, line)
		}
	}
	return commits, nil
}

// ListAllBranchCommits returns all commit SHAs reachable from any branch.
func ListAllBranchCommits() ([]string, error) {
	cmd := exec.Command("git", "rev-list", "--all")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var commits []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line != "" {
			commits = append(commits, line)
		}
	}
	return commits, nil
}
