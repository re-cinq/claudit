package acceptance_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/re-cinq/shift-log/tests/acceptance/testutil"
)

var _ = Describe("Show Command", func() {
	var repo *testutil.GitRepo

	BeforeEach(func() {
		var err error
		repo, err = testutil.NewGitRepo()
		Expect(err).NotTo(HaveOccurred())

		// Create initial commit
		Expect(repo.WriteFile("README.md", "# Test")).To(Succeed())
		Expect(repo.Commit("Initial commit")).To(Succeed())
	})

	AfterEach(func() {
		if repo != nil {
			repo.Cleanup()
		}
	})

	// Helper to store a conversation on the current commit
	storeConversation := func(sessionID string) string {
		transcriptPath := filepath.Join(repo.Path, "transcript.jsonl")
		Expect(os.WriteFile(transcriptPath, []byte(testutil.SampleTranscript()), 0644)).To(Succeed())

		head, err := repo.GetHead()
		Expect(err).NotTo(HaveOccurred())

		hookInput := testutil.SampleHookInput(sessionID, transcriptPath, "git commit -m 'test'")
		_, _, err = testutil.RunShiftlogInDirWithStdin(repo.Path, hookInput, "store")
		Expect(err).NotTo(HaveOccurred())

		return head
	}

	Describe("with conversation", func() {
		It("shows conversation for commit SHA", func() {
			commitSHA := storeConversation("session-show-1")

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "show", commitSHA)
			Expect(err).NotTo(HaveOccurred())

			// Should contain conversation header
			Expect(stdout).To(ContainSubstring(commitSHA[:7]))
			Expect(stdout).To(ContainSubstring("Showing:"))
		})

		It("shows conversation for HEAD when no ref provided", func() {
			storeConversation("session-show-2")

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "show")
			Expect(err).NotTo(HaveOccurred())

			// Should show conversation
			Expect(stdout).To(ContainSubstring("Conversation for"))
		})

		It("displays User: prefix for user messages", func() {
			storeConversation("session-show-3")

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "show")
			Expect(err).NotTo(HaveOccurred())

			Expect(stdout).To(ContainSubstring("User:"))
		})

		It("displays Assistant: prefix for assistant messages", func() {
			storeConversation("session-show-4")

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "show")
			Expect(err).NotTo(HaveOccurred())

			Expect(stdout).To(ContainSubstring("Assistant:"))
		})

		It("displays message content", func() {
			storeConversation("session-show-5")

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "show")
			Expect(err).NotTo(HaveOccurred())

			// Should contain the sample transcript content
			Expect(stdout).To(ContainSubstring("Hello, can you help me with a task?"))
			Expect(stdout).To(ContainSubstring("Of course! What would you like help with?"))
		})
	})

	Describe("without conversation", func() {
		It("shows error when commit has no conversation", func() {
			head, err := repo.GetHead()
			Expect(err).NotTo(HaveOccurred())

			_, stderr, err := testutil.RunShiftlogInDir(repo.Path, "show", head)
			Expect(err).To(HaveOccurred())
			Expect(stderr).To(ContainSubstring("no conversation found"))
		})

		It("shows error when HEAD has no conversation", func() {
			_, stderr, err := testutil.RunShiftlogInDir(repo.Path, "show")
			Expect(err).To(HaveOccurred())
			Expect(stderr).To(ContainSubstring("no conversation found"))
		})
	})

	Describe("with invalid reference", func() {
		It("shows error for invalid commit reference", func() {
			_, stderr, err := testutil.RunShiftlogInDir(repo.Path, "show", "invalid-ref-xyz")
			Expect(err).To(HaveOccurred())
			Expect(stderr).To(ContainSubstring("could not resolve reference"))
		})
	})

	Describe("relative references", func() {
		It("supports HEAD~1 syntax", func() {
			// Create first commit with conversation
			storeConversation("session-relative-1")

			// Create second commit without conversation
			Expect(repo.WriteFile("file2.txt", "content")).To(Succeed())
			Expect(repo.Commit("Second commit")).To(Succeed())

			// HEAD~1 should show the previous commit's conversation
			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "show", "HEAD~1")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("User:"))
		})
	})

	Describe("outside git repository", func() {
		It("fails with error", func() {
			tmpDir, err := os.MkdirTemp("", "shiftlog-no-git-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			_, stderr, err := testutil.RunShiftlogInDir(tmpDir, "show")
			Expect(err).To(HaveOccurred())
			Expect(stderr).To(ContainSubstring("not inside a git repository"))
		})
	})

	Describe("incremental display", func() {
		var sessionID string

		BeforeEach(func() {
			sessionID = "session-incremental-test"
		})

		// Helper to store a conversation with specific UUIDs
		storeConversationWithUUIDs := func(uuids []string, messages []string) string {
			transcriptPath := filepath.Join(repo.Path, "transcript.jsonl")
			transcript := testutil.SampleTranscriptWithIDs(uuids, messages)
			Expect(os.WriteFile(transcriptPath, []byte(transcript), 0644)).To(Succeed())

			head, err := repo.GetHead()
			Expect(err).NotTo(HaveOccurred())

			hookInput := testutil.SampleHookInput(sessionID, transcriptPath, "git commit -m 'test'")
			_, _, err = testutil.RunShiftlogInDirWithStdin(repo.Path, hookInput, "store")
			Expect(err).NotTo(HaveOccurred())

			return head
		}

		It("shows only entries since parent commit by default", func() {
			// First commit with entries 1-4
			firstUUIDs := []string{"uuid-1", "uuid-2", "uuid-3", "uuid-4"}
			firstMessages := []string{"First user message", "First assistant response", "Second user message", "Second assistant response"}
			storeConversationWithUUIDs(firstUUIDs, firstMessages)

			// Create second commit
			Expect(repo.WriteFile("file2.txt", "content")).To(Succeed())
			Expect(repo.Commit("Second commit")).To(Succeed())

			// Second commit with entries 1-6 (includes all previous plus 2 new)
			secondUUIDs := []string{"uuid-1", "uuid-2", "uuid-3", "uuid-4", "uuid-5", "uuid-6"}
			secondMessages := []string{"First user message", "First assistant response", "Second user message", "Second assistant response", "Third user message", "Third assistant response"}
			storeConversationWithUUIDs(secondUUIDs, secondMessages)

			// Show should only display entries after uuid-4 (the last entry from first commit)
			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "show")
			Expect(err).NotTo(HaveOccurred())

			// Should contain the new messages
			Expect(stdout).To(ContainSubstring("Third user message"))
			Expect(stdout).To(ContainSubstring("Third assistant response"))

			// Should NOT contain the old messages
			Expect(stdout).NotTo(ContainSubstring("First user message"))
			Expect(stdout).NotTo(ContainSubstring("Second user message"))

			// Should indicate incremental display
			Expect(stdout).To(ContainSubstring("since"))
			Expect(stdout).To(ContainSubstring("2 entries"))
		})

		It("shows full session with --full flag", func() {
			// First commit
			firstUUIDs := []string{"uuid-1", "uuid-2"}
			firstMessages := []string{"First user message", "First assistant response"}
			storeConversationWithUUIDs(firstUUIDs, firstMessages)

			// Second commit
			Expect(repo.WriteFile("file2.txt", "content")).To(Succeed())
			Expect(repo.Commit("Second commit")).To(Succeed())

			secondUUIDs := []string{"uuid-1", "uuid-2", "uuid-3", "uuid-4"}
			secondMessages := []string{"First user message", "First assistant response", "Second user message", "Second assistant response"}
			storeConversationWithUUIDs(secondUUIDs, secondMessages)

			// Show with --full should display all entries
			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "show", "--full")
			Expect(err).NotTo(HaveOccurred())

			// Should contain all messages
			Expect(stdout).To(ContainSubstring("First user message"))
			Expect(stdout).To(ContainSubstring("Second user message"))
			Expect(stdout).To(ContainSubstring("full session"))
		})

		It("shows full session for first commit (no parent)", func() {
			// Only one commit with conversation
			uuids := []string{"uuid-1", "uuid-2"}
			messages := []string{"First user message", "First assistant response"}
			storeConversationWithUUIDs(uuids, messages)

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "show")
			Expect(err).NotTo(HaveOccurred())

			// Should show full session since no parent conversation exists
			Expect(stdout).To(ContainSubstring("First user message"))
			Expect(stdout).To(ContainSubstring("full session"))
		})

		It("shows full session when parent has no conversation", func() {
			// First commit without conversation
			// (already created in BeforeEach with initial commit)

			// Second commit with conversation
			Expect(repo.WriteFile("file2.txt", "content")).To(Succeed())
			Expect(repo.Commit("Second commit")).To(Succeed())

			uuids := []string{"uuid-1", "uuid-2"}
			messages := []string{"First user message", "First assistant response"}
			storeConversationWithUUIDs(uuids, messages)

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "show")
			Expect(err).NotTo(HaveOccurred())

			// Should show full session since parent has no conversation
			Expect(stdout).To(ContainSubstring("First user message"))
			Expect(stdout).To(ContainSubstring("full session"))
		})

		It("shows full session when session ID differs from parent", func() {
			// First commit with one session ID
			firstUUIDs := []string{"uuid-1", "uuid-2"}
			firstMessages := []string{"First user message", "First assistant response"}

			transcriptPath := filepath.Join(repo.Path, "transcript.jsonl")
			transcript := testutil.SampleTranscriptWithIDs(firstUUIDs, firstMessages)
			Expect(os.WriteFile(transcriptPath, []byte(transcript), 0644)).To(Succeed())

			hookInput := testutil.SampleHookInput("session-A", transcriptPath, "git commit -m 'test'")
			_, _, err := testutil.RunShiftlogInDirWithStdin(repo.Path, hookInput, "store")
			Expect(err).NotTo(HaveOccurred())

			// Second commit with different session ID
			Expect(repo.WriteFile("file2.txt", "content")).To(Succeed())
			Expect(repo.Commit("Second commit")).To(Succeed())

			secondUUIDs := []string{"uuid-new-1", "uuid-new-2"}
			secondMessages := []string{"New session message", "New session response"}
			transcript = testutil.SampleTranscriptWithIDs(secondUUIDs, secondMessages)
			Expect(os.WriteFile(transcriptPath, []byte(transcript), 0644)).To(Succeed())

			hookInput = testutil.SampleHookInput("session-B", transcriptPath, "git commit -m 'test'")
			_, _, err = testutil.RunShiftlogInDirWithStdin(repo.Path, hookInput, "store")
			Expect(err).NotTo(HaveOccurred())

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "show")
			Expect(err).NotTo(HaveOccurred())

			// Should show full session since session IDs differ
			Expect(stdout).To(ContainSubstring("New session message"))
			Expect(stdout).To(ContainSubstring("full session"))
		})
	})
})
