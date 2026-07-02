package storage

import (
	"testing"
	"time"

	"github.com/re-cinq/shift-log/internal/agent"
)

func TestNewMatcherSubstring(t *testing.T) {
	// Case-insensitive by default
	match, err := newMatcher(&SearchParams{Query: "Hello"})
	if err != nil {
		t.Fatalf("newMatcher() error: %v", err)
	}

	idx, length := match("say hello world")
	if idx != 4 || length != 5 {
		t.Errorf("match(\"say hello world\") = (%d, %d), want (4, 5)", idx, length)
	}

	idx, _ = match("no match here")
	if idx != -1 {
		t.Errorf("match(\"no match here\") = %d, want -1", idx)
	}
}

func TestNewMatcherSubstringCaseSensitive(t *testing.T) {
	match, err := newMatcher(&SearchParams{Query: "Hello", CaseSensitive: true})
	if err != nil {
		t.Fatalf("newMatcher() error: %v", err)
	}

	idx, _ := match("say hello world")
	if idx != -1 {
		t.Errorf("case-sensitive match(\"say hello world\") = %d, want -1", idx)
	}

	idx, length := match("say Hello world")
	if idx != 4 || length != 5 {
		t.Errorf("case-sensitive match(\"say Hello world\") = (%d, %d), want (4, 5)", idx, length)
	}
}

func TestNewMatcherRegex(t *testing.T) {
	match, err := newMatcher(&SearchParams{Query: `hel+o`, Regex: true})
	if err != nil {
		t.Fatalf("newMatcher() error: %v", err)
	}

	idx, length := match("say helllo world")
	if idx != 4 || length != 6 {
		t.Errorf("regex match = (%d, %d), want (4, 6)", idx, length)
	}
}

func TestNewMatcherRegexCaseSensitive(t *testing.T) {
	match, err := newMatcher(&SearchParams{Query: `Hello`, Regex: true, CaseSensitive: true})
	if err != nil {
		t.Fatalf("newMatcher() error: %v", err)
	}

	idx, _ := match("say hello world")
	if idx != -1 {
		t.Errorf("case-sensitive regex match(\"say hello world\") = %d, want -1", idx)
	}

	idx, length := match("say Hello world")
	if idx != 4 || length != 5 {
		t.Errorf("case-sensitive regex match(\"say Hello world\") = (%d, %d), want (4, 5)", idx, length)
	}
}

func TestNewMatcherInvalidRegex(t *testing.T) {
	_, err := newMatcher(&SearchParams{Query: `[invalid`, Regex: true})
	if err == nil {
		t.Error("newMatcher() with invalid regex should return error")
	}
}

func TestBuildSnippet(t *testing.T) {
	text := "line0\nline1\nline2\nline3\nline4"

	// Match on "line2" (starts at index 12)
	snippet := buildSnippet(text, 12, 1)
	if snippet != "line1\nline2\nline3" {
		t.Errorf("buildSnippet(ctx=1) = %q, want %q", snippet, "line1\nline2\nline3")
	}

	// Zero context
	snippet = buildSnippet(text, 12, 0)
	if snippet != "line2" {
		t.Errorf("buildSnippet(ctx=0) = %q, want %q", snippet, "line2")
	}

	// Two context lines
	snippet = buildSnippet(text, 12, 2)
	if snippet != "line0\nline1\nline2\nline3\nline4" {
		t.Errorf("buildSnippet(ctx=2) = %q, want %q", snippet, "line0\nline1\nline2\nline3\nline4")
	}
}

func TestBuildSnippetFirstLine(t *testing.T) {
	text := "first\nsecond\nthird"
	snippet := buildSnippet(text, 0, 1)
	if snippet != "first\nsecond" {
		t.Errorf("buildSnippet(first line) = %q, want %q", snippet, "first\nsecond")
	}
}

func TestBuildSnippetLastLine(t *testing.T) {
	text := "first\nsecond\nthird"
	// "third" starts at index 13
	snippet := buildSnippet(text, 13, 1)
	if snippet != "second\nthird" {
		t.Errorf("buildSnippet(last line) = %q, want %q", snippet, "second\nthird")
	}
}

