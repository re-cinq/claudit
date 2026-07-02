package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"

	"github.com/re-cinq/shift-log/internal/agent"
	agentclaude "github.com/re-cinq/shift-log/internal/agent/claude"
	"github.com/re-cinq/shift-log/internal/git"
	"github.com/re-cinq/shift-log/internal/storage"
)

// CommitInfo represents commit data for the API
type CommitInfo struct {
	SHA             string          `json:"sha"`
	Message         string          `json:"message"`
	Author          string          `json:"author"`
	Date            string          `json:"date"`
	HasConversation bool            `json:"has_conversation"`
	MessageCount    int             `json:"message_count,omitempty"`
	Effort          *storage.Effort `json:"effort,omitempty"`
}

// ConversationResponse represents the full conversation data
type ConversationResponse struct {
	SHA              string                   `json:"sha"`
	SessionID        string                   `json:"session_id"`
	Timestamp        string                   `json:"timestamp"`
	MessageCount     int                      `json:"message_count"`
	Agent            string                   `json:"agent,omitempty"`
	Model            string                   `json:"model,omitempty"`
	Effort           *storage.Effort          `json:"effort,omitempty"`
	Transcript       []agent.TranscriptEntry `json:"transcript"`
	IsIncremental    bool                     `json:"is_incremental"`
	ParentCommitSHA  string                   `json:"parent_commit_sha,omitempty"`
	IncrementalCount int                      `json:"incremental_count,omitempty"`
}

// GraphNode represents a node in the commit graph
type GraphNode struct {
	SHA             string   `json:"sha"`
	Parents         []string `json:"parents"`
	HasConversation bool     `json:"has_conversation"`
	Message         string   `json:"message"`
	Date            string   `json:"date,omitempty"`
}

// BranchSummary represents a branch in the branches API response.
type BranchSummary struct {
	Name              string `json:"name"`
	HeadSHA           string `json:"head_sha"`
	IsCurrent         bool   `json:"is_current"`
	CommitDate        string `json:"commit_date"`
	ConversationCount int    `json:"conversation_count"`
}

// BranchGraphData is the top-level response for the branch graph endpoint.
type BranchGraphData struct {
	Branches []BranchGraphEntry `json:"branches"`
}

// ForkPoint describes where a branch diverged from another branch.
type ForkPoint struct {
	ParentBranch string `json:"parent_branch"`
	CommitSHA    string `json:"commit_sha"`
}

// BranchGraphEntry holds graph nodes for a single branch.
type BranchGraphEntry struct {
	Name      string     `json:"name"`
	IsCurrent bool       `json:"is_current"`
	Nodes     []GraphNode `json:"nodes"`
	ForkPoint *ForkPoint `json:"fork_point,omitempty"`
}

// writeJSONError writes a JSON error response with the given status code.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// buildNoteSet returns a set of commit SHAs that have conversation notes.
func buildNoteSet() (map[string]bool, error) {
	commitsWithNotes, err := git.ListCommitsWithNotes()
	if err != nil {
		return nil, err
	}
	noteSet := make(map[string]bool, len(commitsWithNotes))
	for _, sha := range commitsWithNotes {
		noteSet[sha] = true
	}
	return noteSet, nil
}

// buildAllNoteSet returns the set of all commit SHAs with notes (cross-branch).
func buildAllNoteSet(repoDir string) (map[string]bool, error) {
	return git.ListAllCommitsWithNotes(repoDir)
}

// getStoredOrWriteError retrieves a stored conversation for the given SHA,
// writing an appropriate JSON error response and returning nil if not found or on error.
func getStoredOrWriteError(w http.ResponseWriter, commitSHA string) *storage.StoredConversation {
	stored, err := storage.GetStoredConversation(commitSHA)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to read conversation")
		return nil
	}
	if stored == nil {
		writeJSONError(w, http.StatusNotFound, "no conversation found")
		return nil
	}
	return stored
}

