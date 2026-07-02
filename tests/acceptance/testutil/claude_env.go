package testutil

import (
	"os"
	"path/filepath"
)

// AgentEnv represents an isolated test environment with a temporary HOME directory.
// It works for any agent by using the config's GetSessionDir and SessionFileExt.
type AgentEnv struct {
	// TempHome is the temporary HOME directory for isolation
	TempHome string
	// OrigHome is the original HOME to restore after tests
	OrigHome string
	// Config holds agent-specific settings for session path computation
	Config AgentTestConfig
}

// NewAgentEnv creates a new isolated environment for testing with the given agent config.
func NewAgentEnv(config AgentTestConfig) (*AgentEnv, error) {
	tempHome, err := os.MkdirTemp("", "shiftlog-test-home-*")
	if err != nil {
		return nil, err
	}

	env := &AgentEnv{
		TempHome: tempHome,
		OrigHome: os.Getenv("HOME"),
		Config:   config,
	}

	return env, nil
}

// Cleanup removes the temporary HOME directory and restores the original HOME
func (e *AgentEnv) Cleanup() {
	_ = os.Setenv("HOME", e.OrigHome)
	_ = os.RemoveAll(e.TempHome)
}

// Apply sets the HOME environment variable to the temporary directory
func (e *AgentEnv) Apply() {
	_ = os.Setenv("HOME", e.TempHome)
}

// GetEnvVars returns environment variables for running commands with isolated HOME
func (e *AgentEnv) GetEnvVars() []string {
	return []string{"HOME=" + e.TempHome}
}

// GetProjectDir returns the path to a specific project's session directory,
// delegating to the agent config's GetSessionDir function.
func (e *AgentEnv) GetProjectDir(projectPath string) string {
	// Resolve symlinks to match production behavior (git rev-parse resolves symlinks)
	// This is critical on macOS where /tmp is a symlink to /private/tmp
	resolved, err := filepath.EvalSymlinks(projectPath)
	if err == nil {
		projectPath = resolved
	}
	return e.Config.GetSessionDir(e.TempHome, projectPath)
}

// SetupProjectDir creates the directory structure for a project
func (e *AgentEnv) SetupProjectDir(projectPath string) error {
	projectDir := e.GetProjectDir(projectPath)
	return os.MkdirAll(projectDir, 0700)
}

// WriteSessionFile writes a session file for testing using the agent's file extension.
func (e *AgentEnv) WriteSessionFile(projectPath, sessionID string, content []byte) (string, error) {
	projectDir := e.GetProjectDir(projectPath)
	if err := os.MkdirAll(projectDir, 0700); err != nil {
		return "", err
	}

	sessionPath := filepath.Join(projectDir, sessionID+e.Config.SessionFileExt)
	if err := os.WriteFile(sessionPath, content, 0600); err != nil {
		return "", err
	}

	return sessionPath, nil
}

// SessionFileExists checks if a session file exists.
// For agents with custom ReadRestoredTranscript, it uses that to check existence.
func (e *AgentEnv) SessionFileExists(projectPath, sessionID string) bool {
	if e.Config.ReadRestoredTranscript != nil {
		_, err := e.Config.ReadRestoredTranscript(e.TempHome, projectPath, sessionID)
		return err == nil
	}
	sessionPath := filepath.Join(e.GetProjectDir(projectPath), sessionID+e.Config.SessionFileExt)
	_, err := os.Stat(sessionPath)
	return err == nil
}

// ReadSessionFile reads a session file
func (e *AgentEnv) ReadSessionFile(projectPath, sessionID string) ([]byte, error) {
	sessionPath := filepath.Join(e.GetProjectDir(projectPath), sessionID+e.Config.SessionFileExt)
	return os.ReadFile(sessionPath)
}

// ReadRestoredTranscript reads the restored transcript content.
// For agents where the session file IS the transcript (Claude, Gemini), this
// reads the session file. For agents with separate storage (OpenCode), this
// delegates to the config's ReadRestoredTranscript function.
func (e *AgentEnv) ReadRestoredTranscript(projectPath, sessionID string) ([]byte, error) {
	if e.Config.ReadRestoredTranscript != nil {
		return e.Config.ReadRestoredTranscript(e.TempHome, projectPath, sessionID)
	}
	return e.ReadSessionFile(projectPath, sessionID)
}

// GetSessionsIndexPath returns the path to sessions-index.json for a project
func (e *AgentEnv) GetSessionsIndexPath(projectPath string) string {
	return filepath.Join(e.GetProjectDir(projectPath), "sessions-index.json")
}

// SessionsIndexExists checks if sessions-index.json exists for a project
func (e *AgentEnv) SessionsIndexExists(projectPath string) bool {
	_, err := os.Stat(e.GetSessionsIndexPath(projectPath))
	return err == nil
}

// encodeProjectPath converts an absolute path to Claude's encoded format.
// This must match the production EncodeProjectPath in internal/claude/session.go.
func encodeProjectPath(path string) string {
	// Replace path separators with dashes
	encoded := ""
	for _, c := range path {
		if c == '/' || c == filepath.Separator {
			encoded += "-"
		} else {
			encoded += string(c)
		}
	}
	// Ensure it starts with a dash (the root "/" becomes the leading dash)
	if len(encoded) > 0 && encoded[0] != '-' {
		encoded = "-" + encoded
	}
	return encoded
}