func TestFindMatchesBasic(t *testing.T) {
	match, _ := newMatcher(&SearchParams{Query: "target"})
	text := "line one\ntarget line\nline three"

	snippets := findMatches(text, match, 0, 5)
	if len(snippets) != 1 {
		t.Fatalf("findMatches() returned %d snippets, want 1", len(snippets))
	}
	if snippets[0] != "target line" {
		t.Errorf("snippet = %q, want %q", snippets[0], "target line")
	}
}

func TestFindMatchesMultiple(t *testing.T) {
	match, _ := newMatcher(&SearchParams{Query: "match"})
	text := "match one\nno hit\nmatch two\nno hit\nmatch three"

	snippets := findMatches(text, match, 0, 5)
	if len(snippets) != 3 {
		t.Fatalf("findMatches() returned %d snippets, want 3", len(snippets))
	}
}

func TestFindMatchesRespectsCap(t *testing.T) {
	match, _ := newMatcher(&SearchParams{Query: "x"})
	text := "x\nx\nx\nx\nx"

	snippets := findMatches(text, match, 0, 2)
	if len(snippets) != 2 {
		t.Fatalf("findMatches() returned %d snippets, want 2", len(snippets))
	}
}

func TestFindMatchesEmpty(t *testing.T) {
	match, _ := newMatcher(&SearchParams{Query: "nope"})
	snippets := findMatches("some text", match, 0, 5)
	if len(snippets) != 0 {
		t.Fatalf("findMatches() returned %d snippets, want 0", len(snippets))
	}
}

func TestFindMatchesEmptyText(t *testing.T) {
	match, _ := newMatcher(&SearchParams{Query: "x"})
	snippets := findMatches("", match, 0, 5)
	if snippets != nil {
		t.Fatalf("findMatches(\"\") returned non-nil")
	}
}

func TestSearchTranscriptText(t *testing.T) {
	match, _ := newMatcher(&SearchParams{Query: "help"})

	transcript := &agent.Transcript{
		Entries: []agent.TranscriptEntry{
			{
				Type: agent.MessageTypeUser,
				Message: &agent.Message{
					Content: []agent.ContentBlock{
						{Type: "text", Text: "Can you help me?"},
					},
				},
			},
			{
				Type: agent.MessageTypeAssistant,
				Message: &agent.Message{
					Content: []agent.ContentBlock{
						{Type: "text", Text: "Sure, I can help!"},
					},
				},
			},
		},
	}

	matches := searchTranscript(transcript, match, 0)
	if len(matches) != 2 {
		t.Fatalf("searchTranscript() returned %d matches, want 2", len(matches))
	}
	if matches[0].EntryType != "user" {
		t.Errorf("matches[0].EntryType = %q, want %q", matches[0].EntryType, "user")
	}
	if matches[1].EntryType != "assistant" {
		t.Errorf("matches[1].EntryType = %q, want %q", matches[1].EntryType, "assistant")
	}
}

func TestSearchTranscriptToolUse(t *testing.T) {
	match, _ := newMatcher(&SearchParams{Query: "bash"})

	transcript := &agent.Transcript{
		Entries: []agent.TranscriptEntry{
			{
				Type: agent.MessageTypeAssistant,
				Message: &agent.Message{
					Content: []agent.ContentBlock{
						{Type: "tool_use", Name: "Bash", Text: "running command"},
					},
				},
			},
		},
	}

	matches := searchTranscript(transcript, match, 0)
	if len(matches) != 1 {
		t.Fatalf("searchTranscript() returned %d matches, want 1", len(matches))
	}
	if matches[0].BlockType != "tool_use" {
		t.Errorf("matches[0].BlockType = %q, want %q", matches[0].BlockType, "tool_use")
	}
	if matches[0].ToolName != "Bash" {
		t.Errorf("matches[0].ToolName = %q, want %q", matches[0].ToolName, "Bash")
	}
}