// handleCommits returns a list of commits with conversation metadata
func (s *Server) handleCommits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse query parameters
	limit := 100
	offset := 0
	hasConversationFilter := false

	if l := r.URL.Query().Get("limit"); l != "" {
		if val, err := strconv.Atoi(l); err == nil && val > 0 {
			limit = val
		}
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if val, err := strconv.Atoi(o); err == nil && val >= 0 {
			offset = val
		}
	}
	if hc := r.URL.Query().Get("has_conversation"); hc == "true" {
		hasConversationFilter = true
	}

	branchParam := r.URL.Query().Get("branch")

	var noteSet map[string]bool
	var err error
	if branchParam != "" {
		noteSet, err = buildAllNoteSet(s.repoDir)
	} else {
		noteSet, err = buildNoteSet()
	}
	if err != nil {
		http.Error(w, "Failed to list conversations", http.StatusInternalServerError)
		return
	}

	// Get all commits
	var commits []CommitData
	if branchParam != "" {
		commits, err = getCommitListForRef(branchParam, limit+offset, s.repoDir)
	} else {
		commits, err = getCommitList(limit+offset, s.repoDir)
	}
	if err != nil {
		http.Error(w, "Failed to get commits", http.StatusInternalServerError)
		return
	}

	var result []CommitInfo

	for _, commit := range commits {
		hasConv := noteSet[commit.SHA]

		if hasConversationFilter && !hasConv {
			continue
		}

		info := CommitInfo{
			SHA:             commit.SHA,
			Message:         commit.Message,
			Author:          commit.Author,
			Date:            commit.Date,
			HasConversation: hasConv,
		}

		// Get message count and effort if has conversation
		if hasConv {
			if stored, err := storage.GetStoredConversation(commit.SHA); err == nil && stored != nil {
				info.MessageCount = stored.MessageCount
				info.Effort = stored.Effort
			}
		}

		result = append(result, info)
	}

	// Apply pagination
	if offset > len(result) {
		result = []CommitInfo{}
	} else if offset+limit > len(result) {
		result = result[offset:]
	} else {
		result = result[offset : offset+limit]
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// handleCommitDetail returns the full conversation for a specific commit
func (s *Server) handleCommitDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract SHA from path
	path := strings.TrimPrefix(r.URL.Path, "/api/commits/")
	sha := strings.TrimSuffix(path, "/")

	if sha == "" {
		http.Error(w, "Commit SHA required", http.StatusBadRequest)
		return
	}

	// Check if incremental mode is requested
	incremental := r.URL.Query().Get("incremental") == "true"

	// Resolve the reference
	fullSHA, err := git.ResolveRef(sha)
	if err != nil {
		http.Error(w, "Invalid commit reference", http.StatusBadRequest)
		return
	}

	stored := getStoredOrWriteError(w, fullSHA)
	if stored == nil {
		return
	}

	// Parse transcript
	transcript, err := stored.ParseTranscript()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to parse transcript")
		return
	}

	// Determine which entries to return
	var entries []agent.TranscriptEntry
	var parentSHA string
	var isIncremental bool

	if incremental {
		var lastEntryUUID string
		parentSHA, lastEntryUUID = storage.FindParentConversationBoundary(fullSHA, stored.SessionID)
		if lastEntryUUID != "" {
			entries = transcript.GetEntriesSince(lastEntryUUID)
			isIncremental = true
		} else {
			entries = transcript.Entries
		}
	} else {
		entries = transcript.Entries
	}

	response := ConversationResponse{
		SHA:              fullSHA,
		SessionID:        stored.SessionID,
		Timestamp:        stored.Timestamp,
		MessageCount:     stored.MessageCount,
		Agent:            stored.Agent,
		Model:            stored.Model,
		Effort:           stored.Effort,
		Transcript:       entries,
		IsIncremental:    isIncremental,
		ParentCommitSHA:  parentSHA,
		IncrementalCount: len(entries),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// handleGraph returns the commit graph data
func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	noteSet, err := buildNoteSet()
	if err != nil {
		http.Error(w, "Failed to list conversations", http.StatusInternalServerError)
		return
	}

	// Get graph data
	nodes, err := getGraphData(50, s.repoDir)
	if err != nil {
		http.Error(w, "Failed to get graph data", http.StatusInternalServerError)
		return
	}

	for i := range nodes {
		nodes[i].HasConversation = noteSet[nodes[i].SHA]
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(nodes)
}

// handleResume triggers a session resume
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract SHA from path
	path := strings.TrimPrefix(r.URL.Path, "/api/resume/")
	sha := strings.TrimSuffix(path, "/")

	if sha == "" {
		http.Error(w, "Commit SHA required", http.StatusBadRequest)
		return
	}

	// Check for uncommitted changes
	hasChanges, err := git.HasUncommittedChanges()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to check working directory")
		return
	}

	if hasChanges {
		writeJSONError(w, http.StatusConflict, "uncommitted changes in working directory")
		return
	}

	// Resolve the reference
	fullSHA, err := git.ResolveRef(sha)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid commit reference")
		return
	}

	stored := getStoredOrWriteError(w, fullSHA)
	if stored == nil {
		return
	}

	// Decompress transcript for restore
	transcriptData, err := stored.GetTranscript()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to decompress transcript")
		return
	}

	// Resolve agent for session restoration
	agentName := stored.Agent
	if agentName == "" {
		agentName = "claude"
	}
	ag, agErr := agent.Get(agent.Name(agentName))

	// Restore session using the agent
	var restoreErr error
	if agErr == nil {
		restoreErr = ag.RestoreSession(
			s.repoDir,
			stored.SessionID,
			stored.GitBranch,
			transcriptData,
			stored.MessageCount,
			"Restored from web UI",
		)
	} else {
		// Fallback to Claude agent directly
		var claudeAgent agentclaude.Agent
		restoreErr = claudeAgent.RestoreSession(
			s.repoDir,
			stored.SessionID,
			stored.GitBranch,
			transcriptData,
			stored.MessageCount,
			"Restored from web UI",
		)
	}
	err = restoreErr
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to restore session: %v", err))
		return
	}

	// Checkout commit
	if err := git.Checkout(fullSHA); err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to checkout: %v", err))
		return
	}

	// Launch Claude in background
	claudeCmd := exec.Command("claude", "--resume", stored.SessionID)
	claudeCmd.Dir = s.repoDir
	if err := claudeCmd.Start(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to launch claude: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":     "success",
		"session_id": stored.SessionID,
	})
}

