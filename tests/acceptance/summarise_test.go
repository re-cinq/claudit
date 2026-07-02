package acceptance_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/re-cinq/shift-log/tests/acceptance/testutil"
)

var _ = Describe("Summarise Command", func() {
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

	Describe("without conversation", func() {
		It("shows error when commit has no conversation", func() {
			_, stderr, err := testutil.RunShiftlogInDir(repo.Path, "summarise")
			Expect(err).To(HaveOccurred())
			Expect(stderr).To(ContainSubstring("no conversation found"))
		})

		It("shows error for invalid reference", func() {
			_, stderr, err := testutil.RunShiftlogInDir(repo.Path, "summarise", "invalid-ref-xyz")
			Expect(err).To(HaveOccurred())
			Expect(stderr).To(ContainSubstring("could not resolve reference"))
		})
	})

	Describe("outside git repository", func() {
		It("fails with error", func() {
			tmpDir, err := os.MkdirTemp("", "shiftlog-no-git-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			_, stderr, err := testutil.RunShiftlogInDir(tmpDir, "summarise")
			Expect(err).To(HaveOccurred())
			Expect(stderr).To(ContainSubstring("not inside a git repository"))
		})
	})

	Describe("alias", func() {
		It("tldr alias works the same as summarise", func() {
			_, stderr, err := testutil.RunShiftlogInDir(repo.Path, "tldr")
			Expect(err).To(HaveOccurred())
			// Both should produce the same error for no conversation
			Expect(stderr).To(ContainSubstring("no conversation found"))
		})
	})

	Describe("unsupported agent", func() {
		It("shows error for agents that don't support summarisation", func() {
			storeConversation("session-summarise-unsupported")

			_, stderr, err := testutil.RunShiftlogInDir(repo.Path, "summarise", "--agent=copilot")
			Expect(err).To(HaveOccurred())
			Expect(stderr).To(ContainSubstring("does not support summarisation"))
			Expect(stderr).To(ContainSubstring("--agent=claude"))
		})
	})

	Describe("with conversation and mock agent", func() {
		It("runs the agent and prints output", func() {
			storeConversation("session-summarise-mock")

			// Create a mock "claude" binary that echoes a summary
			mockDir, err := os.MkdirTemp("", "shiftlog-mock-agent-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(mockDir)

			mockScript := `#!/bin/sh
cat <<'SUMMARY'
- User asked for help with a task
- Assistant created a test file
- Used Bash tool to write content
SUMMARY
`
			mockPath := filepath.Join(mockDir, "claude")
			Expect(os.WriteFile(mockPath, []byte(mockScript), 0755)).To(Succeed())

			// Run with mock in PATH
			env := []string{"PATH=" + mockDir + ":" + os.Getenv("PATH")}
			stdout, _, err := testutil.RunShiftlogInDirWithEnv(repo.Path, env, "summarise", "--agent=claude")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("User asked for help"))
			Expect(stdout).To(ContainSubstring("Bash tool"))
		})

		It("passes focus hint to the agent prompt", func() {
			storeConversation("session-summarise-focus")

			// Create a mock "claude" binary that echoes the prompt arg so we can verify it
			mockDir, err := os.MkdirTemp("", "shiftlog-mock-focus-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(mockDir)

			// Mock agent echoes the last positional argument (the prompt)
			mockScript := `#!/bin/sh
for arg; do :; done
printf '%s' "$arg"
`
			mockPath := filepath.Join(mockDir, "claude")
			Expect(os.WriteFile(mockPath, []byte(mockScript), 0755)).To(Succeed())

			env := []string{"PATH=" + mockDir + ":" + os.Getenv("PATH")}
			stdout, _, err := testutil.RunShiftlogInDirWithEnv(repo.Path, env, "summarise", "--agent=claude", "--focus=security changes")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Pay particular attention to: security changes"))
		})

		It("works without focus flag", func() {
			storeConversation("session-summarise-no-focus")

			mockDir, err := os.MkdirTemp("", "shiftlog-mock-nofocus-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(mockDir)

			// Mock agent echoes the last positional argument (the prompt)
			mockScript := `#!/bin/sh
for arg; do :; done
printf '%s' "$arg"
`
			mockPath := filepath.Join(mockDir, "claude")
			Expect(os.WriteFile(mockPath, []byte(mockScript), 0755)).To(Succeed())

			env := []string{"PATH=" + mockDir + ":" + os.Getenv("PATH")}
			stdout, _, err := testutil.RunShiftlogInDirWithEnv(repo.Path, env, "summarise", "--agent=claude")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).NotTo(ContainSubstring("Pay particular attention to"))
		})
	})

	Describe("ref resolution", func() {
		It("supports HEAD~1 syntax", func() {
			// Create first commit with conversation
			storeConversation("session-summarise-ref")

			// Create second commit without conversation
			Expect(repo.WriteFile("file2.txt", "content")).To(Succeed())
			Expect(repo.Commit("Second commit")).To(Succeed())

			// HEAD~1 should attempt to summarise the previous commit's conversation
			// It will fail at the agent binary step, but the ref resolution should work
			_, stderr, err := testutil.RunShiftlogInDir(repo.Path, "summarise", "HEAD~1")
			// Will fail because 'claude' mock just exits 0 with empty output
			// The important thing is it doesn't fail at ref resolution
			if err != nil {
				// Should NOT be a ref resolution error
				Expect(stderr).NotTo(ContainSubstring("could not resolve reference"))
			}
		})
	})
})
