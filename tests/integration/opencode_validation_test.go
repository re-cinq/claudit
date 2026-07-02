package integration_test

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("OpenCode Validation", func() {
	Describe("CLI flag validation", func() {
		It("should have --session resume flag", func() {
			skipIfEnvSet("SKIP_OPENCODE_INTEGRATION")
			requireBinary("opencode")

			cmd := exec.Command("opencode", "--help")
			output, _ := cmd.CombinedOutput()
			helpText := string(output)

			Expect(helpText).To(ContainSubstring("--session"),
				"opencode --help does not list '--session' flag.\nHelp output:\n%s", helpText)

			By("Confirmed: --session flag exists in opencode CLI")

			if !strings.Contains(helpText, "-s,") && !strings.Contains(helpText, "-s ") {
				GinkgoWriter.Println("Note: -s shorthand not found in help (optional)")
			}

			Expect(helpText).To(ContainSubstring("run"),
				"opencode --help does not list 'run' subcommand")
		})

		It("should have session list subcommand", func() {
			skipIfEnvSet("SKIP_OPENCODE_INTEGRATION")
			requireBinary("opencode")

			cmd := exec.Command("opencode", "session", "--help")
			output, _ := cmd.CombinedOutput()
			helpText := string(output)

			Expect(helpText).To(ContainSubstring("list"),
				"opencode session --help does not list 'list' subcommand.\nHelp output:\n%s", helpText)

			By("Confirmed: 'opencode session list' subcommand exists")
		})
	})

	Describe("deep integration validation", Ordered, func() {
		var (
			tmpDir            string
			shiftlogPath       string
			xdgDataHome       string
			dataDir           string
			captureFile       string
			expectedProjectID string
		)

		BeforeAll(func() {
			skipIfEnvSet("SKIP_OPENCODE_INTEGRATION")

			geminiAPIKey := os.Getenv("GEMINI_API_KEY")
			googleGenAIKey := os.Getenv("GOOGLE_GENERATIVE_AI_API_KEY")
			if geminiAPIKey == "" && googleGenAIKey == "" {
				Skip("Neither GEMINI_API_KEY nor GOOGLE_GENERATIVE_AI_API_KEY set")
			}
			apiKey := googleGenAIKey
			if apiKey == "" {
				apiKey = geminiAPIKey
			}

			requireBinary("opencode")

			shiftlogPath = getShiftlogPath()

			var err error
			tmpDir, err = os.MkdirTemp("", "opencode-validation-*")
			Expect(err).NotTo(HaveOccurred())

			xdgDataHome = filepath.Join(tmpDir, "xdg-data")
			dataDir = filepath.Join(xdgDataHome, "opencode")
			captureFile = filepath.Join(tmpDir, "hook-capture.jsonl")

			// Initialize git repo
			runGit(tmpDir, "init")
			runGit(tmpDir, "config", "user.email", "test@example.com")
			runGit(tmpDir, "config", "user.name", "Test User")

			Expect(os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# Test Project\n"), 0644)).To(Succeed())
			runGit(tmpDir, "add", "README.md")
			runGit(tmpDir, "commit", "-m", "Initial commit")

			// Get expected project ID (root commit hash)
			cmd := exec.Command("git", "rev-list", "--max-parents=0", "--all")
			cmd.Dir = tmpDir
			rootCommitOutput, err := cmd.Output()
			Expect(err).NotTo(HaveOccurred(), "Failed to get root commit")
			expectedProjectID = strings.TrimSpace(string(rootCommitOutput))
			GinkgoWriter.Printf("Expected project ID (root commit): %s\n", expectedProjectID[:12])

			// Write opencode.json
			opencodeConfig := map[string]interface{}{
				"$schema":    "https://opencode.ai/config.json",
				"model":      "google/gemini-2.5-flash",
				"permission": "allow",
			}
			configData, _ := json.MarshalIndent(opencodeConfig, "", "  ")
			Expect(os.WriteFile(filepath.Join(tmpDir, "opencode.json"), configData, 0644)).To(Succeed())

			// Initialize shiftlog
			initCmd := exec.Command(shiftlogPath, "init", "--agent=opencode")
			initCmd.Dir = tmpDir
			output, err := initCmd.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), "shiftlog init failed:\n%s", output)

			// Overwrite plugin with capture version
			pluginPath := filepath.Join(tmpDir, ".opencode", "plugins", "shiftlog.js")
			Expect(os.WriteFile(pluginPath, []byte(capturePluginJS), 0644)).To(Succeed())

			// Create a file for OpenCode to commit
			Expect(os.WriteFile(filepath.Join(tmpDir, "todo.txt"), []byte("- Buy milk\n- Walk dog\n"), 0644)).To(Succeed())

			// Run OpenCode
			opencodeCmd := exec.Command("opencode", "run",
				"Please run: git add todo.txt && git commit -m 'Add todo list'",
			)
			opencodeCmd.Dir = tmpDir
			opencodeCmd.Env = append(os.Environ(),
				"PATH="+filepath.Dir(shiftlogPath)+":"+os.Getenv("PATH"),
				"GOOGLE_GENERATIVE_AI_API_KEY="+apiKey,
				"XDG_DATA_HOME="+xdgDataHome,
				"CLAUDIT_HOOK_CAPTURE_FILE="+captureFile,
			)

			opencodeOutput := runAgentWithTimeout(opencodeCmd, 90*time.Second)

			time.Sleep(2 * time.Second)

			// Check if commit was made (prerequisite for all sub-tests)
			cmd = exec.Command("git", "log", "--oneline", "-n", "2")
			cmd.Dir = tmpDir
			logOutput, _ := cmd.CombinedOutput()
			if !strings.Contains(string(logOutput), "todo") {
				GinkgoWriter.Printf("OpenCode output:\n%s\n", opencodeOutput)
				Skip("OpenCode did not make the commit - cannot validate integration")
			}
			By("Commit was created successfully")
		})

		AfterAll(func() {
			if tmpDir != "" {
				os.RemoveAll(tmpDir)
			}
		})

		It("should invoke plugin hook API (before/after events)", func() {
			Expect(captureFile).To(BeAnExistingFile(),
				"Hook capture file was not created.\n"+
					"This means OpenCode did NOT call our plugin hooks.\n"+
					"Possible causes:\n"+
					"  - Plugin file format/export pattern is wrong\n"+
					"  - Hook names (tool.execute.before/after) are not recognized\n"+
					"  - Plugin directory (.opencode/plugins/) is not scanned")

			f, err := os.Open(captureFile)
			Expect(err).NotTo(HaveOccurred())
			defer f.Close()

			var beforeEvents, afterEvents []map[string]interface{}
			scanner := bufio.NewScanner(f)
			scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)
			for scanner.Scan() {
				var entry map[string]interface{}
				if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
					continue
				}
				switch entry["event"] {
				case "before":
					beforeEvents = append(beforeEvents, entry)
				case "after":
					afterEvents = append(afterEvents, entry)
				}
			}

			Expect(beforeEvents).NotTo(BeEmpty(),
				"No 'tool.execute.before' events captured.")
			Expect(afterEvents).NotTo(BeEmpty(),
				"No 'tool.execute.after' events captured.")

			GinkgoWriter.Printf("Captured %d before events and %d after events\n", len(beforeEvents), len(afterEvents))
		})

		It("should provide correct hook input schema", func() {
			events := readCaptureEvents(captureFile)
			if len(events.Before) == 0 {
				Skip("No before events to validate")
			}

			for i, evt := range events.Before {
				Expect(evt.InputSessionID).NotTo(BeEmpty(),
					"before[%d]: input.sessionID is empty — ParseHookInput needs this", i)
				Expect(evt.InputCallID).NotTo(BeEmpty(),
					"before[%d]: input.callID is empty — needed for correlation", i)
				Expect(evt.InputTool).NotTo(BeEmpty(),
					"before[%d]: input.tool is empty — IsCommitCommand needs this", i)
			}

			hasArgs := false
			for _, evt := range events.Before {
				if evt.HasArgs {
					hasArgs = true
					GinkgoWriter.Printf("Confirmed: tool=%q has args, command=%q\n", evt.InputTool, evt.Command)
					break
				}
			}
			Expect(hasArgs).To(BeTrue(),
				"No 'before' event had output.args — we extract command from output.args.command")
		})

		It("should capture git commands in hook output schema", func() {
			events := readCaptureEvents(captureFile)

			hasCommitCommand := false
			for _, evt := range events.Before {
				if strings.Contains(evt.Command, "git commit") || strings.Contains(evt.Command, "git add") {
					hasCommitCommand = true
					GinkgoWriter.Printf("Confirmed: captured git command: %q\n", evt.Command)
					break
				}
			}
			Expect(hasCommitCommand).To(BeTrue(),
				"No captured event has 'git commit' in output.args.command")

			// Verify before/after callID correlation
			beforeCallIDs := make(map[string]bool)
			for _, evt := range events.Before {
				if evt.InputCallID != "" {
					beforeCallIDs[evt.InputCallID] = true
				}
			}
			matchedAfter := 0
			for _, evt := range events.After {
				if beforeCallIDs[evt.InputCallID] {
					matchedAfter++
				}
			}
			if len(events.After) > 0 {
				Expect(matchedAfter).To(BeNumerically(">", 0),
					"No 'after' event callIDs matched 'before' events")
				GinkgoWriter.Printf("Confirmed: %d after events correlated via callID\n", matchedAfter)
			}
		})

		It("should respect XDG_DATA_HOME for data directory", func() {
			Expect(dataDir).To(BeADirectory(),
				"Custom data dir not created. OpenCode may not respect XDG_DATA_HOME.")

			// OpenCode v1.2+ uses SQLite (opencode.db) instead of flat file dirs.
			// Accept either flat dirs or SQLite database.
			hasFlatDirs := true
			for _, subdir := range []string{"storage", "storage/session", "storage/message"} {
				path := filepath.Join(dataDir, subdir)
				if _, err := os.Stat(path); os.IsNotExist(err) {
					hasFlatDirs = false
					break
				}
			}

			hasSQLite := false
			dbPath := filepath.Join(dataDir, "opencode.db")
			if _, err := os.Stat(dbPath); err == nil {
				hasSQLite = true
			}

			Expect(hasFlatDirs || hasSQLite).To(BeTrue(),
				"Neither flat storage dirs nor opencode.db found in data dir %s", dataDir)

			if hasSQLite {
				GinkgoWriter.Printf("Confirmed: XDG_DATA_HOME respected at %s (SQLite mode)\n", dataDir)
			} else {
				GinkgoWriter.Printf("Confirmed: XDG_DATA_HOME respected at %s (flat file mode)\n", dataDir)
			}
		})

		It("should use root commit hash as project ID", func() {
			// Try flat file storage first
			sessionDir := filepath.Join(dataDir, "storage", "session")
			projectSessionDir := filepath.Join(sessionDir, expectedProjectID)
			if _, err := os.Stat(projectSessionDir); err == nil {
				// Flat file mode: verify session file
				GinkgoWriter.Printf("Confirmed: session stored under root commit hash %s (flat files)\n", expectedProjectID[:12])

				sessionEntries, err := os.ReadDir(projectSessionDir)
				Expect(err).NotTo(HaveOccurred())

				for _, e := range sessionEntries {
					if !strings.HasSuffix(e.Name(), ".json") {
						continue
					}
					data, err := os.ReadFile(filepath.Join(projectSessionDir, e.Name()))
					if err != nil {
						continue
					}
					var session map[string]interface{}
					if err := json.Unmarshal(data, &session); err != nil {
						continue
					}
					if pid, ok := session["projectID"]; ok {
						pidStr, _ := pid.(string)
						Expect(pidStr).To(Equal(expectedProjectID))
						GinkgoWriter.Println("Confirmed: session file projectID matches root commit")
					}
					break
				}
				return
			}

			// SQLite mode: query the database
			dbPath := filepath.Join(dataDir, "opencode.db")
			if _, err := os.Stat(dbPath); os.IsNotExist(err) {
				Fail("Neither flat session dir nor opencode.db found.\n" +
					"Expected project ID: " + expectedProjectID[:12])
			}

			cmd := exec.Command("sqlite3", dbPath,
				"SELECT project_id FROM session LIMIT 1;")
			output, err := cmd.Output()
			Expect(err).NotTo(HaveOccurred(), "Failed to query session from SQLite")

			foundProjectID := strings.TrimSpace(string(output))
			Expect(foundProjectID).To(Equal(expectedProjectID),
				"SQLite session project_id does not match root commit hash")

			GinkgoWriter.Printf("Confirmed: session stored with project_id=%s (SQLite)\n", expectedProjectID[:12])
		})

		It("should store sessions at expected paths", func() {
			// Try flat file storage first
			projectSessionDir := filepath.Join(dataDir, "storage", "session", expectedProjectID)
			entries, err := os.ReadDir(projectSessionDir)
			if err == nil {
				// Flat file mode
				for _, e := range entries {
					if !strings.HasSuffix(e.Name(), ".json") {
						continue
					}
					data, err := os.ReadFile(filepath.Join(projectSessionDir, e.Name()))
					if err != nil {
						continue
					}

					var session map[string]interface{}
					Expect(json.Unmarshal(data, &session)).To(Succeed(),
						"Session file %s is not valid JSON", e.Name())

					Expect(session).To(HaveKey("id"),
						"Session file missing 'id' field (have: %v)", mapKeys(session))

					GinkgoWriter.Printf("Session file %s fields: %v\n", e.Name(), mapKeys(session))

					sessionID, _ := session["id"].(string)
					expectedName := sessionID + ".json"
					if e.Name() != expectedName {
						GinkgoWriter.Printf("Note: session filename %q != id+.json %q\n", e.Name(), expectedName)
					}
					break
				}
				return
			}

			// SQLite mode: verify session exists with required fields
			dbPath := filepath.Join(dataDir, "opencode.db")
			if _, err := os.Stat(dbPath); os.IsNotExist(err) {
				Skip("Neither flat session dir nor opencode.db found")
			}

			cmd := exec.Command("sqlite3", "-json", dbPath,
				"SELECT id, project_id FROM session LIMIT 1;")
			output, err := cmd.Output()
			Expect(err).NotTo(HaveOccurred(), "Failed to query sessions from SQLite")

			var sessions []map[string]interface{}
			Expect(json.Unmarshal(output, &sessions)).To(Succeed(),
				"SQLite session query did not return valid JSON")
			Expect(sessions).NotTo(BeEmpty(), "No sessions found in SQLite database")

			session := sessions[0]
			Expect(session).To(HaveKey("id"), "Session missing 'id' field")
			GinkgoWriter.Printf("SQLite session fields: %v\n", mapKeys(session))
		})

		It("should store messages at expected paths", func() {
			// Try flat file storage first
			msgBaseDir := filepath.Join(dataDir, "storage", "message")
			entries, err := os.ReadDir(msgBaseDir)
			if err == nil && len(entries) > 0 {
				// Flat file mode
				for _, e := range entries {
					if !e.IsDir() {
						continue
					}
					sessionMsgDir := filepath.Join(msgBaseDir, e.Name())
					msgEntries, err := os.ReadDir(sessionMsgDir)
					if err != nil {
						continue
					}
					GinkgoWriter.Printf("Session %s: %d message files\n", e.Name(), len(msgEntries))

					for _, me := range msgEntries {
						if !strings.HasSuffix(me.Name(), ".json") {
							continue
						}
						data, err := os.ReadFile(filepath.Join(sessionMsgDir, me.Name()))
						if err != nil {
							continue
						}
						var msg map[string]interface{}
						Expect(json.Unmarshal(data, &msg)).To(Succeed(),
							"Message %s is not valid JSON", me.Name())

						hasRole := msg["role"] != nil
						hasType := msg["type"] != nil
						Expect(hasRole || hasType).To(BeTrue(),
							"Message %s has neither 'role' nor 'type' field (keys: %v)",
							me.Name(), mapKeys(msg))

						GinkgoWriter.Printf("Message format fields: %v\n", mapKeys(msg))
						break
					}
					break
				}
				return
			}

			// SQLite mode: verify messages exist
			dbPath := filepath.Join(dataDir, "opencode.db")
			if _, err := os.Stat(dbPath); os.IsNotExist(err) {
				Skip("Neither flat message dirs nor opencode.db found")
			}

			cmd := exec.Command("sqlite3", dbPath,
				"SELECT COUNT(*) FROM message;")
			output, err := cmd.Output()
			Expect(err).NotTo(HaveOccurred(), "Failed to query messages from SQLite")

			countStr := strings.TrimSpace(string(output))
			GinkgoWriter.Printf("SQLite message count: %s\n", countStr)
			Expect(countStr).NotTo(Equal("0"), "No messages found in SQLite database")

			// Verify message data has expected structure
			cmd = exec.Command("sqlite3", dbPath,
				"SELECT data FROM message LIMIT 1;")
			output, err = cmd.Output()
			Expect(err).NotTo(HaveOccurred(), "Failed to read message data from SQLite")

			var msg map[string]interface{}
			Expect(json.Unmarshal(output, &msg)).To(Succeed(),
				"Message data is not valid JSON")

			hasRole := msg["role"] != nil
			hasType := msg["type"] != nil
			Expect(hasRole || hasType).To(BeTrue(),
				"Message has neither 'role' nor 'type' field (keys: %v)", mapKeys(msg))

			GinkgoWriter.Printf("SQLite message format fields: %v\n", mapKeys(msg))
		})
	})
})

