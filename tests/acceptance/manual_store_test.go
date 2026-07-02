package acceptance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/re-cinq/shift-log/tests/acceptance/testutil"
)

var _ = Describe("Manual Store Command", func() {
	var repo *testutil.GitRepo

	BeforeEach(func() {
		var err error
		repo, err = testutil.NewGitRepo()
		Expect(err).NotTo(HaveOccurred())

		// Initialize shiftlog
		_, _, err = testutil.RunShiftlogInDir(repo.Path, "init")
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		repo.Cleanup()
	})

	Describe("shiftlog store --manual", func() {
		It("exits silently when no active session exists", func() {
			// Make a commit first
			repo.WriteFile("test.txt", "content")
			repo.Run("git", "add", "test.txt")
			repo.Run("git", "commit", "-m", "test commit")

			// Run manual store without active session
			stdout, stderr, err := testutil.RunShiftlogInDir(repo.Path, "store", "--manual")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(BeEmpty())
			// Should not produce warning since this is expected behavior
			Expect(stderr).NotTo(ContainSubstring("error"))
		})

		It("stores conversation when active session exists", func() {
			// Create Claude session directory structure
			claudeProjectsDir := filepath.Join(os.TempDir(), ".claude-test-projects")
			os.MkdirAll(claudeProjectsDir, 0755)
			defer os.RemoveAll(claudeProjectsDir)

			// Create transcript file
			transcriptPath := filepath.Join(claudeProjectsDir, "test-session.jsonl")
			transcriptContent := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hi there!"}]}}`
			os.WriteFile(transcriptPath, []byte(transcriptContent), 0644)

			// Create active session file
			shiftlogDir := filepath.Join(repo.Path, ".shiftlog")
			os.MkdirAll(shiftlogDir, 0755)
			activeSession := map[string]string{
				"session_id":      "test-session-123",
				"transcript_path": transcriptPath,
				"started_at":      time.Now().UTC().Format(time.RFC3339),
				"project_path":    repo.Path,
			}
			sessionData, _ := json.MarshalIndent(activeSession, "", "  ")
			os.WriteFile(filepath.Join(shiftlogDir, "active-session.json"), sessionData, 0644)

			// Make a commit - the post-commit hook will run store --manual automatically
			repo.WriteFile("test.txt", "content")
			repo.Run("git", "add", "test.txt")
			repo.Run("git", "commit", "-m", "test commit")

			// Run manual store again - should report "already stored" since hook ran
			_, stderr, err := testutil.RunShiftlogInDir(repo.Path, "store", "--manual")
			Expect(err).NotTo(HaveOccurred())
			// Accept either "stored conversation" (first time) or "already stored" (hook already ran)
			Expect(stderr).To(Or(ContainSubstring("stored conversation"), ContainSubstring("already stored")))

			// Verify note was created (this is the key assertion)
			noteOutput, _ := repo.RunOutput("git", "notes", "--ref=refs/notes/shiftlog", "show", "HEAD")
			Expect(noteOutput).To(ContainSubstring("test-session-123"))
		})

		It("skips storage when same session already stored (idempotent)", func() {
			// Create transcript file
			transcriptPath := filepath.Join(os.TempDir(), "idempotent-test.jsonl")
			transcriptContent := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hi!"}]}}`
			os.WriteFile(transcriptPath, []byte(transcriptContent), 0644)
			defer os.Remove(transcriptPath)

			// Create active session file
			shiftlogDir := filepath.Join(repo.Path, ".shiftlog")
			os.MkdirAll(shiftlogDir, 0755)
			activeSession := map[string]string{
				"session_id":      "idempotent-session-456",
				"transcript_path": transcriptPath,
				"started_at":      time.Now().UTC().Format(time.RFC3339),
				"project_path":    repo.Path,
			}
			sessionData, _ := json.MarshalIndent(activeSession, "", "  ")
			os.WriteFile(filepath.Join(shiftlogDir, "active-session.json"), sessionData, 0644)

			// Make a commit - the post-commit hook will run store --manual automatically
			repo.WriteFile("test.txt", "content")
			repo.Run("git", "add", "test.txt")
			repo.Run("git", "commit", "-m", "test commit")

			// First explicit store - may be "stored" or "already stored" depending on hook execution
			_, stderr1, err := testutil.RunShiftlogInDir(repo.Path, "store", "--manual")
			Expect(err).NotTo(HaveOccurred())
			// Accept either message since hook may have already stored it
			Expect(stderr1).To(Or(ContainSubstring("stored conversation"), ContainSubstring("already stored")))

			// Second store with same session should definitely be idempotent
			_, stderr2, err := testutil.RunShiftlogInDir(repo.Path, "store", "--manual")
			Expect(err).NotTo(HaveOccurred())
			Expect(stderr2).To(ContainSubstring("already stored"))
		})

		It("overwrites when different session stored", func() {
			// Create first transcript file
			transcriptPath1 := filepath.Join(os.TempDir(), "overwrite-test1.jsonl")
			transcriptContent1 := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"first session"}]}}`
			os.WriteFile(transcriptPath1, []byte(transcriptContent1), 0644)
			defer os.Remove(transcriptPath1)

			// Create second transcript file
			transcriptPath2 := filepath.Join(os.TempDir(), "overwrite-test2.jsonl")
			transcriptContent2 := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"second session"}]}}`
			os.WriteFile(transcriptPath2, []byte(transcriptContent2), 0644)
			defer os.Remove(transcriptPath2)

			shiftlogDir := filepath.Join(repo.Path, ".shiftlog")
			os.MkdirAll(shiftlogDir, 0755)

			// First session
			activeSession1 := map[string]string{
				"session_id":      "first-session",
				"transcript_path": transcriptPath1,
				"started_at":      time.Now().UTC().Format(time.RFC3339),
				"project_path":    repo.Path,
			}
			sessionData1, _ := json.MarshalIndent(activeSession1, "", "  ")
			os.WriteFile(filepath.Join(shiftlogDir, "active-session.json"), sessionData1, 0644)

			// Make a commit
			repo.WriteFile("test.txt", "content")
			repo.Run("git", "add", "test.txt")
			repo.Run("git", "commit", "-m", "test commit")

			// First store
			_, _, err := testutil.RunShiftlogInDir(repo.Path, "store", "--manual")
			Expect(err).NotTo(HaveOccurred())

			// Verify first session stored
			noteOutput1, _ := repo.RunOutput("git", "notes", "--ref=refs/notes/shiftlog", "show", "HEAD")
			Expect(noteOutput1).To(ContainSubstring("first-session"))

			// Change to second session
			activeSession2 := map[string]string{
				"session_id":      "second-session",
				"transcript_path": transcriptPath2,
				"started_at":      time.Now().UTC().Format(time.RFC3339),
				"project_path":    repo.Path,
			}
			sessionData2, _ := json.MarshalIndent(activeSession2, "", "  ")
			os.WriteFile(filepath.Join(shiftlogDir, "active-session.json"), sessionData2, 0644)

			// Second store should overwrite
			_, stderr2, err := testutil.RunShiftlogInDir(repo.Path, "store", "--manual")
			Expect(err).NotTo(HaveOccurred())
			Expect(stderr2).To(ContainSubstring("stored conversation"))

			// Verify second session stored
			noteOutput2, _ := repo.RunOutput("git", "notes", "--ref=refs/notes/shiftlog", "show", "HEAD")
			Expect(noteOutput2).To(ContainSubstring("second-session"))
			Expect(noteOutput2).NotTo(ContainSubstring("first-session"))
		})

		It("skips when project path doesn't match", func() {
			// Create transcript file
			transcriptPath := filepath.Join(os.TempDir(), "wrong-project-test.jsonl")
			transcriptContent := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`
			os.WriteFile(transcriptPath, []byte(transcriptContent), 0644)
			defer os.Remove(transcriptPath)

			// Create active session file with different project path
			shiftlogDir := filepath.Join(repo.Path, ".shiftlog")
			os.MkdirAll(shiftlogDir, 0755)
			activeSession := map[string]string{
				"session_id":      "wrong-project-session",
				"transcript_path": transcriptPath,
				"started_at":      time.Now().UTC().Format(time.RFC3339),
				"project_path":    "/different/project/path",
			}
			sessionData, _ := json.MarshalIndent(activeSession, "", "  ")
			os.WriteFile(filepath.Join(shiftlogDir, "active-session.json"), sessionData, 0644)

			// Make a commit
			repo.WriteFile("test.txt", "content")
			repo.Run("git", "add", "test.txt")
			repo.Run("git", "commit", "-m", "test commit")

			// Run manual store - should skip silently
			stdout, stderr, err := testutil.RunShiftlogInDir(repo.Path, "store", "--manual")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(BeEmpty())
			Expect(stderr).NotTo(ContainSubstring("stored"))

			// Verify no note was created
			_, err = repo.RunOutput("git", "notes", "--ref=refs/notes/shiftlog", "show", "HEAD")
			Expect(err).To(HaveOccurred()) // Should fail because no note exists
		})
	})

	Describe("session-start command", func() {
		It("creates active session file", func() {
			// Prepare session start input
			input := map[string]string{
				"session_id":      "new-session-789",
				"transcript_path": "/home/user/.claude/projects/-test/new-session-789.jsonl",
				"cwd":             repo.Path,
			}
			inputData, _ := json.Marshal(input)

			// Run session-start with stdin
			_, stderr, err := testutil.RunShiftlogInDirWithStdin(repo.Path, string(inputData), "session-start")
			Expect(err).NotTo(HaveOccurred())
			Expect(stderr).To(ContainSubstring("session started"))

			// Verify session file was created
			sessionPath := filepath.Join(repo.Path, ".shiftlog", "active-session.json")
			Expect(sessionPath).To(BeAnExistingFile())

			// Verify content
			data, _ := os.ReadFile(sessionPath)
			var session map[string]string
			json.Unmarshal(data, &session)
			Expect(session["session_id"]).To(Equal("new-session-789"))
		})
	})

	Describe("session-end command", func() {
		It("removes active session file", func() {
			// Create active session file first
			shiftlogDir := filepath.Join(repo.Path, ".shiftlog")
			os.MkdirAll(shiftlogDir, 0755)
			sessionData := `{"session_id":"ending-session","transcript_path":"/tmp/test.jsonl","started_at":"2024-01-01T00:00:00Z","project_path":"/test"}`
			os.WriteFile(filepath.Join(shiftlogDir, "active-session.json"), []byte(sessionData), 0644)

			// Prepare session end input
			input := map[string]string{
				"session_id":      "ending-session",
				"transcript_path": "/tmp/test.jsonl",
				"cwd":             repo.Path,
				"reason":          "user_quit",
			}
			inputData, _ := json.Marshal(input)

			// Run session-end
			_, stderr, err := testutil.RunShiftlogInDirWithStdin(repo.Path, string(inputData), "session-end")
			Expect(err).NotTo(HaveOccurred())
			Expect(stderr).To(ContainSubstring("session ended"))

			// Verify session file was removed
			sessionPath := filepath.Join(repo.Path, ".shiftlog", "active-session.json")
			Expect(sessionPath).NotTo(BeAnExistingFile())
		})
	})
})
