package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// ANSI color codes
const (
	colorReset  = "\033[0m"
	colorBold   = "\033[1m"
	colorDim    = "\033[2m"
	colorBlue   = "\033[34m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
)

// Renderer renders transcript entries to the terminal.
type Renderer struct {
	w           io.Writer
	useColor    bool
	toolAliases map[string]string
}

// NewRenderer creates a new terminal renderer with optional tool aliases.
func NewRenderer(w io.Writer, toolAliases map[string]string) *Renderer {
	useColor := os.Getenv("NO_COLOR") == ""
	return &Renderer{w: w, useColor: useColor, toolAliases: toolAliases}
}

func (r *Renderer) color(code string) string {
	if r.useColor {
		return code
	}
	return ""
}

// RenderEntries renders a slice of transcript entries to the writer.
func (r *Renderer) RenderEntries(entries []TranscriptEntry) error {
	hadPrevious := false
	for _, entry := range entries {
		if r.shouldRender(&entry) {
			if hadPrevious {
				if _, err := fmt.Fprintln(r.w); err != nil {
					return err
				}
			}
			r.RenderEntry(&entry)
			hadPrevious = true
		}
	}
	return nil
}

func (r *Renderer) shouldRender(entry *TranscriptEntry) bool {
	_, ok := messageStyles[entry.Type]
	return ok
}

var messageStyles = map[MessageType]struct {
	label string
	color string
}{
	MessageTypeUser:      {"User", colorBlue},
	MessageTypeAssistant: {"Assistant", colorGreen},
	MessageTypeSystem:    {"System", colorYellow},
}

// RenderEntry renders a single transcript entry.
func (r *Renderer) RenderEntry(entry *TranscriptEntry) {
	style, ok := messageStyles[entry.Type]
	if !ok {
		return
	}
	_, _ = fmt.Fprintf(r.w, "%s%s%s:%s\n", r.color(colorBold), r.color(style.color), style.label, r.color(colorReset))
	r.renderMessageContent(entry.Message)
}

func (r *Renderer) renderMessageContent(msg *Message) {
	if msg == nil {
		return
	}

	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			r.renderText(block.Text)
		case "thinking":
			r.renderThinking(block.Thinking)
		case "tool_use":
			r.renderToolUse(block)
		case "tool_result":
			r.renderToolResult(block)
		}
	}
}

func (r *Renderer) renderText(text string) {
	if text == "" {
		return
	}
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		_, _ = fmt.Fprintf(r.w, "  %s\n", line)
	}
}

func (r *Renderer) renderThinking(thinking string) {
	if thinking == "" {
		return
	}
	_, _ = fmt.Fprintf(r.w, "  %s[thinking]%s\n", r.color(colorDim), r.color(colorReset))
	lines := strings.Split(thinking, "\n")
	maxLines := 3
	for i, line := range lines {
		if i >= maxLines {
			_, _ = fmt.Fprintf(r.w, "  %s... (%d more lines)%s\n", r.color(colorDim), len(lines)-maxLines, r.color(colorReset))
			break
		}
		_, _ = fmt.Fprintf(r.w, "  %s%s%s\n", r.color(colorDim), line, r.color(colorReset))
	}
}

// resolveToolName maps an agent-specific tool name to its canonical display name.
func (r *Renderer) resolveToolName(name string) string {
	if r.toolAliases != nil {
		if alias, ok := r.toolAliases[name]; ok {
			return alias
		}
	}
	return name
}

func (r *Renderer) renderToolUse(block ContentBlock) {
	displayName := r.resolveToolName(block.Name)
	_, _ = fmt.Fprintf(r.w, "  %s[tool: %s]%s\n", r.color(colorCyan), displayName, r.color(colorReset))

	if len(block.Input) == 0 {
		return
	}

	var input map[string]interface{}
	if err := json.Unmarshal(block.Input, &input); err != nil {
		return
	}

	// Resolve tool name for rendering rules
	canonicalName := displayName

	switch canonicalName {
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			r.renderToolInput("command", cmd)
		}
	case "Write":
		if path, ok := input["file_path"].(string); ok {
			r.renderToolInput("file", path)
		}
		if content, ok := input["content"].(string); ok {
			r.renderToolInput("content", content)
		}
	case "Read":
		if path, ok := input["file_path"].(string); ok {
			r.renderToolInput("file", path)
		}
	case "Edit":
		if path, ok := input["file_path"].(string); ok {
			r.renderToolInput("file", path)
		}
		if old, ok := input["old_string"].(string); ok {
			r.renderToolInput("old", old)
		}
		if new, ok := input["new_string"].(string); ok {
			r.renderToolInput("new", new)
		}
	case "Grep":
		if pattern, ok := input["pattern"].(string); ok {
			r.renderToolInput("pattern", pattern)
		}
		if path, ok := input["path"].(string); ok {
			r.renderToolInput("path", path)
		}
	case "Glob":
		if pattern, ok := input["pattern"].(string); ok {
			r.renderToolInput("pattern", pattern)
		}
	default:
		for k, v := range input {
			if s, ok := v.(string); ok && s != "" {
				r.renderToolInput(k, s)
			}
		}
	}
}

func (r *Renderer) renderToolInput(label, value string) {
	lines := strings.Split(value, "\n")
	if len(lines) == 1 {
		display := value
		if len(display) > 100 {
			display = display[:100] + "..."
		}
		_, _ = fmt.Fprintf(r.w, "  %s%s: %s%s\n", r.color(colorDim), label, display, r.color(colorReset))
	} else {
		_, _ = fmt.Fprintf(r.w, "  %s%s:%s\n", r.color(colorDim), label, r.color(colorReset))
		maxLines := 10
		for i, line := range lines {
			if i >= maxLines {
				_, _ = fmt.Fprintf(r.w, "    %s... (%d more lines)%s\n", r.color(colorDim), len(lines)-maxLines, r.color(colorReset))
				break
			}
			_, _ = fmt.Fprintf(r.w, "    %s%s%s\n", r.color(colorDim), line, r.color(colorReset))
		}
	}
}

func (r *Renderer) renderToolResult(block ContentBlock) {
	_, _ = fmt.Fprintf(r.w, "  %s[tool result]%s\n", r.color(colorDim), r.color(colorReset))

	if len(block.Content) == 0 {
		return
	}

	var content string
	if err := json.Unmarshal(block.Content, &content); err == nil {
		r.renderToolResultContent(content)
		return
	}

	var blocks []ContentBlock
	if err := json.Unmarshal(block.Content, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				r.renderToolResultContent(b.Text)
			}
		}
		return
	}

	raw := string(block.Content)
	if len(raw) > 200 {
		raw = raw[:200] + "..."
	}
	_, _ = fmt.Fprintf(r.w, "  %s%s%s\n", r.color(colorDim), raw, r.color(colorReset))
}

func (r *Renderer) renderToolResultContent(content string) {
	if content == "" {
		return
	}
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		_, _ = fmt.Fprintf(r.w, "  %s%s%s\n", r.color(colorDim), line, r.color(colorReset))
	}
}
