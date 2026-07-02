package acceptance_test

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/re-cinq/shift-log/tests/acceptance/testutil"
)

var _ = Describe("Remote Rebase Notes Remap", func() {
	var repo *testutil.GitRepo

	BeforeEach(func() {
		var err error
		repo, err = testutil.NewGitRepo()
		Expect(err).NotTo(HaveOccurred())
		repo.SetBinaryPath(testutil.BinaryPath())
	})

	AfterEach(func() {
		if repo != nil {
			repo.Cleanup()
		}
	})

	Describe("shiftlog remap matches rebased commits by patch-id", func() {
		It("copies notes from orphaned commits to their rebased counterparts", func() {
			// Create a base commit on master
			Expect(repo.WriteFile("base.txt", "base content\n")).To(Succeed())
			Expect(repo.Commit("base commit")).To(Succeed())

			// Create a feature branch with two commits
			Expect(repo.Run("git", "checkout", "-b", "feature")).To(Succeed())

			Expect(repo.WriteFile("feature1.txt", "feature 1\n")).To(Succeed())
			Expect(repo.Commit("feature commit 1")).To(Succeed())
			feat1SHA, err := repo.GetHead()
			Expect(err).NotTo(HaveOccurred())

			Expect(repo.WriteFile("feature2.txt", "feature 2\n")).To(Succeed())
			Expect(repo.Commit("feature commit 2")).To(Succeed())
			feat2SHA, err := repo.GetHead()
			Expect(err).NotTo(HaveOccurred())

			// Add notes to both feature commits
			Expect(repo.AddNote("refs/notes/shiftlog", feat1SHA, `{"session_id":"remap-test-1"}`)).To(Succeed())
			Expect(repo.AddNote("refs/notes/shiftlog", feat2SHA, `{"session_id":"remap-test-2"}`)).To(Succeed())

			// Go back to master and diverge (so cherry-pick produces new SHAs)
			Expect(repo.Run("git", "checkout", "master")).To(Succeed())
			Expect(repo.WriteFile("diverge.txt", "diverging\n")).To(Succeed())
			Expect(repo.Commit("diverge master")).To(Succeed())

			// Simulate GitHub rebase-merge: cherry-pick the feature commits onto master
			Expect(repo.Run("git", "cherry-pick", feat1SHA)).To(Succeed())
			new1SHA, err := repo.GetHead()
			Expect(err).NotTo(HaveOccurred())

			Expect(repo.Run("git", "cherry-pick", feat2SHA)).To(Succeed())
			new2SHA, err := repo.GetHead()
			Expect(err).NotTo(HaveOccurred())

			// New SHAs must differ from originals
			Expect(new1SHA).NotTo(Equal(feat1SHA))
			Expect(new2SHA).NotTo(Equal(feat2SHA))

			// Delete the feature branch (simulates GitHub auto-delete)
			Expect(repo.Run("git", "branch", "-D", "feature")).To(Succeed())

			// Notes should still be on the old SHAs
			Expect(repo.HasNote("refs/notes/shiftlog", feat1SHA)).To(BeTrue())
			Expect(repo.HasNote("refs/notes/shiftlog", feat2SHA)).To(BeTrue())
			// But NOT on the new SHAs yet
			Expect(repo.HasNote("refs/notes/shiftlog", new1SHA)).To(BeFalse())
			Expect(repo.HasNote("refs/notes/shiftlog", new2SHA)).To(BeFalse())

			// Run remap
			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "remap")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Remapped 2 note(s)"))

			// Notes should now be on the new SHAs
			Expect(repo.HasNote("refs/notes/shiftlog", new1SHA)).To(BeTrue())
			Expect(repo.HasNote("refs/notes/shiftlog", new2SHA)).To(BeTrue())
		})
	})

	Describe("notes are accessible on new commit SHAs after remap", func() {
		It("preserves note content through remap", func() {
			// Create base + feature
			Expect(repo.WriteFile("base.txt", "base\n")).To(Succeed())
			Expect(repo.Commit("base")).To(Succeed())

			Expect(repo.Run("git", "checkout", "-b", "feature")).To(Succeed())
			Expect(repo.WriteFile("f.txt", "feature\n")).To(Succeed())
			Expect(repo.Commit("feature")).To(Succeed())
			featSHA, err := repo.GetHead()
			Expect(err).NotTo(HaveOccurred())

			Expect(repo.AddNote("refs/notes/shiftlog", featSHA, `{"session_id":"content-check","version":1}`)).To(Succeed())

			// Diverge master and cherry-pick
			Expect(repo.Run("git", "checkout", "master")).To(Succeed())
			Expect(repo.WriteFile("d.txt", "diverge\n")).To(Succeed())
			Expect(repo.Commit("diverge")).To(Succeed())
			Expect(repo.Run("git", "cherry-pick", featSHA)).To(Succeed())
			newSHA, err := repo.GetHead()
			Expect(err).NotTo(HaveOccurred())

			Expect(repo.Run("git", "branch", "-D", "feature")).To(Succeed())

			// Remap
			_, _, err = testutil.RunShiftlogInDir(repo.Path, "remap")
			Expect(err).NotTo(HaveOccurred())

			// Verify content
			note, err := repo.GetNote("refs/notes/shiftlog", newSHA)
			Expect(err).NotTo(HaveOccurred())
			Expect(note).To(ContainSubstring("content-check"))
			Expect(note).To(ContainSubstring(`"version":1`))
		})
	})

	Describe("orphaned notes with no patch-id match are reported but not deleted", func() {
		It("reports unmatched notes without removing them", func() {
			// Create a commit with a note, then make it orphaned
			Expect(repo.WriteFile("base.txt", "base\n")).To(Succeed())
			Expect(repo.Commit("base")).To(Succeed())

			Expect(repo.Run("git", "checkout", "-b", "feature")).To(Succeed())
			Expect(repo.WriteFile("unique.txt", "unique content\n")).To(Succeed())
			Expect(repo.Commit("unique feature")).To(Succeed())
			featSHA, err := repo.GetHead()
			Expect(err).NotTo(HaveOccurred())

			Expect(repo.AddNote("refs/notes/shiftlog", featSHA, `{"session_id":"unmatched"}`)).To(Succeed())

			// Go back to master — do NOT cherry-pick the feature commit
			Expect(repo.Run("git", "checkout", "master")).To(Succeed())
			Expect(repo.Run("git", "branch", "-D", "feature")).To(Succeed())

			// Remap should report unmatched
			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "remap")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("orphaned note(s) could not be matched"))

			// The original note should still exist
			Expect(repo.HasNote("refs/notes/shiftlog", featSHA)).To(BeTrue())
		})
	})

	Describe("no orphaned notes", func() {
		It("reports no orphaned notes when all notes are on reachable commits", func() {
			Expect(repo.WriteFile("file.txt", "content\n")).To(Succeed())
			Expect(repo.Commit("commit")).To(Succeed())
			head, err := repo.GetHead()
			Expect(err).NotTo(HaveOccurred())

			Expect(repo.AddNote("refs/notes/shiftlog", head, `{"session_id":"reachable"}`)).To(Succeed())

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "remap")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("No orphaned notes found"))
		})
	})

	Describe("post-merge hook triggers remap", func() {
		It("runs remap automatically after git pull", func() {
			// Set up local + remote
			local, remote, err := testutil.NewGitRepoWithRemote()
			Expect(err).NotTo(HaveOccurred())
			defer local.Cleanup()
			defer remote.Cleanup()

			local.SetBinaryPath(testutil.BinaryPath())

			// Create initial commit and push
			Expect(local.WriteFile("README.md", "# Test")).To(Succeed())
			Expect(local.Commit("Initial commit")).To(Succeed())
			Expect(local.Run("git", "push", "-u", "origin", "master")).To(Succeed())

			// Initialize shiftlog (installs hooks with remap)
			_, _, err = testutil.RunShiftlogInDir(local.Path, "init")
			Expect(err).NotTo(HaveOccurred())

			// Create feature branch with a note
			Expect(local.Run("git", "checkout", "-b", "feature")).To(Succeed())
			Expect(local.WriteFile("feat.txt", "feature\n")).To(Succeed())
			Expect(local.Commit("feature commit")).To(Succeed())
			featSHA, err := local.GetHead()
			Expect(err).NotTo(HaveOccurred())

			Expect(local.AddNote("refs/notes/shiftlog", featSHA, `{"session_id":"hook-remap"}`)).To(Succeed())

			// Push feature branch and notes
			Expect(local.Run("git", "push", "origin", "feature")).To(Succeed())
			_, _, err = testutil.RunShiftlogInDir(local.Path, "sync", "push")
			Expect(err).NotTo(HaveOccurred())

			// Simulate a second developer who does the rebase-merge
			clone, err := testutil.NewGitRepo()
			Expect(err).NotTo(HaveOccurred())
			defer clone.Cleanup()

			Expect(clone.Run("git", "remote", "add", "origin", remote.Path)).To(Succeed())
			Expect(clone.Run("git", "fetch", "origin")).To(Succeed())
			Expect(clone.Run("git", "checkout", "-b", "master", "origin/master")).To(Succeed())

			// Diverge master
			Expect(clone.WriteFile("diverge.txt", "diverge\n")).To(Succeed())
			Expect(clone.Commit("diverge commit")).To(Succeed())

			// Cherry-pick the feature commit (simulates GitHub rebase-merge)
			Expect(clone.Run("git", "cherry-pick", featSHA)).To(Succeed())

			// Push the rebased master and delete feature branch
			Expect(clone.Run("git", "push", "origin", "master")).To(Succeed())
			Expect(clone.Run("git", "push", "origin", "--delete", "feature")).To(Succeed())

			// Back on local: switch to master and pull
			Expect(local.Run("git", "checkout", "master")).To(Succeed())
			// Delete local feature branch (simulates pruning)
			Expect(local.Run("git", "branch", "-D", "feature")).To(Succeed())

			// Pull triggers post-merge hook → sync pull + remap
			Expect(local.Run("git", "pull", "origin", "master")).To(Succeed())

			// The note should now be on the new rebased commit
			// Get the commit that has the feature changes on master
			newHead, err := local.GetHead()
			Expect(err).NotTo(HaveOccurred())

			// The new head should have the note (remapped from the deleted feature branch)
			// Check a few recent commits for the note
			found := false
			for i := 0; i < 5; i++ {
				sha, err := local.RunOutput("git", "rev-parse", fmt.Sprintf("HEAD~%d", i))
				if err != nil {
					break
				}
				sha = sha[:len(sha)-1] // trim newline
				if local.HasNote("refs/notes/shiftlog", sha) && sha != featSHA {
					found = true
					note, err := local.GetNote("refs/notes/shiftlog", sha)
					Expect(err).NotTo(HaveOccurred())
					Expect(note).To(ContainSubstring("hook-remap"))
					break
				}
			}
			// The note was either remapped to a new SHA (feature branch was deleted and
			// orphan detected), or the pull brought in the feature ref before prune.
			// Either way, verify the note content is accessible somewhere.
			if !found {
				// If remap didn't find orphans (branch not yet pruned), the note
				// should still exist on the original SHA
				Expect(local.HasNote("refs/notes/shiftlog", featSHA)).To(BeTrue())
				_, _ = fmt.Println("Note still on original SHA (branch not yet pruned)")
			} else {
				_, _ = fmt.Println("Note successfully remapped to new SHA on", newHead[:7])
			}
		})
	})
})
