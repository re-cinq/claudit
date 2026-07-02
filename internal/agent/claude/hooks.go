package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Hook defines a Claude Code hook configuration.
type Hook struct {
	Matcher string    `json:"matcher"`
	Hooks   []HookCmd `json:"hooks"`
}

// HookCmd defines a command to run for a hook.
type HookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// HooksConfig represents the nested hooks configuration.
type HooksConfig struct {
	PostToolUse  []Hook `json:"PostToolUse,omitempty"`
	SessionStart []Hook `json:"SessionStart,omitempty"`
	SessionEnd   []Hook `json:"SessionEnd,omitempty"`
}

// Settings represents Claude Code's settings.local.json structure.
type Settings struct {
	Hooks HooksConfig            `json:"hooks,omitempty"`
	Other map[string]interface{} `json:"-"`
}

// ReadSettings reads the Claude settings file from the given directory.
func ReadSettings(claudeDir string) (*Settings, error) {
	path := filepath.Join(claudeDir, "settings.local.json")

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

// WriteSettings writes the settings to the Claude settings file.
func WriteSettings(claudeDir string, settings *Settings) error {
	output := make(map[string]interface{})
	for k, v := range settings.Other {
		output[k] = v
	}

	if len(settings.Hooks.PostToolUse) > 0 || len(settings.Hooks.SessionStart) > 0 || len(settings.Hooks.SessionEnd) > 0 {
		output["hooks"] = settings.Hooks
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		return err
	}

	path := filepath.Join(claudeDir, "settings.local.json")
	return os.WriteFile(path, data, 0644)
}

// AddShiftlogHook adds or updates the shiftlog store hook in settings.
func AddShiftlogHook(settings *Settings) {
	shiftlogHook := Hook{
		Matcher: "Bash",
		Hooks: []HookCmd{
			{
				Type:    "command",
				Command: "shiftlog store",
				Timeout: 30,
			},
		},
	}

	for i, hook := range settings.Hooks.PostToolUse {
		if hook.Matcher == "Bash" && len(hook.Hooks) > 0 {
			for _, h := range hook.Hooks {
				if h.Command == "shiftlog store" {
					settings.Hooks.PostToolUse[i] = shiftlogHook
					return
				}
			}
		}
	}

	settings.Hooks.PostToolUse = append(settings.Hooks.PostToolUse, shiftlogHook)
}

// AddSessionHooks adds or updates the SessionStart and SessionEnd hooks.
func AddSessionHooks(settings *Settings) {
	sessionStartHook := Hook{
		Matcher: "",
		Hooks: []HookCmd{
			{
				Type:    "command",
				Command: "shiftlog session-start",
				Timeout: 5,
			},
		},
	}

	sessionEndHook := Hook{
		Matcher: "",
		Hooks: []HookCmd{
			{
				Type:    "command",
				Command: "shiftlog session-end",
				Timeout: 5,
			},
		},
	}

	settings.Hooks.SessionStart = addOrUpdateHook(settings.Hooks.SessionStart, sessionStartHook, "shiftlog session-start")
	settings.Hooks.SessionEnd = addOrUpdateHook(settings.Hooks.SessionEnd, sessionEndHook, "shiftlog session-end")
}

// RemoveShiftlogHook removes shiftlog store hook entries from PostToolUse.
func RemoveShiftlogHook(settings *Settings) {
	filtered := settings.Hooks.PostToolUse[:0]
	for _, hook := range settings.Hooks.PostToolUse {
		isShiftlog := false
		for _, h := range hook.Hooks {
			if h.Command == "shiftlog store" {
				isShiftlog = true
				break
			}
		}
		if !isShiftlog {
			filtered = append(filtered, hook)
		}
	}
	settings.Hooks.PostToolUse = filtered
}

// RemoveSessionHooks removes shiftlog session hooks from SessionStart and SessionEnd.
func RemoveSessionHooks(settings *Settings) {
	settings.Hooks.SessionStart = removeHookByCommand(settings.Hooks.SessionStart, "shiftlog session-start")
	settings.Hooks.SessionEnd = removeHookByCommand(settings.Hooks.SessionEnd, "shiftlog session-end")
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

func addOrUpdateHook(hooks []Hook, newHook Hook, command string) []Hook {
	for i, hook := range hooks {
		if len(hook.Hooks) > 0 {
			for _, h := range hook.Hooks {
				if h.Command == command {
					hooks[i] = newHook
					return hooks
				}
			}
		}
	}
	return append(hooks, newHook)
}
