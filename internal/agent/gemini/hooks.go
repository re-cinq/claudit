package gemini

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Hook defines a Gemini CLI hook configuration.
type Hook struct {
	Matcher string    `json:"matcher,omitempty"`
	Hooks   []HookCmd `json:"hooks"`
}

// HookCmd defines a command to run for a hook.
type HookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// HooksConfig represents the hooks section of Gemini settings.
type HooksConfig struct {
	AfterTool    []Hook `json:"AfterTool,omitempty"`
	SessionStart []Hook `json:"SessionStart,omitempty"`
	SessionEnd   []Hook `json:"SessionEnd,omitempty"`
}

// Settings represents Gemini CLI's settings.json structure.
type Settings struct {
	Hooks HooksConfig            `json:"hooks,omitempty"`
	Other map[string]interface{} `json:"-"`
}

// ReadSettings reads the Gemini settings file from the given directory.
func ReadSettings(geminiDir string) (*Settings, error) {
	path := filepath.Join(geminiDir, "settings.json")

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Settings{Other: make(map[string]interface{})}, nil
		}
		return nil, err
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	settings := &Settings{Other: make(map[string]interface{})}

	if hooks, ok := raw["hooks"]; ok {
		hookBytes, _ := json.Marshal(hooks)
		_ = json.Unmarshal(hookBytes, &settings.Hooks)
		delete(raw, "hooks")
	}

	settings.Other = raw
	return settings, nil
}

// WriteSettings writes the settings to the Gemini settings file.
func WriteSettings(geminiDir string, settings *Settings) error {
	output := make(map[string]interface{})
	for k, v := range settings.Other {
		output[k] = v
	}

	if len(settings.Hooks.AfterTool) > 0 || len(settings.Hooks.SessionStart) > 0 || len(settings.Hooks.SessionEnd) > 0 {
		output["hooks"] = settings.Hooks
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(geminiDir, 0755); err != nil {
		return err
	}

	path := filepath.Join(geminiDir, "settings.json")
	return os.WriteFile(path, data, 0644)
}

// AddShiftlogHook adds or updates the shiftlog store hook in Gemini settings.
func AddShiftlogHook(settings *Settings) {
	shiftlogHook := Hook{
		Matcher: "run_shell_command",
		Hooks: []HookCmd{
			{
				Type:    "command",
				Command: "shiftlog store --agent=gemini",
				Timeout: 30000,
			},
		},
	}

	for i, hook := range settings.Hooks.AfterTool {
		for _, h := range hook.Hooks {
			if h.Command == "shiftlog store --agent=gemini" {
				settings.Hooks.AfterTool[i] = shiftlogHook
				return
			}
		}
	}

	settings.Hooks.AfterTool = append(settings.Hooks.AfterTool, shiftlogHook)
}

// AddSessionHooks adds SessionStart and SessionEnd hooks for session tracking.
func AddSessionHooks(settings *Settings) {
	startHook := Hook{
		Hooks: []HookCmd{
			{
				Type:    "command",
				Command: "shiftlog session-start --agent=gemini",
				Timeout: 5000,
			},
		},
	}
	endHook := Hook{
		Hooks: []HookCmd{
			{
				Type:    "command",
				Command: "shiftlog session-end --agent=gemini",
				Timeout: 5000,
			},
		},
	}

	settings.Hooks.SessionStart = addOrUpdateHook(settings.Hooks.SessionStart, startHook, "shiftlog session-start")
	settings.Hooks.SessionEnd = addOrUpdateHook(settings.Hooks.SessionEnd, endHook, "shiftlog session-end")
}

// RemoveShiftlogHook removes shiftlog store hook entries from AfterTool.
func RemoveShiftlogHook(settings *Settings) {
	filtered := settings.Hooks.AfterTool[:0]
	for _, hook := range settings.Hooks.AfterTool {
		isShiftlog := false
		for _, h := range hook.Hooks {
			if h.Command == "shiftlog store --agent=gemini" {
				isShiftlog = true
				break
			}
		}
		if !isShiftlog {
			filtered = append(filtered, hook)
		}
	}
	settings.Hooks.AfterTool = filtered
}

// RemoveSessionHooks removes shiftlog session hooks from SessionStart and SessionEnd.
func RemoveSessionHooks(settings *Settings) {
	settings.Hooks.SessionStart = removeHookByCommand(settings.Hooks.SessionStart, "shiftlog session-start --agent=gemini")
	settings.Hooks.SessionEnd = removeHookByCommand(settings.Hooks.SessionEnd, "shiftlog session-end --agent=gemini")
}

func removeHookByCommand(hooks []Hook, command string) []Hook {
	filtered := hooks[:0]
	for _, hook := range hooks {
		isShiftlog := false
		for _, h := range hook.Hooks {
			if h.Command == command {
				isShiftlog = true
				break
			}
		}
		if !isShiftlog {
			filtered = append(filtered, hook)
		}
	}
	return filtered
}

func addOrUpdateHook(hooks []Hook, newHook Hook, commandPrefix string) []Hook {
	for i, hook := range hooks {
		for _, h := range hook.Hooks {
			if h.Command == commandPrefix || h.Command == commandPrefix+" --agent=gemini" {
				hooks[i] = newHook
				return hooks
			}
		}
	}
	return append(hooks, newHook)
}
