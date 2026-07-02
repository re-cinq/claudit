package storage

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/re-cinq/shift-log/internal/agent"
	"github.com/re-cinq/shift-log/internal/git"
)

// SearchParams defines the parameters for searching conversations.
type SearchParams struct {
	Query         string
	Agent         string
	Branch        string
	Model         string
	Before        time.Time
	After         time.Time
	Limit         int
	ContextLines  int
	MetadataOnly  bool
	CaseSensitive bool
	Regex         bool
}

// SearchMatch represents a single text match within a conversation.
type SearchMatch struct {
	EntryType string // "user", "assistant", "system"
	BlockType string // "text", "tool_use", "tool_result", "thinking"
	ToolName  string // populated for tool_use blocks
	Snippet   string // text snippet with context
}

// SearchResult represents a conversation that matched the search.
type SearchResult struct {
	CommitSHA  string
	CommitDate string
	CommitMsg  string
	Agent      string
	Branch     string
	Model      string
	MsgCount   int
	Matches    []SearchMatch
}

// maxMatchesPerConversation caps how many matches we report per conversation.
const maxMatchesPerConversation = 5

// matchFunc is a function that returns the index and length of the first match in s,
// or (-1, 0) if no match is found.
type matchFunc func(s string) (index, length int)

// newMatcher creates a matchFunc based on the search parameters.
func newMatcher(params *SearchParams) (matchFunc, error) {
	if params.Regex {
		pattern := params.Query
		if !params.CaseSensitive {
			pattern = "(?i)" + pattern
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex pattern: %w", err)
		}
		return func(s string) (int, int) {
			loc := re.FindStringIndex(s)
			if loc == nil {
				return -1, 0
			}
			return loc[0], loc[1] - loc[0]
		}, nil
	}

	// Substring matcher
	query := params.Query
	if !params.CaseSensitive {
		query = strings.ToLower(query)
	}
	return func(s string) (int, int) {
		haystack := s
		if !params.CaseSensitive {
			haystack = strings.ToLower(s)
		}
		idx := strings.Index(haystack, query)
		if idx < 0 {
			return -1, 0
		}
		return idx, len(query)
	}, nil
}

// buildSnippet extracts a snippet around the matched line with context lines.
func buildSnippet(text string, matchIndex int, contextLines int) string {
	lines := strings.Split(text, "\n")

	// Find which line the match is on
	charCount := 0
	matchLine := 0
	for i, line := range lines {
		end := charCount + len(line)
		if i < len(lines)-1 {
			end++ // account for the \n
		}
		if matchIndex < end {
			matchLine = i
			break
		}
		charCount = end
	}

	// Calculate range of lines to include
	startLine := matchLine - contextLines
	if startLine < 0 {
		startLine = 0
	}
	endLine := matchLine + contextLines + 1
	if endLine > len(lines) {
		endLine = len(lines)
	}

	return strings.Join(lines[startLine:endLine], "\n")
}

// findMatches searches the text for all matches and returns snippets.
func findMatches(text string, match matchFunc, contextLines int, maxMatches int) []string {
	if text == "" {
		return nil
	}

	var snippets []string
	offset := 0
	remaining := text

	for len(snippets) < maxMatches {
		idx, _ := match(remaining)
		if idx < 0 {
			break
		}
		absIdx := offset + idx
		snippet := buildSnippet(text, absIdx, contextLines)
		snippets = append(snippets, snippet)
		// Advance past this match to find the next one (by line)
		lines := strings.Split(text, "\n")
		charCount := 0
		matchLine := 0
		for i, line := range lines {
			end := charCount + len(line)
			if i < len(lines)-1 {
				end++
			}
			if absIdx < end {
				matchLine = i
				break
			}
			charCount = end
		}
		// Skip to the next line after the match line
		nextLineStart := 0
		for i := 0; i <= matchLine && i < len(lines); i++ {
			nextLineStart += len(lines[i])
			if i < len(lines)-1 {
				nextLineStart++ // newline
			}
		}
		if nextLineStart >= len(text) {
			break
		}
		offset = nextLineStart
		remaining = text[nextLineStart:]
	}

	return snippets
}

