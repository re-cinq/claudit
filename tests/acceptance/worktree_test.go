package acceptance_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/re-cinq/shift-log/tests/acceptance/testutil"
)

var _ = Describe("Worktree isolation", func() {
	var repo *testutil.GitRepo
	var worktreePath string

	BeforeEach(func() {
		var err error
		repo, err = testutil.NewGitRepo()
		Expect(err).NotTo(HaveOccurred())

		repo.SetBinaryPath(testutil.BinaryPath())

		// Create initial commit on master
		Expect(repo.WriteFile("README.md", "# Test")).To(Succeed())
		Expect(repo.Commit("Initial commit")).To(Succeed())

		// Init shiftlog in the main repo
		_, _, err = testutil.RunShiftlogInDir(repo.Path, "init")
		Expect(err).NotTo(HaveOccurred())

		// Create branch-b and add a worktree for it
		Expect(repo.Run("git", "branch", "branch-b")).To(Succeed())

		worktreePath, err = os.MkdirTemp("", "shiftlog-worktree-*")
		Expect(err).NotTo(HaveOccurred())
		// git worktree add requires the path not to exist
		Expect(os.RemoveAll(worktreePath)).To(Succeed())
		Expect(repo.Run("git", "worktree", "add", worktreePath, "branch-b")).To(Succeed())
	})

	AfterEach(func() {
		if repo != nil {
			// Remove worktree before cleaning up repo
			_ = repo.Run("git", "worktree", "remove", "--force", worktreePath)
			repo.Cleanup()
		}
		_ = os.RemoveAll(worktreePath)
	})

	// Helper to store a conversation in a given directory
	storeConversation := func(dir, sessionID string) string {
		transcriptPath := filepath.Join(dir, "transcript.jsonl")
		Expect(os.WriteFile(transcriptPath, []byte(testutil.SampleTranscript()), 0644)).To(Succeed())

		// Get HEAD of the repo at dir
		wtRepo := &testutil.GitRepo{Path: dir}
		head, err := wtRepo.GetHead()
		Expect(err).NotTo(HaveOccurred())

		hookInput := testutil.SampleHookInput(sessionID, transcriptPath, "git commit -m 'test'")
		_, _, err = testutil.RunShiftlogInDirWithStdin(dir, hookInput, "store")
		Expect(err).NotTo(HaveOccurred())

		return head
	}

	It("only shows conversations from the current branch, not the other worktree's branch", func() {
		// -- Worktree A (master): make a commit and store a conversation --
		Expect(repo.WriteFile("a.txt", "branch A work")).To(Succeed())
		Expect(repo.Commit("Commit on master")).To(Succeed())
		storeConversation(repo.Path, "session-branch-a")

		// -- Worktree B (branch-b): make a commit and store a conversation --
		wtRepo := &testutil.GitRepo{Path: worktreePath}
		wtRepo.SetBinaryPath(testutil.BinaryPath())
		Expect(wtRepo.WriteFile("b.txt", "branch B work")).To(Succeed())
		Expect(wtRepo.Commit("Commit on branch-b")).To(Succeed())
		storeConversation(worktreePath, "session-branch-b")

		// -- From worktree A (master): shiftlog list should only show master's conversation --
		stdoutA, _, err := testutil.RunShiftlogInDir(repo.Path, "list")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdoutA).To(ContainSubstring("Commit on master"))
		Expect(stdoutA).NotTo(ContainSubstring("Commit on branch-b"),
			"worktree A (master) should NOT see branch-b's conversation")

		// -- From worktree B (branch-b): shiftlog list should only show branch-b's conversation --
		stdoutB, _, err := testutil.RunShiftlogInDir(worktreePath, "list")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdoutB).To(ContainSubstring("Commit on branch-b"))
		Expect(stdoutB).NotTo(ContainSubstring("Commit on master"),
			"worktree B (branch-b) should NOT see master's conversation")
	})

	It("shiftlog show on HEAD only shows the current worktree's conversation", func() {
		// -- Worktree A (master): make a commit and store a conversation --
		Expect(repo.WriteFile("a.txt", "branch A work")).To(Succeed())
		Expect(repo.Commit("Commit on master")).To(Succeed())
		shaA := storeConversation(repo.Path, "session-show-a")

		// -- Worktree B (branch-b): make a commit and store a conversation --
		wtRepo := &testutil.GitRepo{Path: worktreePath}
		wtRepo.SetBinaryPath(testutil.BinaryPath())
		Expect(wtRepo.WriteFile("b.txt", "branch B work")).To(Succeed())
		Expect(wtRepo.Commit("Commit on branch-b")).To(Succeed())
		shaB := storeConversation(worktreePath, "session-show-b")

		// show from worktree A should show A's HEAD conversation
		stdoutA, _, err := testutil.RunShiftlogInDir(repo.Path, "show")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdoutA).To(ContainSubstring(shaA[:7]))

		// show from worktree B should show B's HEAD conversation
		stdoutB, _, err := testutil.RunShiftlogInDir(worktreePath, "show")
		Expect(err).NotTo(HaveOccurred())
		Expect(stdoutB).To(ContainSubstring(shaB[:7]))
	})
})
