package integration_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// getWorkspaceRoot finds the workspace root by looking for go.mod.
func getWorkspaceRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "."
}

// getShiftlogPath returns the path to the shiftlog binary, failing the spec if not found.
func getShiftlogPath() string {
	GinkgoHelper()
	shiftlogPath := os.Getenv("SHIFTLOG_BINARY")
	if shiftlogPath == "" {
		shiftlogPath = filepath.Join(getWorkspaceRoot(), "shiftlog")
	}
	_, err := os.Stat(shiftlogPath)
	Expect(err).NotTo(HaveOccurred(), "shiftlog binary not found at %s - run 'go build'", shiftlogPath)
	return shiftlogPath
}

// runGit runs a git command in the specified directory, failing the spec on error.
func runGit(dir string, args ...string) {
	GinkgoHelper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "git %v failed:\n%s", args, output)
}

// runAgentWithTimeout runs a command with a timeout, returning its combined output.
// On timeout it kills the process and fails the spec.
func runAgentWithTimeout(cmd *exec.Cmd, timeout time.Duration) []byte {
	GinkgoHelper()
	type result struct {
		output []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		output, err := cmd.CombinedOutput()
		done <- result{output, err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			GinkgoWriter.Printf("Command finished with error (may be expected): %v\n", res.err)
		}
		return res.output
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		Fail(fmt.Sprintf("Command timed out after %v", timeout))
		return nil
	}
}

// skipIfEnvSet skips the spec if the given environment variable is set to "1".
func skipIfEnvSet(envVar string) {
	GinkgoHelper()
	if os.Getenv(envVar) == "1" {
		Skip(fmt.Sprintf("%s=1 is set", envVar))
	}
}

// requireEnvVar skips the spec if none of the given environment variables are set.
// Returns the first non-empty value.
func requireEnvVar(vars ...string) string {
	GinkgoHelper()
	for _, v := range vars {
		if val := os.Getenv(v); val != "" {
			return val
		}
	}
	Skip(fmt.Sprintf("None of %v set", vars))
	return ""
}

// requireBinary skips the spec if the binary is not found in PATH.
func requireBinary(name string) {
	GinkgoHelper()
	if _, err := exec.LookPath(name); err != nil {
		Skip(fmt.Sprintf("%s CLI not found in PATH", name))
	}
}

// initGitRepo creates a temp dir with an initialized git repo and initial commit.
// Returns the tmpDir path. Caller should DeferCleanup(os.RemoveAll, tmpDir).
func initGitRepo(prefix string) string {
	GinkgoHelper()
	tmpDir, err := os.MkdirTemp("", prefix+"-*")
	Expect(err).NotTo(HaveOccurred())

	runGit(tmpDir, "init")
	runGit(tmpDir, "config", "user.email", "test@example.com")
	runGit(tmpDir, "config", "user.name", "Test User")

	Expect(os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# Test Project\n"), 0644)).To(Succeed())
	runGit(tmpDir, "add", "README.md")
	runGit(tmpDir, "commit", "-m", "Initial commit")

	return tmpDir
}

// verifyNoteOnHead checks that the HEAD commit has a valid shiftlog git note
// with all required fields and the expected agent value.
func verifyNoteOnHead(tmpDir, expectedAgent string) {
	GinkgoHelper()

	cmd := exec.Command("git", "notes", "--ref=refs/notes/shiftlog", "list")
	cmd.Dir = tmpDir
	notesOutput, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "Commit was made but no git notes exist!\nOutput: %s", notesOutput)
	Expect(strings.TrimSpace(string(notesOutput))).NotTo(BeEmpty(), "Commit was made but git notes list is empty!")

	By("Git note was created by shiftlog hook")

	cmd = exec.Command("git", "notes", "--ref=refs/notes/shiftlog", "show", "HEAD")
	cmd.Dir = tmpDir
	noteContent, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "Note exists but cannot be read")

	var noteData map[string]interface{}
	Expect(json.Unmarshal(noteContent, &noteData)).To(Succeed(), "Note content is not valid JSON")

	requiredFields := []string{"version", "session_id", "project_path", "git_branch", "message_count", "checksum", "transcript", "timestamp"}
	for _, field := range requiredFields {
		Expect(noteData).To(HaveKey(field), "Note missing required field '%s'", field)
	}

	Expect(noteData["agent"]).To(Equal(expectedAgent))

	By("Note content is valid and contains all required fields")
	GinkgoWriter.Printf("Note preview: version=%v, session_id=%v, agent=%v, message_count=%v\n",
		noteData["version"], noteData["session_id"], noteData["agent"], noteData["message_count"])
}

// setupManualCommitRepo creates a temp git repo, initializes shiftlog with the
// given agent, and returns the tmpDir and shiftlogPath.
func setupManualCommitRepo(agentName string) (tmpDir, shiftlogPath string) {
	GinkgoHelper()

	shiftlogPath = getShiftlogPath()
	tmpDir = initGitRepo(agentName + "-manual-commit")

	initArgs := []string{"init"}
	if agentName != "claude" {
		initArgs = append(initArgs, "--agent="+agentName)
	}
	cmd := exec.Command(shiftlogPath, initArgs...)
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "shiftlog init failed:\n%s", output)

	return tmpDir, shiftlogPath
}