// CommitData holds basic commit information
type CommitData struct {
	SHA     string
	Message string
	Author  string
	Date    string
}

// fieldSep is the delimiter used to split git log output.
// We use %x00 in git --format strings to emit a null byte, which avoids
// collisions with commit messages that may contain pipes or other punctuation.
const fieldSep = "\x00"

// getCommitList returns a list of commits
func getCommitList(limit int, repoDir string) ([]CommitData, error) {
	cmd := exec.Command("git", "log", fmt.Sprintf("--max-count=%d", limit),
		"--format=%H%x00%s%x00%an%x00%ci")
	cmd.Dir = repoDir
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var commits []CommitData
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, fieldSep, 4)
		if len(parts) < 4 {
			continue
		}
		commits = append(commits, CommitData{
			SHA:     parts[0],
			Message: parts[1],
			Author:  parts[2],
			Date:    parts[3],
		})
	}

	return commits, nil
}

// getGraphData returns commit graph data
func getGraphData(limit int, repoDir string) ([]GraphNode, error) {
	cmd := exec.Command("git", "log", fmt.Sprintf("--max-count=%d", limit),
		"--format=%H%x00%P%x00%s%x00%ci")
	cmd.Dir = repoDir
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	return parseGraphNodes(output), nil
}

// getCommitListForRef returns commits reachable from a specific ref.
func getCommitListForRef(ref string, limit int, repoDir string) ([]CommitData, error) {
	cmd := exec.Command("git", "log", ref, fmt.Sprintf("--max-count=%d", limit),
		"--format=%H%x00%s%x00%an%x00%ci")
	cmd.Dir = repoDir
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var commits []CommitData
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, fieldSep, 4)
		if len(parts) < 4 {
			continue
		}
		commits = append(commits, CommitData{
			SHA:     parts[0],
			Message: parts[1],
			Author:  parts[2],
			Date:    parts[3],
		})
	}

	return commits, nil
}

