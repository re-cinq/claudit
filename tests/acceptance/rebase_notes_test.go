package acceptance_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/re-cinq/shift-log/tests/acceptance/testutil"
)

var _ = Describe("Local Rebase Notes Preservation", func() {
	var repo *testutil.GitRepo

	BeforeEach(func() {
		var err error
		repo, err = testutil.NewGitRepo()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if repo != nil {
			repo.Cleanup()
		}
	})

	Describe("notes follow rebased commits", func() {
		It("preserves notes when commits are rebased", func() {
			// Set binary path so git hooks installed by init can find shiftlog
			repo.SetBinaryPath(testutil.BinaryPath())

			// Initialize shiftlog (sets notes.rewriteRef)
			_, _, err := testutil.RunShiftlogInDir(repo.Path, "init")
			Expect(err).NotTo(HaveOccurred())

			// Create base commit on master
			Expect(repo.WriteFile("base.txt", "base\n")).To(Succeed())
			Expect(repo.Commit("base commit")).To(Succeed())

			// Create a feature branch
			Expect(repo.Run("git", "checkout", "-b", "feature")).To(Succeed())

			// Create a commit on feature branch
			Expect(repo.WriteFile("feature.txt", "feature\n")).To(Succeed())
			Expect(repo.Commit("feature commit")).To(Succeed())

			featureHead, err := repo.GetHead()
			Expect(err).NotTo(HaveOccurred())

			// Add a note to the feature commit
			repo.AddNote("refs/notes/shiftlog", featureHead, `{"session_id":"rebase-test","version":1}`)
			Expect(repo.HasNote("refs/notes/shiftlog", featureHead)).To(BeTrue())

			// Go back to master and add a new commit (so rebase will rewrite)
			Expect(repo.Run("git", "checkout", "master")).To(Succeed())
			Expect(repo.WriteFile("master2.txt", "master2\n")).To(Succeed())
			Expect(repo.Commit("second master commit")).To(Succeed())

			// Rebase feature onto master
			Expect(repo.Run("git", "checkout", "feature")).To(Succeed())
			Expect(repo.Run("git", "rebase", "master")).To(Succeed())

			// The old SHA should no longer be HEAD
			newHead, err := repo.GetHead()
			Expect(err).NotTo(HaveOccurred())
			Expect(newHead).NotTo(Equal(featureHead))

			// The note should now be on the NEW commit SHA
			Expect(repo.HasNote("refs/notes/shiftlog", newHead)).To(BeTrue())

			noteContent, err := repo.GetNote("refs/notes/shiftlog", newHead)
			Expect(err).NotTo(HaveOccurred())
			Expect(noteContent).To(ContainSubstring("rebase-test"))
		})
	})

	Describe("doctor validates notes.rewriteRef", func() {
		It("reports OK when notes.rewriteRef is configured", func() {
			// Initialize (sets notes.rewriteRef)
			_, _, err := testutil.RunShiftlogInDir(repo.Path, "init")
			Expect(err).NotTo(HaveOccurred())

			stdout, _, _ := testutil.RunShiftlogInDir(repo.Path, "doctor")
			Expect(stdout).To(ContainSubstring("Checking notes.rewriteRef config... OK"))
			Expect(stdout).To(ContainSubstring("Notes will follow commits during rebase"))
		})

		It("reports FAIL when notes.rewriteRef is missing", func() {
			// Initialize, then remove the config
			_, _, err := testutil.RunShiftlogInDir(repo.Path, "init")
			Expect(err).NotTo(HaveOccurred())

			// Unset notes.rewriteRef
			Expect(repo.Run("git", "config", "--unset", "notes.rewriteRef")).To(Succeed())

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "doctor")
			Expect(err).To(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Checking notes.rewriteRef config... FAIL"))
			Expect(stdout).To(ContainSubstring("Notes will not follow commits during rebase"))
		})

		It("reports FAIL when not initialized at all", func() {
			// Fresh git repo without shiftlog init — doctor should report FAIL
			freshRepo, err := testutil.NewGitRepo()
			Expect(err).NotTo(HaveOccurred())
			defer freshRepo.Cleanup()

			stdout, _, _ := testutil.RunShiftlogInDir(freshRepo.Path, "doctor")
			Expect(stdout).To(ContainSubstring("Checking notes.rewriteRef config... FAIL"))
		})
	})
})
