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

var _ = Describe("Copilot CLI Integration", func() {
	It("should create a git note when Copilot makes a commit", func() {
		skipIfEnvSet("SKIP_COPILOT_INTEGRATION")
		githubToken := requireEnvVar("COPILOT_GITHUB_TOKEN")
		requireBinary("copilot")
		shiftlogPath := getShiftlogPath()
		tmpDir := initGitRepo("copilot-integration")
		DeferCleanup(os.RemoveAll, tmpDir)

		// Initialize shiftlog with Copilot agent
		cmd := exec.Command(shiftlogPath, "init", "--agent=copilot")
		cmd.Dir = tmpDir
		output, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "shiftlog init --agent=copilot failed:\n%s", output)

		// Verify hooks at .github/hooks/shiftlog.json
		hooksPath := filepath.Join(tmpDir, ".github", "hooks", "shiftlog.json")
		hooksData, err := os.ReadFile(hooksPath)
		Expect(err).NotTo(HaveOccurred(), "Failed to read hooks file")

		var hooksFile map[string]interface{}
		Expect(json.Unmarshal(hooksData, &hooksFile)).To(Succeed())

		hooks, ok := hooksFile["hooks"].(map[string]interface{})
		Expect(ok).To(BeTrue(), "Expected hooks object in shiftlog.json")
		postToolUse, ok := hooks["postToolUse"].([]interface{})
		Expect(ok).To(BeTrue(), "Expected postToolUse array in hooks")
		Expect(postToolUse).NotTo(BeEmpty())

		hookEntry, ok := postToolUse[0].(map[string]interface{})
		Expect(ok).To(BeTrue(), "Expected hook entry to be an object")
		Expect(hookEntry["type"]).To(Equal("command"))
		Expect(hookEntry["command"].(string)).To(ContainSubstring("shiftlog store"))

		By("Hook configuration verified successfully")

		// Create a test file
		Expect(os.WriteFile(filepath.Join(tmpDir, "todo.txt"), []byte("- Buy milk\n- Walk dog\n"), 0644)).To(Succeed())

		// Run Copilot CLI
		copilotCmd := exec.Command("copilot",
			"-p", "Run this exact command: git add todo.txt && git commit -m 'Add todo list'",
			"--yolo",
		)
		copilotCmd.Dir = tmpDir
		copilotCmd.Env = append(os.Environ(),
			"PATH="+filepath.Dir(shiftlogPath)+":"+os.Getenv("PATH"),
			"COPILOT_GITHUB_TOKEN="+githubToken,
		)

		copilotOutput := runAgentWithTimeout(copilotCmd, 90*time.Second)

		time.Sleep(2 * time.Second)

		Expect(string(copilotOutput)).NotTo(ContainSubstring("shiftlog: warning:"),
			"shiftlog logged warnings during execution:\n%s", copilotOutput)

		// Check if commit was made
		cmd = exec.Command("git", "log", "--oneline", "-n", "2")
		cmd.Dir = tmpDir
		logOutput, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "Failed to check git log:\n%s", logOutput)

		if !strings.Contains(string(logOutput), "todo") {
			Skip("Copilot did not make the commit - cannot test note storage")
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
		Expect(noteData["agent"]).To(Equal("copilot"))

		By("Note content is valid and contains all required fields")
	})
})
