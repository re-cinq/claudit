package acceptance_test

import (
	"encoding/json"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/re-cinq/shift-log/internal/storage"
	"github.com/re-cinq/shift-log/tests/acceptance/testutil"
)

var _ = Describe("Resume Command", func() {
	for _, cfg := range testutil.AllAgentConfigs() {
		config := cfg // capture loop variable

		Describe(config.Name+" agent", func() {
			var repo *testutil.GitRepo
			var agentEnv *testutil.AgentEnv

			BeforeEach(func() {
				var err error
				repo, err = testutil.NewGitRepo()
				Expect(err).NotTo(HaveOccurred())

				if config.NeedsBinaryPath {
					repo.SetBinaryPath(testutil.BinaryPath())
				}

				agentEnv, err = testutil.NewAgentEnv(config)
				Expect(err).NotTo(HaveOccurred())

				// Initialize agent hooks so store works correctly
				_, _, err = testutil.RunShiftlogInDir(repo.Path, config.InitArgs...)
				Expect(err).NotTo(HaveOccurred())

				// Create initial commit
				Expect(repo.WriteFile("README.md", "# Test")).To(Succeed())
				Expect(repo.Commit("Initial commit")).To(Succeed())
			})

			AfterEach(func() {
				if repo != nil {
					repo.Cleanup()
				}
				if agentEnv != nil {
					agentEnv.Cleanup()
				}
			})

			// Helper to store a conversation on the current commit
			storeConversation := func(sessionID string) string {
				hookParam, err := config.PrepareTranscript(repo.Path, sessionID, config.SampleTranscript())
				Expect(err).NotTo(HaveOccurred())

				head, err := repo.GetHead()
				Expect(err).NotTo(HaveOccurred())

				hookInput := config.SampleHookInput(sessionID, hookParam, "git commit -m 'test'")
				_, _, err = testutil.RunShiftlogInDirWithStdin(repo.Path, hookInput, config.StoreArgs...)
				Expect(err).NotTo(HaveOccurred())

				return head
			}

			Describe("resolving commit references", func() {
				It("resolves full SHA", func() {
					commitSHA := storeConversation("session-full-sha")

					stdout, _, _ := testutil.RunShiftlogInDirWithEnv(
						repo.Path,
						agentEnv.GetEnvVars(),
						"resume", commitSHA, "--force",
					)

					Expect(stdout).To(ContainSubstring("restored session"))
				})

				It("resolves short SHA", func() {
					commitSHA := storeConversation("session-short-sha")
					shortSHA := commitSHA[:7]

					stdout, _, _ := testutil.RunShiftlogInDirWithEnv(
						repo.Path,
						agentEnv.GetEnvVars(),
						"resume", shortSHA, "--force",
					)

					Expect(stdout).To(ContainSubstring("restored session"))
				})

				It("resolves HEAD reference", func() {
					storeConversation("session-head-ref")

					stdout, _, _ := testutil.RunShiftlogInDirWithEnv(
						repo.Path,
						agentEnv.GetEnvVars(),
						"resume", "HEAD", "--force",
					)

					Expect(stdout).To(ContainSubstring("restored session"))
				})
			})

			Describe("restoring session files", func() {
				It("writes transcript to agent session directory", func() {
					commitSHA := storeConversation("session-restore-test")

					testutil.RunShiftlogInDirWithEnv(
						repo.Path,
						agentEnv.GetEnvVars(),
						"resume", commitSHA, "--force",
					)

					Expect(agentEnv.SessionFileExists(repo.Path, "session-restore-test")).To(BeTrue())
				})

				if config.HasSessionsIndex {
					It("creates sessions-index.json", func() {
						commitSHA := storeConversation("session-index-test")

						testutil.RunShiftlogInDirWithEnv(
							repo.Path,
							agentEnv.GetEnvVars(),
							"resume", commitSHA, "--force",
						)

						Expect(agentEnv.SessionsIndexExists(repo.Path)).To(BeTrue())
					})
				}
			})

			Describe("handling missing conversations", func() {
				It("fails when commit has no conversation", func() {
					Expect(repo.WriteFile("file.txt", "content")).To(Succeed())
					Expect(repo.Commit("Commit without conversation")).To(Succeed())

					head, err := repo.GetHead()
					Expect(err).NotTo(HaveOccurred())

					_, stderr, err := testutil.RunShiftlogInDirWithEnv(
						repo.Path,
						agentEnv.GetEnvVars(),
						"resume", head, "--force",
					)

					Expect(err).To(HaveOccurred())
					Expect(stderr).To(ContainSubstring("no conversation found"))
				})

				It("fails with invalid commit reference", func() {
					_, stderr, err := testutil.RunShiftlogInDirWithEnv(
						repo.Path,
						agentEnv.GetEnvVars(),
						"resume", "invalid-ref", "--force",
					)

					Expect(err).To(HaveOccurred())
					Expect(stderr).To(ContainSubstring("could not resolve commit"))
				})
			})

			Describe("handling uncommitted changes", func() {
				It("warns about uncommitted changes without --force", func() {
					commitSHA := storeConversation("session-dirty")

					Expect(repo.WriteFile("uncommitted.txt", "changes")).To(Succeed())

					_, stderr, _ := testutil.RunShiftlogInDirWithEnvAndStdin(
						repo.Path,
						agentEnv.GetEnvVars(),
						"n\n",
						"resume", commitSHA,
					)

					Expect(stderr).To(ContainSubstring("uncommitted changes"))
				})

				It("proceeds with --force flag despite uncommitted changes", func() {
					commitSHA := storeConversation("session-force")

					Expect(repo.WriteFile("uncommitted.txt", "changes")).To(Succeed())

					stdout, _, _ := testutil.RunShiftlogInDirWithEnv(
						repo.Path,
						agentEnv.GetEnvVars(),
						"resume", commitSHA, "--force",
					)

					Expect(stdout).To(ContainSubstring("restored session"))
				})
			})

			Describe("handling corrupt conversations", func() {
				It("fails when note contains invalid JSON", func() {
					head, err := repo.GetHead()
					Expect(err).NotTo(HaveOccurred())

					repo.AddNote("refs/notes/shiftlog", head, "not valid json at all")

					_, stderr, err := testutil.RunShiftlogInDirWithEnv(
						repo.Path,
						agentEnv.GetEnvVars(),
						"resume", head, "--force",
					)

					Expect(err).To(HaveOccurred())
					Expect(stderr).To(ContainSubstring("could not parse"))
				})

				It("warns on checksum mismatch but still restores", func() {
					head, err := repo.GetHead()
					Expect(err).NotTo(HaveOccurred())

					transcript := []byte(config.SampleTranscript())
					sc, err := storage.NewStoredConversation("session-tampered", repo.Path, "master", 4, transcript)
					Expect(err).NotTo(HaveOccurred())

					sc.Checksum = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

					noteData, err := sc.Marshal()
					Expect(err).NotTo(HaveOccurred())
					repo.AddNote("refs/notes/shiftlog", head, string(noteData))

					stdout, stderr, _ := testutil.RunShiftlogInDirWithEnv(
						repo.Path,
						agentEnv.GetEnvVars(),
						"resume", head, "--force",
					)

					Expect(stderr).To(ContainSubstring("checksum mismatch"))
					Expect(stdout).To(ContainSubstring("restored session"))
				})

				It("fails when transcript cannot be decompressed", func() {
					head, err := repo.GetHead()
					Expect(err).NotTo(HaveOccurred())

					note := map[string]interface{}{
						"version":       1,
						"session_id":    "session-corrupt",
						"timestamp":     "2025-01-01T00:00:00Z",
						"project_path":  repo.Path,
						"git_branch":    "master",
						"message_count": 1,
						"checksum":      "sha256:abcd",
						"transcript":    "dGhpcyBpcyBub3QgZ3ppcCBkYXRh", // base64("this is not gzip data")
					}
					noteData, err := json.Marshal(note)
					Expect(err).NotTo(HaveOccurred())
					repo.AddNote("refs/notes/shiftlog", head, string(noteData))

					_, stderr, err := testutil.RunShiftlogInDirWithEnv(
						repo.Path,
						agentEnv.GetEnvVars(),
						"resume", head, "--force",
					)

					Expect(err).To(HaveOccurred())
					Expect(stderr).To(ContainSubstring("could not decompress"))
				})

				It("fails when transcript field is not valid base64", func() {
					head, err := repo.GetHead()
					Expect(err).NotTo(HaveOccurred())

					note := map[string]interface{}{
						"version":       1,
						"session_id":    "session-bad-b64",
						"timestamp":     "2025-01-01T00:00:00Z",
						"project_path":  repo.Path,
						"git_branch":    "master",
						"message_count": 1,
						"checksum":      "sha256:abcd",
						"transcript":    "!!!not-base64!!!",
					}
					noteData, err := json.Marshal(note)
					Expect(err).NotTo(HaveOccurred())
					repo.AddNote("refs/notes/shiftlog", head, string(noteData))

					_, stderr, err := testutil.RunShiftlogInDirWithEnv(
						repo.Path,
						agentEnv.GetEnvVars(),
						"resume", head, "--force",
					)

					Expect(err).To(HaveOccurred())
					Expect(stderr).To(SatisfyAny(
						ContainSubstring("could not verify transcript integrity"),
						ContainSubstring("could not decompress"),
					))
				})
			})

			Describe("resolving relative references", func() {
				It("resolves HEAD~1 to parent commit", func() {
					storeConversation("session-parent")

					Expect(repo.WriteFile("second.txt", "content")).To(Succeed())
					Expect(repo.Commit("Second commit")).To(Succeed())

					stdout, _, _ := testutil.RunShiftlogInDirWithEnv(
						repo.Path,
						agentEnv.GetEnvVars(),
						"resume", "HEAD~1", "--force",
					)

					Expect(stdout).To(ContainSubstring("restored session"))
					Expect(stdout).To(ContainSubstring("session-parent"))
				})
			})

			Describe("session content verification", func() {
				It("restores the original transcript content", func() {
					originalTranscript := config.SampleTranscript()
					commitSHA := storeConversation("session-content-verify")

					testutil.RunShiftlogInDirWithEnv(
						repo.Path,
						agentEnv.GetEnvVars(),
						"resume", commitSHA, "--force",
					)

					content, err := agentEnv.ReadRestoredTranscript(repo.Path, "session-content-verify")
					Expect(err).NotTo(HaveOccurred())
					Expect(string(content)).To(Equal(originalTranscript))
				})

				if config.HasSessionsIndex {
					It("populates sessions-index.json with correct metadata", func() {
						commitSHA := storeConversation("session-meta-check")

						testutil.RunShiftlogInDirWithEnv(
							repo.Path,
							agentEnv.GetEnvVars(),
							"resume", commitSHA, "--force",
						)

						indexPath := agentEnv.GetSessionsIndexPath(repo.Path)
						indexData, err := os.ReadFile(indexPath)
						Expect(err).NotTo(HaveOccurred())

						var index map[string]interface{}
						Expect(json.Unmarshal(indexData, &index)).To(Succeed())

						entries := index["entries"].([]interface{})
						Expect(entries).To(HaveLen(1))

						entry := entries[0].(map[string]interface{})
						Expect(entry["sessionId"]).To(Equal("session-meta-check"))
					})
				}
			})
		})
	}

	// Shared test that runs once (not per-agent)
	Describe("requires arguments", func() {
		var repo *testutil.GitRepo

		BeforeEach(func() {
			var err error
			repo, err = testutil.NewGitRepo()
			Expect(err).NotTo(HaveOccurred())

			Expect(repo.WriteFile("README.md", "# Test")).To(Succeed())
			Expect(repo.Commit("Initial commit")).To(Succeed())
		})

		AfterEach(func() {
			if repo != nil {
				repo.Cleanup()
			}
		})

		It("fails when no commit argument is provided", func() {
			_, stderr, err := testutil.RunShiftlogInDir(
				repo.Path,
				"resume",
			)

			Expect(err).To(HaveOccurred())
			Expect(stderr).To(ContainSubstring("accepts 1 arg"))
		})
	})
})
