package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/re-cinq/shift-log/internal/git"
	"github.com/re-cinq/shift-log/internal/storage"
	"github.com/spf13/cobra"
)

var (
	searchAgent         string
	searchBranch        string
	searchModel         string
	searchBefore        string
	searchAfter         string
	searchLimit         int
	searchContext        int
	searchMetadataOnly  bool
	searchCaseSensitive bool
	searchRegex         bool
)

// ANSI color codes (local to avoid coupling with agent/render.go)
const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiYellow = "\033[33m"
)

var searchCmd = &cobra.Command{
	Use:     "search [query]",
	Short:   "Search through stored conversations",
	GroupID: "human",
	Long: `Searches conversation transcripts stored as Git Notes.

Supports text search through conversation content and metadata filtering.
Text search is case-insensitive by default.

Examples:
  shiftlog search "authentication"             # Find conversations mentioning auth
  shiftlog search --agent claude                # All Claude conversations
  shiftlog search --branch main --limit 5       # Recent conversations on main
  shiftlog search "test" --regex --context 2    # Regex search with context lines
  shiftlog search --before 2025-01-01           # Conversations before a date
  shiftlog search "bug" --metadata-only         # Only match metadata, not content`,
	Args: cobra.MaximumNArgs(1),
	RunE: runSearch,
}

func init() {
	searchCmd.Flags().StringVar(&searchAgent, "agent", "", "filter by agent name (e.g. claude, copilot)")
	searchCmd.Flags().StringVar(&searchBranch, "branch", "", "filter by git branch")
	searchCmd.Flags().StringVar(&searchModel, "model", "", "filter by model (substring match)")
	searchCmd.Flags().StringVar(&searchBefore, "before", "", "filter by date (YYYY-MM-DD or RFC3339)")
	searchCmd.Flags().StringVar(&searchAfter, "after", "", "filter by date (YYYY-MM-DD or RFC3339)")
	searchCmd.Flags().IntVar(&searchLimit, "limit", 20, "max number of results")
	searchCmd.Flags().IntVar(&searchContext, "context", 1, "lines of context around matches")
	searchCmd.Flags().BoolVar(&searchMetadataOnly, "metadata-only", false, "skip transcript search, filter by metadata only")
	searchCmd.Flags().BoolVar(&searchCaseSensitive, "case-sensitive", false, "case-sensitive matching (default: insensitive)")
	searchCmd.Flags().BoolVar(&searchRegex, "regex", false, "treat query as a regular expression")
	rootCmd.AddCommand(searchCmd)
}

func runSearch(cmd *cobra.Command, args []string) error {
	if err := git.RequireGitRepo(); err != nil {
		return err
	}

	query := ""
	if len(args) > 0 {
		query = args[0]
	}

	// Require at least a query or one filter flag
	hasFilter := searchAgent != "" || searchBranch != "" || searchModel != "" ||
		searchBefore != "" || searchAfter != ""
	if query == "" && !hasFilter {
		return fmt.Errorf("provide a search query or at least one filter flag (--agent, --branch, --model, --before, --after)")
	}

	params := &storage.SearchParams{
		Query:         query,
		Agent:         searchAgent,
		Branch:        searchBranch,
		Model:         searchModel,
		Limit:         searchLimit,
		ContextLines:  searchContext,
		MetadataOnly:  searchMetadataOnly,
		CaseSensitive: searchCaseSensitive,
		Regex:         searchRegex,
	}

	// Parse date flags
	if searchBefore != "" {
		t, err := parseSearchDate(searchBefore)
		if err != nil {
			return fmt.Errorf("invalid --before date: %w", err)
		}
		params.Before = t
	}
	if searchAfter != "" {
		t, err := parseSearchDate(searchAfter)
		if err != nil {
			return fmt.Errorf("invalid --after date: %w", err)
		}
		params.After = t
	}

	results, err := storage.Search(params)
	if err != nil {
		return err
	}

	if len(results) == 0 {
		fmt.Println("no matching conversations found")
		return nil
	}

	useColor := os.Getenv("NO_COLOR") == ""
	for i, result := range results {
		if i > 0 {
			fmt.Println()
		}
		printSearchResult(result, useColor)
	}

	return nil
}

func printSearchResult(result storage.SearchResult, useColor bool) {
	shortSHA := result.CommitSHA
	if len(shortSHA) > 7 {
		shortSHA = shortSHA[:7]
	}

	shortDate := result.CommitDate
	if len(shortDate) >= 10 {
		shortDate = shortDate[:10]
	}

	msg := result.CommitMsg
	if len(msg) > 50 {
		msg = msg[:47] + "..."
	}

	// Header line: abc1234 2024-01-15 feat: add auth (claude, main, 42 messages)
	if useColor {
		fmt.Printf("%s%s%s %s %s (%s, %s, %d messages)\n",
			ansiBold, shortSHA, ansiReset,
			shortDate, msg,
			result.Agent, result.Branch, result.MsgCount)
	} else {
		fmt.Printf("%s %s %s (%s, %s, %d messages)\n",
			shortSHA, shortDate, msg,
			result.Agent, result.Branch, result.MsgCount)
	}

	// Match snippets
	for _, m := range result.Matches {
		label := formatMatchLabel(m)
		lines := strings.Split(m.Snippet, "\n")
		for _, line := range lines {
			if useColor {
				fmt.Printf("  %s%s%s %s\n", ansiDim, label, ansiReset, highlightMatch(line, useColor))
			} else {
				fmt.Printf("  %s %s\n", label, line)
			}
			// Only show label on first line
			label = strings.Repeat(" ", len(stripANSI(label)))
		}
	}
}

func formatMatchLabel(m storage.SearchMatch) string {
	label := "[" + m.EntryType
	if m.BlockType == "tool_use" && m.ToolName != "" {
		label += "/tool_use: " + m.ToolName
	} else if m.BlockType != "text" {
		label += "/" + m.BlockType
	}
	label += "]"
	return label
}

func highlightMatch(line string, _ bool) string {
	// Highlighting individual matches in the snippet would require carrying
	// match offsets through the pipeline. For now, just return the line as-is.
	// The snippet context already helps users find what they're looking for.
	return line
}

func stripANSI(s string) string {
	// Simple strip: remove \033[...m sequences
	result := s
	for {
		idx := strings.Index(result, "\033[")
		if idx < 0 {
			break
		}
		end := strings.Index(result[idx:], "m")
		if end < 0 {
			break
		}
		result = result[:idx] + result[idx+end+1:]
	}
	return result
}

func parseSearchDate(s string) (time.Time, error) {
	// Try YYYY-MM-DD first (most common)
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	// Try RFC3339
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("expected YYYY-MM-DD or RFC3339 format, got %q", s)
}
