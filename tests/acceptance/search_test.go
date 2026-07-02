package acceptance_test

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/re-cinq/shift-log/tests/acceptance/testutil"
)

var _ = Describe("Search Command", func() {
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

	Describe("text search", func() {
		It("finds content in user messages", func() {
			storeConversation("session-search-1")

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "search", "help me with a task")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("help me with a task"))
		})

		It("finds content in assistant messages", func() {
			storeConversation("session-search-2")

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "search", "What would you like help with")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("[assistant]"))
		})

		It("is case-insensitive by default", func() {
			storeConversation("session-search-case")

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "search", "HELLO")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Hello"))
		})

		It("shows commit metadata in results", func() {
			storeConversation("session-search-meta")

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "search", "help")
			Expect(err).NotTo(HaveOccurred())

			// Should contain short SHA, date, commit message
			Expect(stdout).To(MatchRegexp(`[0-9a-f]{7}`))
			Expect(stdout).To(ContainSubstring("Initial commit"))
			Expect(stdout).To(ContainSubstring("messages"))
		})
	})

	Describe("metadata filters", func() {
		It("filters by agent", func() {
			storeConversation("session-search-agent")

			// claude agent (default) should match
			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "search", "--agent", "claude")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Initial commit"))

			// copilot agent should not match
			stdout, _, err = testutil.RunShiftlogInDir(repo.Path, "search", "--agent", "copilot")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("no matching conversations found"))
		})

		It("filters by branch", func() {
			storeConversation("session-search-branch")

			// master branch should match (test repos use master)
			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "search", "--branch", "master")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Initial commit"))

			// other branch should not match
			stdout, _, err = testutil.RunShiftlogInDir(repo.Path, "search", "--branch", "develop")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("no matching conversations found"))
		})
	})

	Describe("limit flag", func() {
		It("caps the number of results", func() {
			// Create two commits with conversations
			storeConversation("session-limit-1")

			Expect(repo.WriteFile("file2.txt", "content")).To(Succeed())
			Expect(repo.Commit("Second commit")).To(Succeed())
			storeConversation("session-limit-2")

			// Limit to 1 result
			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "search", "--agent", "claude", "--limit", "1")
			Expect(err).NotTo(HaveOccurred())

			// Count result headers (lines with SHA pattern followed by date)
			lines := strings.Split(strings.TrimSpace(stdout), "\n")
			headerCount := 0
			for _, line := range lines {
				if len(line) > 0 && !strings.HasPrefix(line, "  ") {
					headerCount++
				}
			}
			Expect(headerCount).To(Equal(1))
		})
	})

	Describe("metadata-only flag", func() {
		It("returns results without searching transcript content", func() {
			storeConversation("session-metaonly")

			// With --metadata-only and a filter, should get results without match snippets
			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "search", "--agent", "claude", "--metadata-only")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Initial commit"))
			// Should not contain match labels like [user] or [assistant]
			Expect(stdout).NotTo(ContainSubstring("[user]"))
			Expect(stdout).NotTo(ContainSubstring("[assistant]"))
		})
	})

	Describe("error cases", func() {
		It("shows error when no query and no filters", func() {
			_, stderr, err := testutil.RunShiftlogInDir(repo.Path, "search")
			Expect(err).To(HaveOccurred())
			Expect(stderr).To(ContainSubstring("provide a search query or at least one filter flag"))
		})

		It("fails outside git repository", func() {
			tmpDir, err := os.MkdirTemp("", "shiftlog-no-git-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			_, stderr, err := testutil.RunShiftlogInDir(tmpDir, "search", "test")
			Expect(err).To(HaveOccurred())
			Expect(stderr).To(ContainSubstring("not inside a git repository"))
		})

		It("shows clean message when no matches", func() {
			storeConversation("session-nomatch")

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "search", "xyznonexistent12345")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("no matching conversations found"))
		})
	})
})
