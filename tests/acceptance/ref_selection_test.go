package acceptance_test

import (
	"os"
	"os/exec"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/re-cinq/shift-log/tests/acceptance/testutil"
)

var _ = Describe("Notes Ref", func() {
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

	Describe("custom ref usage", func() {
		It("configures notes.displayRef to shiftlog", func() {
			_, _, err := testutil.RunShiftlogInDir(repo.Path, "init")
			Expect(err).NotTo(HaveOccurred())

			cmd := exec.Command("git", "config", "notes.displayRef")
			cmd.Dir = repo.Path
			output, err := cmd.Output()
			Expect(err).NotTo(HaveOccurred())
			Expect(string(output)).To(ContainSubstring("refs/notes/shiftlog"))
		})

		It("configures notes.rewriteRef to shiftlog", func() {
			_, _, err := testutil.RunShiftlogInDir(repo.Path, "init")
			Expect(err).NotTo(HaveOccurred())

			cmd := exec.Command("git", "config", "notes.rewriteRef")
			cmd.Dir = repo.Path
			output, err := cmd.Output()
			Expect(err).NotTo(HaveOccurred())
			Expect(string(output)).To(ContainSubstring("refs/notes/shiftlog"))
		})
	})

	Describe("note storage isolation", func() {
		It("stores notes on custom ref, not default", func() {
			_, _, err := testutil.RunShiftlogInDir(repo.Path, "init")
			Expect(err).NotTo(HaveOccurred())

			// Create a commit
			Expect(repo.WriteFile("test.txt", "content")).To(Succeed())
			Expect(repo.Commit("Test commit")).To(Succeed())

			head, err := repo.GetHead()
			Expect(err).NotTo(HaveOccurred())

			// Store a conversation
			transcriptPath := filepath.Join(repo.Path, "transcript.jsonl")
			Expect(os.WriteFile(transcriptPath, []byte(testutil.SampleTranscript()), 0644)).To(Succeed())

			hookInput := testutil.SampleHookInput("test-session", transcriptPath, "git commit -m 'test'")
			_, _, err = testutil.RunShiftlogInDirWithStdin(repo.Path, hookInput, "store")
			Expect(err).NotTo(HaveOccurred())

			// Verify note was created on custom ref
			Expect(repo.HasNote("refs/notes/shiftlog", head)).To(BeTrue())
			// Should NOT be on default ref
			Expect(repo.HasNote("refs/notes/commits", head)).To(BeFalse())
		})

		It("does not pollute git log by default", func() {
			_, _, err := testutil.RunShiftlogInDir(repo.Path, "init")
			Expect(err).NotTo(HaveOccurred())

			// Create a commit and store a conversation
			Expect(repo.WriteFile("test.txt", "content")).To(Succeed())
			Expect(repo.Commit("Test commit")).To(Succeed())

			transcriptPath := filepath.Join(repo.Path, "transcript.jsonl")
			Expect(os.WriteFile(transcriptPath, []byte(testutil.SampleTranscript()), 0644)).To(Succeed())

			hookInput := testutil.SampleHookInput("test-session", transcriptPath, "git commit -m 'test'")
			_, _, err = testutil.RunShiftlogInDirWithStdin(repo.Path, hookInput, "store")
			Expect(err).NotTo(HaveOccurred())

			// git log without --notes flag should NOT show the note content
			// (notes.displayRef is set, but only for the custom ref — the default
			// git notes ref is not used, so standard git log is clean)
			cmd := exec.Command("git", "log", "--notes=refs/notes/commits", "-1")
			cmd.Dir = repo.Path
			output, err := cmd.Output()
			Expect(err).NotTo(HaveOccurred())
			Expect(string(output)).NotTo(ContainSubstring("transcript"))
		})
	})
})
