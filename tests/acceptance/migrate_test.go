package acceptance_test

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/re-cinq/shift-log/tests/acceptance/testutil"
)

var _ = Describe("Migrate Command", func() {
	var repo *testutil.GitRepo

	BeforeEach(func() {
		var err error
		repo, err = testutil.NewGitRepo()
		Expect(err).NotTo(HaveOccurred())
		repo.SetBinaryPath(testutil.BinaryPath())
	})

	AfterEach(func() {
		repo.Cleanup()
	})

	Describe("config directory migration", func() {
		It("renames .claudit/ to .shiftlog/", func() {
			clauditDir := filepath.Join(repo.Path, ".claudit")
			Expect(os.MkdirAll(clauditDir, 0755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(clauditDir, "config"), []byte(`{"agent":"claude"}`), 0644)).To(Succeed())

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "migrate")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring(".claudit/ → .shiftlog/"))

			Expect(repo.FileExists(".shiftlog/config")).To(BeTrue())
			Expect(repo.FileExists(".claudit")).To(BeFalse())
		})

		It("skips rename when .shiftlog/ already exists", func() {
			Expect(os.MkdirAll(filepath.Join(repo.Path, ".shiftlog"), 0755)).To(Succeed())

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "migrate")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("already exists"))
		})

		It("warns when both .claudit/ and .shiftlog/ exist", func() {
			Expect(os.MkdirAll(filepath.Join(repo.Path, ".claudit"), 0755)).To(Succeed())
			Expect(os.MkdirAll(filepath.Join(repo.Path, ".shiftlog"), 0755)).To(Succeed())

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "migrate")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Both .claudit/ and .shiftlog/ exist"))
		})

		It("reports nothing to migrate when no claudit artifacts exist", func() {
			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "migrate")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Nothing to migrate"))
		})
	})

	Describe("gitignore migration", func() {
		It("replaces .claudit/ with .shiftlog/ in .gitignore", func() {
			gitignorePath := filepath.Join(repo.Path, ".gitignore")
			Expect(os.WriteFile(gitignorePath, []byte(".claudit/\n*.log\n"), 0644)).To(Succeed())

			_, _, err := testutil.RunShiftlogInDir(repo.Path, "migrate")
			Expect(err).NotTo(HaveOccurred())

			content, err := os.ReadFile(gitignorePath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(ContainSubstring(".shiftlog/"))
			Expect(string(content)).NotTo(ContainSubstring(".claudit/"))
			Expect(string(content)).To(ContainSubstring("*.log"))
		})

		It("leaves .gitignore unchanged when no .claudit/ entry", func() {
			gitignorePath := filepath.Join(repo.Path, ".gitignore")
			original := "*.log\n.env\n"
			Expect(os.WriteFile(gitignorePath, []byte(original), 0644)).To(Succeed())

			_, _, err := testutil.RunShiftlogInDir(repo.Path, "migrate")
			Expect(err).NotTo(HaveOccurred())

			content, err := os.ReadFile(gitignorePath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(Equal(original))
		})
	})

	Describe("git hook migration", func() {
		It("replaces # claudit-managed with # shiftlog-managed in hooks", func() {
			hooksDir := filepath.Join(repo.Path, ".git", "hooks")
			Expect(os.MkdirAll(hooksDir, 0755)).To(Succeed())

			oldHook := "#!/bin/sh\n# claudit-managed start\nclaudit sync push\n# claudit-managed end\n"
			hookPath := filepath.Join(hooksDir, "pre-push")
			Expect(os.WriteFile(hookPath, []byte(oldHook), 0755)).To(Succeed())

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "migrate")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("pre-push"))

			content, err := os.ReadFile(hookPath)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(content)).To(ContainSubstring("# shiftlog-managed"))
			Expect(string(content)).NotTo(ContainSubstring("# claudit-managed"))
			// Binary name updated inside managed section
			Expect(string(content)).To(ContainSubstring("shiftlog sync push"))
		})

		It("leaves user content outside managed sections unchanged", func() {
			hooksDir := filepath.Join(repo.Path, ".git", "hooks")
			Expect(os.MkdirAll(hooksDir, 0755)).To(Succeed())

			oldHook := "#!/bin/sh\necho claudit is cool\n# claudit-managed start\nclaudit sync push\n# claudit-managed end\n"
			hookPath := filepath.Join(hooksDir, "pre-push")
			Expect(os.WriteFile(hookPath, []byte(oldHook), 0755)).To(Succeed())

			_, _, migrateErr := testutil.RunShiftlogInDir(repo.Path, "migrate")
			Expect(migrateErr).NotTo(HaveOccurred())

			content, err := os.ReadFile(hookPath)
			Expect(err).NotTo(HaveOccurred())
			// User content outside managed section is preserved verbatim
			Expect(string(content)).To(ContainSubstring("echo claudit is cool"))
		})
	})

	Describe("Copilot hook migration", func() {
		It("renames .github/hooks/claudit.json to shiftlog.json", func() {
			hooksDir := filepath.Join(repo.Path, ".github", "hooks")
			Expect(os.MkdirAll(hooksDir, 0755)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(hooksDir, "claudit.json"), []byte(`{}`), 0644)).To(Succeed())

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "migrate")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("claudit.json → shiftlog.json"))

			Expect(repo.FileExists(".github/hooks/shiftlog.json")).To(BeTrue())
			Expect(repo.FileExists(".github/hooks/claudit.json")).To(BeFalse())
		})
	})

	Describe("--dry-run flag", func() {
		It("reports changes without applying them", func() {
			clauditDir := filepath.Join(repo.Path, ".claudit")
			Expect(os.MkdirAll(clauditDir, 0755)).To(Succeed())
			gitignorePath := filepath.Join(repo.Path, ".gitignore")
			Expect(os.WriteFile(gitignorePath, []byte(".claudit/\n"), 0644)).To(Succeed())

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "migrate", "--dry-run")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("Dry run"))
			Expect(stdout).To(ContainSubstring("would be applied"))

			// Nothing was actually changed
			Expect(repo.FileExists(".claudit")).To(BeTrue())
			Expect(repo.FileExists(".shiftlog")).To(BeFalse())
			content, _ := os.ReadFile(gitignorePath)
			Expect(string(content)).To(ContainSubstring(".claudit/"))
		})
	})

	Describe("idempotency", func() {
		It("is safe to run twice", func() {
			Expect(os.MkdirAll(filepath.Join(repo.Path, ".claudit"), 0755)).To(Succeed())

			_, _, err := testutil.RunShiftlogInDir(repo.Path, "migrate")
			Expect(err).NotTo(HaveOccurred())
			Expect(repo.FileExists(".shiftlog")).To(BeTrue())

			// Run again — should not error
			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "migrate")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("already exists"))
		})
	})

	Describe("not in a git repo", func() {
		It("returns an error", func() {
			tmpDir, err := os.MkdirTemp("", "shiftlog-no-git-*")
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = os.RemoveAll(tmpDir) }()

			_, stderr, err := testutil.RunShiftlogInDir(tmpDir, "migrate")
			Expect(err).To(HaveOccurred())
			Expect(strings.ToLower(stderr)).To(ContainSubstring("git"))
		})
	})
})
