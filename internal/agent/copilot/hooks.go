package copilot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// HooksFile represents the Copilot CLI hooks file structure.
// Format: {"version": 1, "hooks": {"postToolUse": [{"type":"command","command":"...","timeoutSec":30}]}}
type HooksFile struct {
	Version int                    `json:"version"`
	Hooks   map[string][]HookEntry `json:"hooks"`
	Other   map[string]interface{} `json:"-"`
}

// HookEntry represents a single hook entry in the hooks file.
type HookEntry struct {
	Type       string `json:"type"`
	Command    string `json:"command"`
	TimeoutSec int    `json:"timeoutSec,omitempty"`
}

// hooksFilePath returns the path to the Copilot hooks file.
func hooksFilePath(repoRoot string) string {
	return filepath.Join(repoRoot, ".github", "hooks", "shiftlog.json")
}

// ReadHooksFile reads the Copilot hooks file.
func ReadHooksFile(repoRoot string) (*HooksFile, error) {
	path := hooksFilePath(repoRoot)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &HooksFile{
				Version: 1,
				Hooks:   make(map[string][]HookEntry),
				Other:   make(map[string]interface{}),
			}, nil
		}
		return nil, err
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	hf := &HooksFile{
		Version: 1,
		Hooks:   make(map[string][]HookEntry),
		Other:   make(map[string]interface{}),
	}

	if v, ok := raw["version"].(float64); ok {
		hf.Version = int(v)
	}

	if hooks, ok := raw["hooks"]; ok {
		hookBytes, _ := json.Marshal(hooks)
		_ = json.Unmarshal(hookBytes, &hf.Hooks)
		delete(raw, "hooks")
	}

	delete(raw, "version")
	hf.Other = raw

	return hf, nil
}

// WriteHooksFile writes the hooks file.
func WriteHooksFile(repoRoot string, hf *HooksFile) error {
	output := make(map[string]interface{})
	for k, v := range hf.Other {
		output[k] = v
	}

	output["version"] = hf.Version
	if len(hf.Hooks) > 0 {
		output["hooks"] = hf.Hooks
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return err
	}

	path := hooksFilePath(repoRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// AddShiftlogHooks adds the shiftlog store postToolUse hook entry.
func AddShiftlogHooks(hf *HooksFile) {
	entry := HookEntry{
		Type:       "command",
		Command:    "shiftlog store --agent=copilot",
		TimeoutSec: 30,
	}

	hf.Hooks["postToolUse"] = addOrUpdateHookEntry(
		hf.Hooks["postToolUse"], entry, "shiftlog store",
	)
}

// RemoveShiftlogHooks removes shiftlog hook entries from the hooks file.
func RemoveShiftlogHooks(hf *HooksFile) {
	for key, entries := range hf.Hooks {
		filtered := entries[:0]
		for _, e := range entries {
			if !strings.Contains(e.Command, "shiftlog store") {
				filtered = append(filtered, e)
			}
		}
		if len(filtered) == 0 {
			delete(hf.Hooks, key)
		} else {
			hf.Hooks[key] = filtered
		}
	}
}

// addOrUpdateHookEntry adds or updates a hook entry matching by command prefix.
func addOrUpdateHookEntry(entries []HookEntry, newEntry HookEntry, prefix string) []HookEntry {
	for i, e := range entries {
		if strings.Contains(e.Command, prefix) {
			entries[i] = newEntry
			return entries
		}
	}
	return append(entries, newEntry)
}
