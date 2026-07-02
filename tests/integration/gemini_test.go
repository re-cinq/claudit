package integration_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Gemini CLI Integration", func() {
	It("should create a git note when Gemini makes a commit", func() {
		skipIfEnvSet("SKIP_GEMINI_INTEGRATION")

		geminiAPIKey := os.Getenv("GEMINI_API_KEY")
		googleAPIKey := os.Getenv("GOOGLE_API_KEY")
		if geminiAPIKey == "" && googleAPIKey == "" {
			Skip("Neither GEMINI_API_KEY nor GOOGLE_API_KEY set")
		}

		requireBinary("gemini")
		shiftlogPath := getShiftlogPath()
		tmpDir := initGitRepo("gemini-integration")
		DeferCleanup(os.RemoveAll, tmpDir)

		// Initialize shiftlog with Gemini agent
		cmd := exec.Command(shiftlogPath, "init", "--agent=gemini")
		cmd.Dir = tmpDir
		output, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "shiftlog init --agent=gemini failed:\n%s", output)

		// Verify hooks
		settingsPath := filepath.Join(tmpDir, ".gemini", "settings.json")
		settingsData, err := os.ReadFile(settingsPath)
		Expect(err).NotTo(HaveOccurred(), "Failed to read settings")

		var settings map[string]interface{}
		Expect(json.Unmarshal(settingsData, &settings)).To(Succeed())

		hooks, ok := settings["hooks"].(map[string]interface{})
		Expect(ok).To(BeTrue(), "Expected hooks object in settings")
		afterTool, ok := hooks["AfterTool"].([]interface{})
		Expect(ok).To(BeTrue(), "Expected AfterTool array in hooks")
		Expect(afterTool).NotTo(BeEmpty())

		By("Hook configuration verified successfully")

		// Create a test file
		Expect(os.WriteFile(filepath.Join(tmpDir, "todo.txt"), []byte("- Buy milk\n- Walk dog\n"), 0644)).To(Succeed())

		// Run Gemini CLI
		geminiCmd := exec.Command("gemini",
			"-p", "Please run: git add todo.txt && git commit -m 'Add todo list'",
			"--approval-mode", "yolo",
		)
		geminiCmd.Dir = tmpDir
		geminiCmd.Env = append(os.Environ(),
			"PATH="+filepath.Dir(shiftlogPath)+":"+os.Getenv("PATH"),
		)
		if geminiAPIKey != "" {
			geminiCmd.Env = append(geminiCmd.Env, "GEMINI_API_KEY="+geminiAPIKey)
		}
		if googleAPIKey != "" {
			geminiCmd.Env = append(geminiCmd.Env, "GOOGLE_API_KEY="+googleAPIKey)
		}

		geminiOutput := runAgentWithTimeout(geminiCmd, 90*time.Second)

		time.Sleep(2 * time.Second)

		Expect(string(geminiOutput)).NotTo(ContainSubstring("shiftlog: warning:"),
			"shiftlog logged warnings during execution:\n%s", geminiOutput)

		// Check if commit was made
		cmd = exec.Command("git", "log", "--oneline", "-n", "2")
		cmd.Dir = tmpDir
		logOutput, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "Failed to check git log:\n%s", logOutput)

		if !strings.Contains(string(logOutput), "todo") {
			Skip("Gemini did not make the commit - cannot test note storage")
		}

		By("Commit was created successfully")

		// Verify note
		cmd = exec.Command("git", "notes", "--ref=refs/notes/shiftlog", "list")
		cmd.Dir = tmpDir
		notesOutput, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "Commit was made but no git notes exist!\nOutput: %s", notesOutput)
		Expect(strings.TrimSpace(string(notesOutput))).NotTo(BeEmpty())

		cmd = exec.Command("git", "notes", "--ref=refs/notes/shiftlog", "show", "HEAD")
		cmd.Dir = tmpDir
		noteContent, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred())

		var noteData map[string]interface{}
		Expect(json.Unmarshal(noteContent, &noteData)).To(Succeed())

		requiredFields := []string{"version", "session_id", "project_path", "git_branch", "message_count", "checksum", "transcript", "timestamp"}
		for _, field := range requiredFields {
			Expect(noteData).To(HaveKey(field))
		}
		Expect(noteData["agent"]).To(Equal("gemini"))

		By("Note content is valid and contains all required fields")
	})
})
