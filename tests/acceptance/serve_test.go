package acceptance_test

import (
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/re-cinq/shift-log/tests/acceptance/testutil"
)

var _ = Describe("Serve Command", func() {
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

	Describe("serve command basics", func() {
		It("fails outside git repository", func() {
			// Create a temp directory that's not a git repo
			tmpDir, err := os.MkdirTemp("", "shiftlog-no-git-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			_, stderr, err := testutil.RunShiftlogInDir(tmpDir, "serve", "--port", "0")
			Expect(err).To(HaveOccurred())
			Expect(stderr).To(ContainSubstring("not inside a git repository"))
		})

		It("accepts --port flag", func() {
			// This test just verifies the flag is accepted
			// We use port 0 to avoid actually binding
			stdout, _, _ := testutil.RunShiftlog("serve", "--help")
			Expect(stdout).To(ContainSubstring("--port"))
			Expect(stdout).To(ContainSubstring("--no-browser"))
		})

		It("accepts --no-browser flag", func() {
			stdout, _, _ := testutil.RunShiftlog("serve", "--help")
			Expect(stdout).To(ContainSubstring("--no-browser"))
		})
	})
})
