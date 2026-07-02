package acceptance_test

import (
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/re-cinq/shift-log/tests/acceptance/testutil"
)

var _ = Describe("Doctor Command", func() {
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

	Describe("shiftlog doctor", func() {
		It("reports issues when not initialized", func() {
			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "doctor")
			// Should fail because not initialized
			Expect(err).To(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Shiftlog Doctor"))
			Expect(stdout).To(ContainSubstring("FAIL"))
		})

		It("passes all checks after init", func() {
			// Initialize first
			_, _, err := testutil.RunShiftlogInDir(repo.Path, "init")
			Expect(err).NotTo(HaveOccurred())

			// Doctor should pass (except PATH check which we can't control in test)
			stdout, _, _ := testutil.RunShiftlogInDir(repo.Path, "doctor")
			Expect(stdout).To(ContainSubstring("Shiftlog Doctor"))
			Expect(stdout).To(ContainSubstring("Checking git repository... OK"))
			Expect(stdout).To(ContainSubstring("Found PostToolUse hook configuration"))
			Expect(stdout).To(ContainSubstring("All git hooks installed"))
		})

		It("fails outside git repository", func() {
			tmpDir, err := os.MkdirTemp("", "not-a-repo-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			stdout, _, err := testutil.RunShiftlogInDir(tmpDir, "doctor")
			Expect(err).To(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Not inside a git repository"))
		})
	})
})