// searchTranscript searches a parsed transcript for matches and returns SearchMatch entries.
func searchTranscript(transcript *agent.Transcript, match matchFunc, contextLines int) []SearchMatch {
	var matches []SearchMatch

	for _, entry := range transcript.Entries {
		if len(matches) >= maxMatchesPerConversation {
			break
		}

		if entry.Message == nil {
			continue
		}

		entryType := string(entry.Type)

		for _, block := range entry.Message.Content {
			if len(matches) >= maxMatchesPerConversation {
				break
			}

			var text string
			var blockType string
			var toolName string

			switch block.Type {
			case "text":
				text = block.Text
				blockType = "text"
			case "thinking":
				text = block.Thinking
				blockType = "thinking"
			case "tool_use":
				blockType = "tool_use"
				toolName = block.Name
				// Search tool name + text content
				text = block.Name
				if block.Text != "" {
					text += " " + block.Text
				}
			case "tool_result":
				blockType = "tool_result"
				// Try to extract text from tool result content
				text = block.Text
				if text == "" && len(block.Content) > 0 {
					text = string(block.Content)
				}
			default:
				continue
			}

			if text == "" {
				continue
			}

			snippets := findMatches(text, match, contextLines, maxMatchesPerConversation-len(matches))
			for _, snippet := range snippets {
				matches = append(matches, SearchMatch{
					EntryType: entryType,
					BlockType: blockType,
					ToolName:  toolName,
					Snippet:   snippet,
				})
			}
		}
	}

	return matches
}

// matchesMetadata checks if a stored conversation passes the metadata filters.
func matchesMetadata(stored *StoredConversation, commitDate string, params *SearchParams) bool {
	if params.Agent != "" {
		storedAgent := stored.Agent
		if storedAgent == "" {
			storedAgent = "claude"
		}
		if !strings.EqualFold(storedAgent, params.Agent) {
			return false
		}
	}

	if params.Branch != "" {
		if !strings.EqualFold(stored.GitBranch, params.Branch) {
			return false
		}
	}

	if params.Model != "" {
		if !strings.Contains(strings.ToLower(stored.Model), strings.ToLower(params.Model)) {
			return false
		}
	}

	if !params.Before.IsZero() || !params.After.IsZero() {
		// Try parsing the commit date
		t, err := parseDate(commitDate)
		if err != nil {
			return false
		}
		if !params.Before.IsZero() && !t.Before(params.Before) {
			return false
		}
		if !params.After.IsZero() && !t.After(params.After) {
			return false
		}
	}

	return true
}

// parseDate tries common date formats from git.
func parseDate(s string) (time.Time, error) {
	formats := []string{
		time.RFC3339,
		"2006-01-02 15:04:05 -0700",
		"2006-01-02T15:04:05Z",
		"2006-01-02",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, strings.TrimSpace(s)); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("could not parse date: %s", s)
}

// Search searches conversations stored as git notes.
func Search(params *SearchParams) ([]SearchResult, error) {
	commits, err := git.ListCommitsWithNotes()
	if err != nil {
		return nil, fmt.Errorf("could not list conversations: %w", err)
	}

	var match matchFunc
	if params.Query != "" {
		match, err = newMatcher(params)
		if err != nil {
			return nil, err
		}
	}

	var results []SearchResult

	for _, sha := range commits {
		if params.Limit > 0 && len(results) >= params.Limit {
			break
		}

		// Get commit info
		message, date, err := git.GetCommitInfo(sha)
		if err != nil {
			continue
		}

		// Get conversation metadata (cheap JSON parse, no decompression)
		noteContent, err := git.GetNote(sha)
		if err != nil {
			continue
		}
		stored, err := UnmarshalStoredConversation(noteContent)
		if err != nil {
			continue
		}

		// Apply metadata filters
		if !matchesMetadata(stored, date, params) {
			continue
		}

		result := SearchResult{
			CommitSHA:  sha,
			CommitDate: date,
			CommitMsg:  message,
			Agent:      stored.Agent,
			Branch:     stored.GitBranch,
			Model:      stored.Model,
			MsgCount:   stored.MessageCount,
		}
		if result.Agent == "" {
			result.Agent = "claude"
		}

		// If no text query or metadata-only, emit result without matches
		if params.Query == "" || params.MetadataOnly {
			results = append(results, result)
			continue
		}

		// Decompress and search transcript
		transcript, err := stored.ParseTranscript()
		if err != nil {
			continue
		}

		matches := searchTranscript(transcript, match, params.ContextLines)
		if len(matches) == 0 {
			continue
		}

		result.Matches = matches
		results = append(results, result)
	}

	return results, nil
}
