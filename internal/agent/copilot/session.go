package copilot

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/re-cinq/shift-log/internal/agent"
)

// sessionMeta represents lightweight metadata from a Copilot session workspace.yaml.
// Copilot CLI has changed the field name/nesting it uses for the session's
// working directory across releases (e.g. a flat "cwd" key vs "directory" vs
// a nested "workspace.cwd"). Rather than bind to one schema, we collect every
// string leaf value found in the YAML document and match against whichever
// value happens to equal the path we're looking for. This keeps discovery
// working across CLI versions without depending on an exact key name.
type sessionMeta struct {
	PathCandidates []string
}

// MatchesPath reports whether any string value found in the session metadata
// resolves to the same filesystem path as projectPath.
func (m *sessionMeta) MatchesPath(projectPath string) bool {
	if m == nil {
		return false
	}
	for _, candidate := range m.PathCandidates {
		if candidate == "" {
			continue
		}
		if agent.PathsEqual(candidate, projectPath) {
			return true
		}
	}
	return false
}

// writeMeta is the minimal workspace.yaml structure written by WriteSessionFile
// when restoring a session for `copilot --resume`.
type writeMeta struct {
	ID string `yaml:"id"`
}

// GetCopilotDir returns the path to Copilot's config/data directory.
func GetCopilotDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory: %w", err)
	}
	return filepath.Join(home, ".copilot"), nil
}

// GetSessionStateDir returns the session state directory.
func GetSessionStateDir() (string, error) {
	copilotDir, err := GetCopilotDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(copilotDir, "session-state"), nil
}

// parseSessionMeta reads a workspace.yaml from a Copilot session directory,
// collecting every string leaf value it contains for later path matching.
func parseSessionMeta(sessionDir string) (*sessionMeta, error) {
	path := filepath.Join(sessionDir, "workspace.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var raw interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	var candidates []string
	collectYAMLStrings(raw, &candidates)

	return &sessionMeta{PathCandidates: candidates}, nil
}

// collectYAMLStrings walks a generically-decoded YAML value (as produced by
// unmarshalling into an interface{}) and appends every string leaf it finds
// to out. This lets callers match against session metadata regardless of
// which field name or nesting level a CLI version happens to use.
func collectYAMLStrings(v interface{}, out *[]string) {
	switch val := v.(type) {
	case string:
		*out = append(*out, val)
	case map[string]interface{}:
		for _, vv := range val {
			collectYAMLStrings(vv, out)
		}
	case map[interface{}]interface{}:
		for _, vv := range val {
			collectYAMLStrings(vv, out)
		}
	case []interface{}:
		for _, vv := range val {
			collectYAMLStrings(vv, out)
		}
	}
}

// GetTranscriptPath returns the path to the events.jsonl transcript within a session directory.
func GetTranscriptPath(sessionDir string) string {
	return filepath.Join(sessionDir, "events.jsonl")
}

// WriteSessionFile writes a session directory structure to Copilot's session state directory.
// Creates <sessionDir>/<sessionID>/ with workspace.yaml and events.jsonl.
func WriteSessionFile(sessionID string, data []byte) (string, error) {
	stateDir, err := GetSessionStateDir()
	if err != nil {
		return "", err
	}

	sessionDir := filepath.Join(stateDir, sessionID)
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return "", fmt.Errorf("could not create session directory: %w", err)
	}

	// Write workspace.yaml
	meta := writeMeta{ID: sessionID}
	yamlData, err := yaml.Marshal(&meta)
	if err != nil {
		return "", fmt.Errorf("could not marshal workspace.yaml: %w", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "workspace.yaml"), yamlData, 0600); err != nil {
		return "", err
	}

	// Write events.jsonl
	eventsPath := GetTranscriptPath(sessionDir)
	return eventsPath, os.WriteFile(eventsPath, data, 0600)
}