func TestSearchTranscriptNoMessage(t *testing.T) {
	match, _ := newMatcher(&SearchParams{Query: "test"})

	transcript := &agent.Transcript{
		Entries: []agent.TranscriptEntry{
			{Type: agent.MessageTypeUser, Message: nil},
		},
	}

	matches := searchTranscript(transcript, match, 0)
	if len(matches) != 0 {
		t.Fatalf("searchTranscript() with nil message returned %d matches, want 0", len(matches))
	}
}

func TestSearchTranscriptEmptyTranscript(t *testing.T) {
	match, _ := newMatcher(&SearchParams{Query: "test"})

	transcript := &agent.Transcript{}
	matches := searchTranscript(transcript, match, 0)
	if len(matches) != 0 {
		t.Fatalf("searchTranscript() on empty transcript returned %d matches, want 0", len(matches))
	}
}

func TestSearchTranscriptMaxMatches(t *testing.T) {
	match, _ := newMatcher(&SearchParams{Query: "hit"})

	var entries []agent.TranscriptEntry
	for i := 0; i < 10; i++ {
		entries = append(entries, agent.TranscriptEntry{
			Type: agent.MessageTypeUser,
			Message: &agent.Message{
				Content: []agent.ContentBlock{
					{Type: "text", Text: "hit here"},
				},
			},
		})
	}

	transcript := &agent.Transcript{Entries: entries}
	matches := searchTranscript(transcript, match, 0)
	if len(matches) != maxMatchesPerConversation {
		t.Errorf("searchTranscript() returned %d matches, want cap of %d", len(matches), maxMatchesPerConversation)
	}
}

func TestMatchesMetadataAgent(t *testing.T) {
	stored := &StoredConversation{Agent: "claude"}
	if !matchesMetadata(stored, "2025-01-01", &SearchParams{Agent: "claude"}) {
		t.Error("should match agent=claude")
	}
	if matchesMetadata(stored, "2025-01-01", &SearchParams{Agent: "copilot"}) {
		t.Error("should not match agent=copilot")
	}
}

func TestMatchesMetadataAgentDefaultClaude(t *testing.T) {
	// Empty agent should default to "claude"
	stored := &StoredConversation{Agent: ""}
	if !matchesMetadata(stored, "2025-01-01", &SearchParams{Agent: "claude"}) {
		t.Error("empty agent should match agent=claude")
	}
}

func TestMatchesMetadataBranch(t *testing.T) {
	stored := &StoredConversation{GitBranch: "main"}
	if !matchesMetadata(stored, "2025-01-01", &SearchParams{Branch: "main"}) {
		t.Error("should match branch=main")
	}
	if matchesMetadata(stored, "2025-01-01", &SearchParams{Branch: "develop"}) {
		t.Error("should not match branch=develop")
	}
}

func TestMatchesMetadataModel(t *testing.T) {
	stored := &StoredConversation{Model: "claude-sonnet-4-5-20250514"}
	if !matchesMetadata(stored, "2025-01-01", &SearchParams{Model: "sonnet"}) {
		t.Error("should match model substring 'sonnet'")
	}
	if matchesMetadata(stored, "2025-01-01", &SearchParams{Model: "opus"}) {
		t.Error("should not match model substring 'opus'")
	}
}

func TestMatchesMetadataDateRange(t *testing.T) {
	stored := &StoredConversation{}
	date := "2025-06-15 10:00:00 -0000"

	before := time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)
	after := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)

	if !matchesMetadata(stored, date, &SearchParams{Before: before, After: after}) {
		t.Error("should match date in range")
	}

	tooEarly := time.Date(2025, 6, 16, 0, 0, 0, 0, time.UTC)
	if matchesMetadata(stored, date, &SearchParams{After: tooEarly}) {
		t.Error("should not match date after too-early cutoff")
	}
}

func TestMatchesMetadataNoFilters(t *testing.T) {
	stored := &StoredConversation{Agent: "claude", GitBranch: "main"}
	if !matchesMetadata(stored, "2025-01-01", &SearchParams{}) {
		t.Error("no filters should match everything")
	}
}