// getGraphDataForRef returns commit graph data for a specific ref.
func getGraphDataForRef(ref string, limit int, repoDir string) ([]GraphNode, error) {
	cmd := exec.Command("git", "log", ref, fmt.Sprintf("--max-count=%d", limit),
		"--format=%H%x00%P%x00%s%x00%ci")
	cmd.Dir = repoDir
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	return parseGraphNodes(output), nil
}

// parseGraphNodes parses git log output into GraphNode structs.
func parseGraphNodes(output []byte) []GraphNode {
	var nodes []GraphNode
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, fieldSep, 4)
		if len(parts) < 3 {
			continue
		}

		var parents []string
		if parts[1] != "" {
			parents = strings.Split(parts[1], " ")
		}

		node := GraphNode{
			SHA:     parts[0],
			Parents: parents,
			Message: parts[2],
		}
		if len(parts) >= 4 {
			node.Date = parts[3]
		}

		nodes = append(nodes, node)
	}

	return nodes
}

// handleBranches returns a list of all branches with conversation counts.
func (s *Server) handleBranches(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	branches, err := git.ListBranches(s.repoDir)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to list branches")
		return
	}

	noteSet, err := buildAllNoteSet(s.repoDir)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to list notes")
		return
	}

	var result []BranchSummary
	for _, b := range branches {
		convCount := 0
		commits, err := getCommitListForRef(b.Name, 100, s.repoDir)
		if err == nil {
			for _, c := range commits {
				if noteSet[c.SHA] {
					convCount++
				}
			}
		}
		result = append(result, BranchSummary{
			Name:              b.Name,
			HeadSHA:           b.HeadSHA,
			IsCurrent:         b.IsCurrent,
			CommitDate:        b.CommitDate,
			ConversationCount: convCount,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// handleBranchGraph returns graph data for all branches.
func (s *Server) handleBranchGraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	perBranch := 30
	if p := r.URL.Query().Get("per_branch"); p != "" {
		if val, err := strconv.Atoi(p); err == nil && val > 0 {
			perBranch = val
		}
	}

	branches, err := git.ListBranches(s.repoDir)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to list branches")
		return
	}

	noteSet, err := buildAllNoteSet(s.repoDir)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to list notes")
		return
	}

	var entries []BranchGraphEntry
	for _, b := range branches {
		nodes, err := getGraphDataForRef(b.Name, perBranch, s.repoDir)
		if err != nil {
			continue
		}
		for i := range nodes {
			nodes[i].HasConversation = noteSet[nodes[i].SHA]
		}
		entries = append(entries, BranchGraphEntry{
			Name:      b.Name,
			IsCurrent: b.IsCurrent,
			Nodes:     nodes,
		})
	}

	// Compute fork points: for each non-root branch, find where it diverged.
	// Use the current branch (or first) as root; all others get fork_point relative to it.
	rootIdx := 0
	for i, e := range entries {
		if e.IsCurrent {
			rootIdx = i
			break
		}
	}
	for i := range entries {
		if i == rootIdx {
			continue
		}
		// First try merge-base with root branch
		mb, err := git.MergeBase(s.repoDir, entries[i].Name, entries[rootIdx].Name)
		if err == nil && mb != "" {
			parent := entries[rootIdx].Name
			// Check if a closer parent exists among other branches
			for j := range entries {
				if j == i || j == rootIdx {
					continue
				}
				mb2, err2 := git.MergeBase(s.repoDir, entries[i].Name, entries[j].Name)
				if err2 != nil || mb2 == "" {
					continue
				}
				// If mb2 is a descendant of mb, it's a closer fork point
				chk := exec.Command("git", "merge-base", "--is-ancestor", mb, mb2)
				chk.Dir = s.repoDir
				if chk.Run() == nil && mb2 != mb {
					mb = mb2
					parent = entries[j].Name
				}
			}
			entries[i].ForkPoint = &ForkPoint{
				ParentBranch: parent,
				CommitSHA:    mb,
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(BranchGraphData{Branches: entries})
}