// manualCommitNewFile creates a new file and commits it manually.
func manualCommitNewFile(tmpDir string) {
	GinkgoHelper()

	manualFile := filepath.Join(tmpDir, "manual-file.txt")
	Expect(os.WriteFile(manualFile, []byte("manually created file\n"), 0644)).To(Succeed())
	runGit(tmpDir, "add", "manual-file.txt")
	runGit(tmpDir, "commit", "-m", "Manual commit after agent session")
}

// captureEvent represents a parsed hook capture event.
type captureEvent struct {
	Event          string
	InputSessionID string
	InputCallID    string
	InputTool      string
	InputKeys      []string
	OutputKeys     []string
	HasArgs        bool
	ArgsKeys       []string
	Command        string
}

// captureEvents holds parsed before/after events.
type captureEvents struct {
	Before []captureEvent
	After  []captureEvent
}

// stringField extracts a string value from a map.
func stringField(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}

// toStringSlice converts an interface{} (expected []interface{}) to []string.
func toStringSlice(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	var result []string
	for _, item := range arr {
		s, _ := item.(string)
		result = append(result, s)
	}
	return result
}

// mapKeys returns the keys of a map.
func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// readCaptureEvents reads and parses the hook capture file.
func readCaptureEvents(captureFilePath string) captureEvents {
	GinkgoHelper()

	f, err := os.Open(captureFilePath)
	if err != nil {
		Skip("Could not open capture file: " + err.Error())
		return captureEvents{}
	}
	defer f.Close()

	var events captureEvents
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)
	for scanner.Scan() {
		var raw map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
			continue
		}

		evt := captureEvent{
			Event:          stringField(raw, "event"),
			InputSessionID: stringField(raw, "input_sessionID"),
			InputCallID:    stringField(raw, "input_callID"),
			InputTool:      stringField(raw, "input_tool"),
			Command:        stringField(raw, "command"),
		}

		if v, ok := raw["has_args"]; ok {
			evt.HasArgs, _ = v.(bool)
		}
		if v, ok := raw["input_keys"]; ok {
			evt.InputKeys = toStringSlice(v)
		}
		if v, ok := raw["output_keys"]; ok {
			evt.OutputKeys = toStringSlice(v)
		}
		if v, ok := raw["args_keys"]; ok {
			evt.ArgsKeys = toStringSlice(v)
		}

		switch evt.Event {
		case "before":
			events.Before = append(events.Before, evt)
		case "after":
			events.After = append(events.After, evt)
		}
	}

	return events
}

// capturePluginJS is a modified version of the shiftlog plugin that also
// captures the raw data OpenCode provides to plugin hooks for validation.
// It reads the capture file path from CLAUDIT_HOOK_CAPTURE_FILE env var.
const capturePluginJS = `// Capture plugin for shiftlog integration testing
// Logs raw hook data to validate OpenCode's plugin API
export const ShiftlogPlugin = async ({ directory, client }) => {
  const fs = await import("fs");
  const captureFile = process.env.CLAUDIT_HOOK_CAPTURE_FILE;
  const pendingCommits = new Map();

  const capture = (data) => {
    if (!captureFile) return;
    try { fs.appendFileSync(captureFile, JSON.stringify(data) + "\n"); } catch(e) {}
  };

  return {
    "tool.execute.before": async (input, output) => {
      capture({
        event: "before",
        input_keys: Object.keys(input || {}),
        input_sessionID: input?.sessionID || "",
        input_callID: input?.callID || "",
        input_tool: input?.tool || "",
        output_keys: Object.keys(output || {}),
        has_args: !!(output?.args),
        args_keys: output?.args ? Object.keys(output.args) : [],
        command: output?.args?.command || output?.args?.cmd || "",
      });

      const command = output?.args?.command || output?.args?.cmd || "";
      if (command.includes("git commit") || command.includes("git-commit")) {
        pendingCommits.set(input.callID, {
          command,
          tool: input.tool,
          sessionID: input.sessionID,
        });
      }
    },

    "tool.execute.after": async (input, output) => {
      capture({
        event: "after",
        input_keys: Object.keys(input || {}),
        input_sessionID: input?.sessionID || "",
        input_callID: input?.callID || "",
        input_tool: input?.tool || "",
        output_keys: Object.keys(output || {}),
      });

      const pending = pendingCommits.get(input.callID);
      if (!pending) return;
      pendingCommits.delete(input.callID);

      let transcriptData = "";
      if (client && pending.sessionID) {
        try {
          const msgs = await client.session.messages({ path: { id: pending.sessionID } });
          if (msgs && Array.isArray(msgs)) {
            transcriptData = JSON.stringify(msgs.map(m => ({
              role: m.role || "",
              id: m.id || "",
              content: m.content || "",
              time: m.time || {},
            })));
          }
        } catch (e) {}
      }

      const dataDir = process.platform === "darwin"
          ? process.env.HOME + "/Library/Application Support/opencode"
          : (process.env.XDG_DATA_HOME || process.env.HOME + "/.local/share") + "/opencode";

      const hookData = JSON.stringify({
        session_id: pending.sessionID || "",
        data_dir: dataDir,
        project_dir: directory,
        tool_name: pending.tool || "",
        tool_input: { command: pending.command },
        ...(transcriptData ? { transcript_data: transcriptData } : {}),
      });

      try {
        const { execSync } = await import("child_process");
        execSync("shiftlog store --agent=opencode", {
          input: hookData,
          cwd: directory,
          timeout: 30000,
          stdio: ["pipe", "pipe", "pipe"],
        });
      } catch (e) {}
    },
  };
};
`
