package acceptance_test

import (
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/re-cinq/shift-log/tests/acceptance/testutil"
)

var _ = Describe("Deinit Command", func() {
	for _, cfg := range testutil.AllAgentConfigs() {
		config := cfg // capture loop variable

		Describe(config.Name+" agent", func() {
			var repo *testutil.GitRepo

			BeforeEach(func() {
				var err error
				repo, err = testutil.NewGitRepo()
				Expect(err).NotTo(HaveOccurred())

				if config.NeedsBinaryPath {
					repo.SetBinaryPath(testutil.BinaryPath())
				}
			})

			AfterEach(func() {
				if repo != nil {
					repo.Cleanup()
				}
			})

			It("removes agent hooks after init", func() {
				// Init first
				_, _, err := testutil.RunShiftlogInDir(repo.Path, config.InitArgs...)
				Expect(err).NotTo(HaveOccurred())

				// Verify settings file exists (for agents that have one)
				if config.SettingsFile != "" {
					Expect(repo.FileExists(config.SettingsFile)).To(BeTrue())
				}

				// Deinit
				_, _, err = testutil.RunShiftlogInDir(repo.Path, "deinit")
				Expect(err).NotTo(HaveOccurred())

				// Verify shiftlog hooks are gone
				if config.IsHookless {
					// Nothing to check — hookless agents have no settings file
				} else if config.IsPluginBased {
					// Plugin file should be removed
					Expect(repo.FileExists(config.SettingsFile)).To(BeFalse())
				} else if config.IsRepoRootHooks {
					// Hooks file should be removed (shiftlog-owned)
					Expect(repo.FileExists(config.SettingsFile)).To(BeFalse())
				} else {
					// Settings file should be removed (shiftlog-only content)
					Expect(repo.FileExists(config.SettingsFile)).To(BeFalse())
				}
			})

			if !config.IsHookless && !config.IsPluginBased && !config.IsRepoRootHooks {
				It("preserves non-shiftlog settings", func() {
					// Create pre-existing settings
					settingsDir := filepath.Dir(filepath.Join(repo.Path, config.SettingsFile))
					Expect(os.MkdirAll(settingsDir, 0755)).To(Succeed())
					Expect(repo.WriteFile(config.SettingsFile, `{"existingKey": "existingValue"}`)).To(Succeed())

					// Init then deinit
					_, _, err := testutil.RunShiftlogInDir(repo.Path, config.InitArgs...)
					Expect(err).NotTo(HaveOccurred())
					_, _, err = testutil.RunShiftlogInDir(repo.Path, "deinit")
					Expect(err).NotTo(HaveOccurred())

					// Settings file should still exist with existing content
					Expect(repo.FileExists(config.SettingsFile)).To(BeTrue())
					content, err := repo.ReadFile(config.SettingsFile)
					Expect(err).NotTo(HaveOccurred())
					Expect(content).To(ContainSubstring("existingKey"))
					Expect(content).To(ContainSubstring("existingValue"))
					// But should not have shiftlog hooks
					Expect(content).NotTo(ContainSubstring("shiftlog store"))
				})
			}

			if config.IsRepoRootHooks {
				It("preserves non-shiftlog hooks in repo-root hooks file", func() {
					Expect(os.MkdirAll(filepath.Join(repo.Path, ".github", "hooks"), 0755)).To(Succeed())
					Expect(repo.WriteFile(config.SettingsFile, `{"version":1,"hooks":{"postToolUse":[{"type":"command","command":"other-tool","timeoutSec":10}]},"existingKey":"existingValue"}`)).To(Succeed())

					_, _, err := testutil.RunShiftlogInDir(repo.Path, config.InitArgs...)
					Expect(err).NotTo(HaveOccurred())
					_, _, err = testutil.RunShiftlogInDir(repo.Path, "deinit")
					Expect(err).NotTo(HaveOccurred())

					Expect(repo.FileExists(config.SettingsFile)).To(BeTrue())
					content, err := repo.ReadFile(config.SettingsFile)
					Expect(err).NotTo(HaveOccurred())
					Expect(content).To(ContainSubstring("other-tool"))
					Expect(content).To(ContainSubstring("existingKey"))
					Expect(content).NotTo(ContainSubstring("shiftlog store"))
				})
			}

			It("is idempotent", func() {
				// Init then deinit twice — second deinit should not error
				_, _, err := testutil.RunShiftlogInDir(repo.Path, config.InitArgs...)
				Expect(err).NotTo(HaveOccurred())
				_, _, err = testutil.RunShiftlogInDir(repo.Path, "deinit")
				Expect(err).NotTo(HaveOccurred())
				_, _, err = testutil.RunShiftlogInDir(repo.Path, "deinit")
				Expect(err).NotTo(HaveOccurred())
			})
		})
	}

	// Shared tests that run once (not per-agent)
	Describe("shared behavior", func() {
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

		It("removes git hooks", func() {
			_, _, err := testutil.RunShiftlogInDir(repo.Path, "init")
			Expect(err).NotTo(HaveOccurred())

			// Verify hooks exist
			for _, hook := range []string{"pre-push", "post-merge", "post-checkout", "post-commit"} {
				Expect(repo.FileExists(".git/hooks/" + hook)).To(BeTrue())
			}

			_, _, err = testutil.RunShiftlogInDir(repo.Path, "deinit")
			Expect(err).NotTo(HaveOccurred())

			// Verify hook files are deleted (they only had shiftlog content)
			for _, hook := range []string{"pre-push", "post-merge", "post-checkout", "post-commit"} {
				Expect(repo.FileExists(".git/hooks/" + hook)).To(BeFalse())
			}
		})

		It("preserves non-shiftlog hook content", func() {
			// Write custom pre-push hook first
			hooksDir := filepath.Join(repo.Path, ".git", "hooks")
			Expect(os.MkdirAll(hooksDir, 0755)).To(Succeed())
			Expect(os.WriteFile(
				filepath.Join(hooksDir, "pre-push"),
				[]byte("#!/bin/sh\necho custom-hook\n"),
				0755,
			)).To(Succeed())

			// Init adds shiftlog section
			_, _, err := testutil.RunShiftlogInDir(repo.Path, "init")
			Expect(err).NotTo(HaveOccurred())

			content, err := repo.ReadFile(".git/hooks/pre-push")
			Expect(err).NotTo(HaveOccurred())
			Expect(content).To(ContainSubstring("custom-hook"))
			Expect(content).To(ContainSubstring("shiftlog-managed"))

			// Deinit removes only shiftlog section
			_, _, err = testutil.RunShiftlogInDir(repo.Path, "deinit")
			Expect(err).NotTo(HaveOccurred())

			// File should still exist with custom content
			Expect(repo.FileExists(".git/hooks/pre-push")).To(BeTrue())
			content, err = repo.ReadFile(".git/hooks/pre-push")
			Expect(err).NotTo(HaveOccurred())
			Expect(content).To(ContainSubstring("custom-hook"))
			Expect(content).NotTo(ContainSubstring("shiftlog-managed"))
		})

		It("removes git config settings", func() {
			_, _, err := testutil.RunShiftlogInDir(repo.Path, "init")
			Expect(err).NotTo(HaveOccurred())

			// Verify settings are set
			out, err := repo.RunOutput("git", "config", "notes.displayRef")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("refs/notes/shiftlog"))

			// Deinit
			_, _, err = testutil.RunShiftlogInDir(repo.Path, "deinit")
			Expect(err).NotTo(HaveOccurred())

			// Verify settings are unset
			_, err = repo.RunOutput("git", "config", "notes.displayRef")
			Expect(err).To(HaveOccurred()) // git config exits non-zero when key is missing
		})

		It("works without prior init", func() {
			_, _, err := testutil.RunShiftlogInDir(repo.Path, "deinit")
			Expect(err).NotTo(HaveOccurred())
		})

		It("fails outside git repository", func() {
			tmpDir, err := os.MkdirTemp("", "not-a-repo-*")
			Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(tmpDir)

			_, stderr, err := testutil.RunShiftlogInDir(tmpDir, "deinit")
			Expect(err).To(HaveOccurred())
			Expect(stderr).To(ContainSubstring("not inside a git repository"))
		})

		It("reads agent from config", func() {
			// Init with gemini
			_, _, err := testutil.RunShiftlogInDir(repo.Path, "init", "--agent=gemini")
			Expect(err).NotTo(HaveOccurred())

			// Verify config has agent=gemini
			content, err := repo.ReadFile(".shiftlog/config")
			Expect(err).NotTo(HaveOccurred())
			var cfg map[string]interface{}
			Expect(json.Unmarshal([]byte(content), &cfg)).To(Succeed())
			Expect(cfg["agent"]).To(Equal("gemini"))

			// Verify gemini settings exist
			Expect(repo.FileExists(".gemini/settings.json")).To(BeTrue())

			// Deinit (no --agent flag needed)
			_, _, err = testutil.RunShiftlogInDir(repo.Path, "deinit")
			Expect(err).NotTo(HaveOccurred())

			// Gemini settings should be removed
			Expect(repo.FileExists(".gemini/settings.json")).To(BeFalse())
		})

		It("prints informative output", func() {
			_, _, err := testutil.RunShiftlogInDir(repo.Path, "init")
			Expect(err).NotTo(HaveOccurred())

			stdout, _, err := testutil.RunShiftlogInDir(repo.Path, "deinit")
			Expect(err).NotTo(HaveOccurred())

			Expect(stdout).To(ContainSubstring("Removed"))
			Expect(stdout).To(ContainSubstring("hooks"))
			Expect(stdout).To(ContainSubstring("Git notes data has been preserved"))
		})
	})
})
