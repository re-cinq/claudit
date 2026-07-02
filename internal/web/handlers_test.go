package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/re-cinq/shift-log/internal/git"
	"github.com/re-cinq/shift-log/internal/storage"
)

// testRepo is a temporary git repository for handler tests.
type testRepo struct {
	path string
	t    *testing.T
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	dir, err := os.MkdirTemp("", "shiftlog-web-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	r := &testRepo{path: dir, t: t}
	r.git("init", "-b", "master")
	r.git("config", "user.email", "test@example.com")
	r.git("config", "user.name", "Test User")
	r.git("config", "commit.gpgsign", "false")
	return r
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.path
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *testRepo) writeFile(name, content string) {
	r.t.Helper()
	p := filepath.Join(r.path, name)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *testRepo) commit(message string) string {
	r.t.Helper()
	r.git("add", "-A")
	r.git("commit", "--no-gpg-sign", "-m", message, "--allow-empty")
	return r.git("rev-parse", "HEAD")
}

func (r *testRepo) addConversation(commitSHA, sessionID string, transcriptData []byte, messageCount int) {
	r.t.Helper()
	r.addConversationWithEffort(commitSHA, sessionID, transcriptData, messageCount, nil)
}

func (r *testRepo) addConversationWithEffort(commitSHA, sessionID string, transcriptData []byte, messageCount int, effort *storage.Effort) {
	r.t.Helper()
	stored, err := storage.NewStoredConversation(sessionID, r.path, "master", messageCount, transcriptData)
	if err != nil {
		r.t.Fatal(err)
	}
	stored.Effort = effort
	data, err := stored.Marshal()
	if err != nil {
		r.t.Fatal(err)
	}
	r.git("notes", "--ref", git.NotesRef, "add", "-f", "-m", string(data), commitSHA)
}

// chdir changes CWD to dir for git functions that operate on CWD.
func chdir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
}

// decodeJSON decodes a JSON response body, failing the test on error.
func decodeJSON(t *testing.T, w *httptest.ResponseRecorder, v interface{}) {
	t.Helper()
	if err := json.NewDecoder(w.Body).Decode(v); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
}

func sampleTranscript() []byte {
	entries := []map[string]interface{}{
		{
			"uuid": "user-1", "type": "user",
			"message": map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Hello, can you help?"},
				},
			},
		},
		{
			"uuid": "assistant-1", "parentUuid": "user-1", "type": "assistant",
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Of course! What do you need?"},
				},
			},
		},
	}
	return marshalTranscript(entries)
}

func extendedTranscript() []byte {
	entries := []map[string]interface{}{
		{
			"uuid": "user-1", "type": "user",
			"message": map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Hello, can you help?"},
				},
			},
		},
		{
			"uuid": "assistant-1", "parentUuid": "user-1", "type": "assistant",
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Of course! What do you need?"},
				},
			},
		},
		{
			"uuid": "user-2", "parentUuid": "assistant-1", "type": "user",
			"message": map[string]interface{}{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Create a file please"},
				},
			},
		},
		{
			"uuid": "assistant-2", "parentUuid": "user-2", "type": "assistant",
			"message": map[string]interface{}{
				"role": "assistant",
				"content": []map[string]interface{}{
					{"type": "text", "text": "Done! I created the file."},
					{
						"type": "tool_use", "id": "tool-1", "name": "Bash",
						"input": map[string]interface{}{"command": "echo test > file.txt"},
					},
				},
			},
		},
	}
	return marshalTranscript(entries)
}

func marshalTranscript(entries []map[string]interface{}) []byte {
	var lines []string
	for _, e := range entries {
		data, _ := json.Marshal(e)
		lines = append(lines, string(data))
	}
	return []byte(strings.Join(lines, "\n"))
}

// --- Helper function tests ---

func TestGetCommitList(t *testing.T) {
	repo := newTestRepo(t)

	repo.writeFile("a.txt", "a")
	sha1 := repo.commit("First commit")

	repo.writeFile("b.txt", "b")
	sha2 := repo.commit("Second commit")

	commits, err := getCommitList(10, repo.path)
	if err != nil {
		t.Fatalf("getCommitList: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("expected 2 commits, got %d", len(commits))
	}
	// Most recent first
	if commits[0].SHA != sha2 {
		t.Errorf("first commit SHA: want %s, got %s", sha2, commits[0].SHA)
	}
	if commits[1].SHA != sha1 {
		t.Errorf("second commit SHA: want %s, got %s", sha1, commits[1].SHA)
	}
	if commits[0].Message != "Second commit" {
		t.Errorf("message: want %q, got %q", "Second commit", commits[0].Message)
	}
	if commits[0].Author != "Test User" {
		t.Errorf("author: want %q, got %q", "Test User", commits[0].Author)
	}
	if commits[0].Date == "" {
		t.Error("date should not be empty")
	}
}

func TestGetCommitListLimit(t *testing.T) {
	repo := newTestRepo(t)

	repo.writeFile("a.txt", "a")
	repo.commit("First")
	repo.writeFile("b.txt", "b")
	repo.commit("Second")
	repo.writeFile("c.txt", "c")
	repo.commit("Third")

	commits, err := getCommitList(2, repo.path)
	if err != nil {
		t.Fatalf("getCommitList: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("expected 2 commits with limit, got %d", len(commits))
	}
	if commits[0].Message != "Third" {
		t.Errorf("expected newest commit first, got %q", commits[0].Message)
	}
}

func TestGetGraphData(t *testing.T) {
	repo := newTestRepo(t)

	repo.writeFile("a.txt", "a")
	sha1 := repo.commit("First commit")

	repo.writeFile("b.txt", "b")
	sha2 := repo.commit("Second commit")

	nodes, err := getGraphData(10, repo.path)
	if err != nil {
		t.Fatalf("getGraphData: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
	if nodes[0].SHA != sha2 {
		t.Errorf("first node SHA: want %s, got %s", sha2, nodes[0].SHA)
	}
	if nodes[0].Message != "Second commit" {
		t.Errorf("message: want %q, got %q", "Second commit", nodes[0].Message)
	}
	if len(nodes[0].Parents) != 1 || nodes[0].Parents[0] != sha1 {
		t.Errorf("second commit parents: want [%s], got %v", sha1, nodes[0].Parents)
	}
	if len(nodes[1].Parents) != 0 {
		t.Errorf("first commit should have no parents, got %v", nodes[1].Parents)
	}
}

// --- Handler tests ---

func TestHandleCommits(t *testing.T) {
	repo := newTestRepo(t)
	chdir(t, repo.path)

	repo.writeFile("a.txt", "a")
	repo.commit("First commit")

	repo.writeFile("b.txt", "b")
	sha2 := repo.commit("Second commit")
	repo.addConversation(sha2, "session-1", sampleTranscript(), 2)

	srv := NewServer(0, repo.path)

	t.Run("lists all commits", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/commits", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status: want 200, got %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type: want application/json, got %q", ct)
		}

		var commits []CommitInfo
		decodeJSON(t, w, &commits)
		if len(commits) != 2 {
			t.Fatalf("expected 2 commits, got %d", len(commits))
		}
	})

	t.Run("includes conversation metadata", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/commits", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var commits []CommitInfo
		decodeJSON(t, w, &commits)

		var withConv *CommitInfo
		for i, c := range commits {
			if c.SHA == sha2 {
				withConv = &commits[i]
				break
			}
		}
		if withConv == nil {
			t.Fatal("commit with conversation not found")
		}
		if !withConv.HasConversation {
			t.Error("expected HasConversation=true")
		}
		if withConv.MessageCount != 2 {
			t.Errorf("MessageCount: want 2, got %d", withConv.MessageCount)
		}
	})

	t.Run("filters by has_conversation", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/commits?has_conversation=true", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var commits []CommitInfo
		decodeJSON(t, w, &commits)

		if len(commits) != 1 {
			t.Fatalf("expected 1 commit with filter, got %d", len(commits))
		}
		if commits[0].SHA != sha2 {
			t.Errorf("filtered commit SHA: want %s, got %s", sha2, commits[0].SHA)
		}
	})

	t.Run("pagination limit", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/commits?limit=1", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var commits []CommitInfo
		decodeJSON(t, w, &commits)

		if len(commits) != 1 {
			t.Fatalf("expected 1 commit with limit=1, got %d", len(commits))
		}
	})

	t.Run("pagination offset", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/commits?limit=1&offset=1", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var commits []CommitInfo
		decodeJSON(t, w, &commits)

		if len(commits) != 1 {
			t.Fatalf("expected 1 commit with offset=1, got %d", len(commits))
		}
	})

	t.Run("offset beyond range returns empty", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/commits?offset=100", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var commits []CommitInfo
		decodeJSON(t, w, &commits)

		if len(commits) != 0 {
			t.Errorf("expected 0 commits, got %d", len(commits))
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/commits", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("status: want 405, got %d", w.Code)
		}
	})
}

func TestHandleCommitDetail(t *testing.T) {
	repo := newTestRepo(t)
	chdir(t, repo.path)

	repo.writeFile("a.txt", "a")
	sha1 := repo.commit("First commit")

	repo.writeFile("b.txt", "b")
	sha2 := repo.commit("Second commit")
	repo.addConversation(sha2, "session-1", sampleTranscript(), 2)

	srv := NewServer(0, repo.path)

	t.Run("returns conversation", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/commits/"+sha2, nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status: want 200, got %d: %s", w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type: want application/json, got %q", ct)
		}

		var resp ConversationResponse
		decodeJSON(t, w, &resp)
		if resp.SHA != sha2 {
			t.Errorf("SHA: want %s, got %s", sha2, resp.SHA)
		}
		if resp.SessionID != "session-1" {
			t.Errorf("SessionID: want %q, got %q", "session-1", resp.SessionID)
		}
		if len(resp.Transcript) != 2 {
			t.Errorf("transcript entries: want 2, got %d", len(resp.Transcript))
		}
		if resp.MessageCount != 2 {
			t.Errorf("MessageCount: want 2, got %d", resp.MessageCount)
		}
	})

	t.Run("short SHA resolves", func(t *testing.T) {
		shortSHA := sha2[:7]
		req := httptest.NewRequest("GET", "/api/commits/"+shortSHA, nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status: want 200, got %d: %s", w.Code, w.Body.String())
		}

		var resp ConversationResponse
		decodeJSON(t, w, &resp)
		if resp.SHA != sha2 {
			t.Errorf("resolved SHA: want %s, got %s", sha2, resp.SHA)
		}
	})

	t.Run("transcript entry types", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/commits/"+sha2, nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var resp ConversationResponse
		decodeJSON(t, w, &resp)

		if string(resp.Transcript[0].Type) != "user" {
			t.Errorf("first entry type: want user, got %s", resp.Transcript[0].Type)
		}
		if string(resp.Transcript[1].Type) != "assistant" {
			t.Errorf("second entry type: want assistant, got %s", resp.Transcript[1].Type)
		}
	})

	t.Run("no conversation returns 404", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/commits/"+sha1, nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("status: want 404, got %d", w.Code)
		}
	})

	t.Run("missing SHA returns 400", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/commits/", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status: want 400, got %d", w.Code)
		}
	})

	t.Run("invalid SHA returns 400", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/commits/not-a-valid-ref", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status: want 400, got %d", w.Code)
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/commits/"+sha2, nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("status: want 405, got %d", w.Code)
		}
	})
}

func TestHandleCommitDetailIncremental(t *testing.T) {
	repo := newTestRepo(t)
	chdir(t, repo.path)

	// First commit with short conversation
	repo.writeFile("a.txt", "a")
	sha1 := repo.commit("First commit")
	repo.addConversation(sha1, "session-1", sampleTranscript(), 2)

	// Second commit with extended conversation (same session)
	repo.writeFile("b.txt", "b")
	sha2 := repo.commit("Second commit")
	repo.addConversation(sha2, "session-1", extendedTranscript(), 4)

	srv := NewServer(0, repo.path)

	t.Run("incremental returns only new entries", func(t *testing.T) {
		req := httptest.NewRequest("GET", fmt.Sprintf("/api/commits/%s?incremental=true", sha2), nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status: want 200, got %d: %s", w.Code, w.Body.String())
		}

		var resp ConversationResponse
		decodeJSON(t, w, &resp)

		if !resp.IsIncremental {
			t.Error("expected IsIncremental=true")
		}
		if resp.ParentCommitSHA != sha1 {
			t.Errorf("ParentCommitSHA: want %s, got %s", sha1, resp.ParentCommitSHA)
		}
		// Should only have user-2 and assistant-2
		if len(resp.Transcript) != 2 {
			t.Errorf("incremental entries: want 2, got %d", len(resp.Transcript))
		}
		if resp.IncrementalCount != 2 {
			t.Errorf("IncrementalCount: want 2, got %d", resp.IncrementalCount)
		}
	})

	t.Run("full mode returns all entries", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/commits/"+sha2, nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var resp ConversationResponse
		decodeJSON(t, w, &resp)

		if resp.IsIncremental {
			t.Error("expected IsIncremental=false for full mode")
		}
		if len(resp.Transcript) != 4 {
			t.Errorf("full transcript entries: want 4, got %d", len(resp.Transcript))
		}
	})

	t.Run("incremental with no parent conversation returns all", func(t *testing.T) {
		// Create a commit with a different session ID (no matching parent)
		repo.writeFile("c.txt", "c")
		sha3 := repo.commit("Third commit")
		repo.addConversation(sha3, "different-session", sampleTranscript(), 2)

		req := httptest.NewRequest("GET", fmt.Sprintf("/api/commits/%s?incremental=true", sha3), nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var resp ConversationResponse
		decodeJSON(t, w, &resp)

		// Different session from parent, so no incremental boundary found
		if resp.IsIncremental {
			t.Error("expected IsIncremental=false when parent has different session")
		}
		if len(resp.Transcript) != 2 {
			t.Errorf("entries: want 2 (full), got %d", len(resp.Transcript))
		}
	})
}

func TestHandleGraph(t *testing.T) {
	repo := newTestRepo(t)
	chdir(t, repo.path)

	repo.writeFile("a.txt", "a")
	repo.commit("First commit")

	repo.writeFile("b.txt", "b")
	sha2 := repo.commit("Second commit")
	repo.addConversation(sha2, "session-1", sampleTranscript(), 2)

	srv := NewServer(0, repo.path)

	t.Run("returns graph nodes", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/graph", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status: want 200, got %d", w.Code)
		}

		var nodes []GraphNode
		decodeJSON(t, w, &nodes)

		if len(nodes) != 2 {
			t.Fatalf("expected 2 nodes, got %d", len(nodes))
		}
	})

	t.Run("marks conversations on nodes", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/graph", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var nodes []GraphNode
		decodeJSON(t, w, &nodes)

		var found bool
		for _, n := range nodes {
			if n.SHA == sha2 {
				found = true
				if !n.HasConversation {
					t.Error("expected HasConversation=true on second commit")
				}
			} else {
				if n.HasConversation {
					t.Error("expected HasConversation=false on first commit")
				}
			}
		}
		if !found {
			t.Error("second commit not in graph nodes")
		}
	})

	t.Run("nodes include parent references", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/graph", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var nodes []GraphNode
		decodeJSON(t, w, &nodes)

		// Find the second commit and check its parent
		for _, n := range nodes {
			if n.SHA == sha2 && len(n.Parents) == 0 {
				t.Error("second commit should have a parent")
			}
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/graph", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("status: want 405, got %d", w.Code)
		}
	})
}

func TestHandleResume(t *testing.T) {
	repo := newTestRepo(t)
	chdir(t, repo.path)

	repo.writeFile("a.txt", "a")
	sha1 := repo.commit("First commit")

	repo.writeFile("b.txt", "b")
	sha2 := repo.commit("Second commit")
	repo.addConversation(sha2, "session-1", sampleTranscript(), 2)

	srv := NewServer(0, repo.path)

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/resume/"+sha1, nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("status: want 405, got %d", w.Code)
		}
	})

	t.Run("missing SHA returns 400", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/resume/", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("status: want 400, got %d", w.Code)
		}
	})

	t.Run("no conversation returns 404", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/resume/"+sha1, nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("status: want 404, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("uncommitted changes returns 409", func(t *testing.T) {
		// Create an untracked file to dirty the working tree
		dirtyFile := filepath.Join(repo.path, "dirty.txt")
		if err := os.WriteFile(dirtyFile, []byte("dirty"), 0644); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(dirtyFile)

		req := httptest.NewRequest("POST", "/api/resume/"+sha2, nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusConflict {
			t.Errorf("status: want 409, got %d: %s", w.Code, w.Body.String())
		}
	})
}

// --- Static file / embedded HTML tests ---

func TestStaticFileServing(t *testing.T) {
	repo := newTestRepo(t)
	srv := NewServer(0, repo.path)

	t.Run("serves index.html at root", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status: want 200, got %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("Content-Type: want text/html, got %q", ct)
		}
	})

	t.Run("HTML contains page title", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		body := w.Body.String()
		if !strings.Contains(body, "Shiftlog - Conversation History") {
			t.Error("page should contain title 'Shiftlog - Conversation History'")
		}
	})

	t.Run("HTML contains key rendering functions", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		body := w.Body.String()
		functions := []string{
			"function escapeHtml(",
			"function formatContent(",
			"function renderConversation(",
			"function renderUserMessage(",
			"function renderAssistantMessage(",
			"function renderSystemMessage(",
			"function renderThinking(",
			"function renderToolUse(",
			"function renderToolResult(",
			"function formatToolInput(",
			"function countDisplayedMessages(",
			"function formatDate(",
			"function showStatus(",
			"function setViewMode(",
			"function updateViewToggle(",
			"function renderCommits(",
		}
		for _, fn := range functions {
			if !strings.Contains(body, fn) {
				t.Errorf("index.html missing function: %s", fn)
			}
		}
	})

	t.Run("HTML contains expected DOM elements", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		body := w.Body.String()
		elements := []string{
			`id="commit-list"`,
			`id="conversation-content"`,
			`id="resume-btn"`,
			`id="view-toggle"`,
			`id="conversation-title"`,
			`id="incremental-info"`,
			`class="commit-panel"`,
			`class="conversation-panel"`,
		}
		for _, elem := range elements {
			if !strings.Contains(body, elem) {
				t.Errorf("index.html missing DOM element: %s", elem)
			}
		}
	})

	t.Run("HTML contains CSS variables for theming", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		body := w.Body.String()
		cssVars := []string{
			"--bg-primary",
			"--text-primary",
			"--accent",
			"--user-bg",
			"--assistant-bg",
		}
		for _, v := range cssVars {
			if !strings.Contains(body, v) {
				t.Errorf("index.html missing CSS variable: %s", v)
			}
		}
	})
}

// --- Branch endpoint tests ---

func TestHandleBranches(t *testing.T) {
	repo := newTestRepo(t)
	chdir(t, repo.path)

	// Create master commits
	repo.writeFile("a.txt", "a")
	sha1 := repo.commit("First commit")
	repo.addConversation(sha1, "s1", sampleTranscript(), 2)

	repo.writeFile("b.txt", "b")
	repo.commit("Second commit")

	// Create feature-a branch
	repo.git("checkout", "-b", "feature-a")
	repo.writeFile("c.txt", "c")
	shaF := repo.commit("Feature A commit")
	repo.addConversation(shaF, "s2", sampleTranscript(), 2)

	// Create feature-b branch from master
	repo.git("checkout", "master")
	repo.git("checkout", "-b", "feature-b")
	repo.writeFile("d.txt", "d")
	repo.commit("Feature B commit")

	// Go back to master for CWD
	repo.git("checkout", "master")

	srv := NewServer(0, repo.path)

	t.Run("lists all branches", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/branches", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status: want 200, got %d: %s", w.Code, w.Body.String())
		}

		var branches []BranchSummary
		decodeJSON(t, w, &branches)

		if len(branches) != 3 {
			t.Fatalf("expected 3 branches, got %d", len(branches))
		}

		names := map[string]bool{}
		for _, b := range branches {
			names[b.Name] = true
		}
		for _, expected := range []string{"master", "feature-a", "feature-b"} {
			if !names[expected] {
				t.Errorf("missing branch %q", expected)
			}
		}
	})

	t.Run("marks current branch", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/branches", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var branches []BranchSummary
		decodeJSON(t, w, &branches)

		var currentCount int
		for _, b := range branches {
			if b.IsCurrent {
				currentCount++
				if b.Name != "master" {
					t.Errorf("expected current branch to be master, got %q", b.Name)
				}
			}
		}
		if currentCount != 1 {
			t.Errorf("expected exactly 1 current branch, got %d", currentCount)
		}
	})

	t.Run("conversation counts are correct", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/branches", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var branches []BranchSummary
		decodeJSON(t, w, &branches)

		for _, b := range branches {
			switch b.Name {
			case "master":
				if b.ConversationCount != 1 {
					t.Errorf("master conversation_count: want 1, got %d", b.ConversationCount)
				}
			case "feature-a":
				// feature-a has its own commit with conv + inherits master's first commit
				if b.ConversationCount < 1 {
					t.Errorf("feature-a conversation_count: want >= 1, got %d", b.ConversationCount)
				}
			case "feature-b":
				// feature-b inherits master's first commit conversation
				if b.ConversationCount < 1 {
					t.Errorf("feature-b conversation_count: want >= 1, got %d", b.ConversationCount)
				}
			}
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/branches", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("status: want 405, got %d", w.Code)
		}
	})
}

func TestHandleBranchGraph(t *testing.T) {
	repo := newTestRepo(t)
	chdir(t, repo.path)

	// Create master commits
	repo.writeFile("a.txt", "a")
	sha1 := repo.commit("First commit")
	repo.addConversation(sha1, "s1", sampleTranscript(), 2)

	repo.writeFile("b.txt", "b")
	repo.commit("Second commit")

	// Create feature branch
	repo.git("checkout", "-b", "feature-x")
	repo.writeFile("c.txt", "c")
	repo.commit("Feature commit")

	repo.git("checkout", "master")

	srv := NewServer(0, repo.path)

	t.Run("returns per-branch graph nodes", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/graph/branches?per_branch=10", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status: want 200, got %d: %s", w.Code, w.Body.String())
		}

		var data BranchGraphData
		decodeJSON(t, w, &data)

		if len(data.Branches) != 2 {
			t.Fatalf("expected 2 branches, got %d", len(data.Branches))
		}

		names := map[string]bool{}
		for _, b := range data.Branches {
			names[b.Name] = true
			if len(b.Nodes) == 0 {
				t.Errorf("branch %q has no nodes", b.Name)
			}
		}
		if !names["master"] || !names["feature-x"] {
			t.Errorf("missing expected branches in graph: %v", names)
		}
	})

	t.Run("marks has_conversation correctly", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/graph/branches", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var data BranchGraphData
		decodeJSON(t, w, &data)

		for _, b := range data.Branches {
			for _, n := range b.Nodes {
				if n.SHA == sha1 && !n.HasConversation {
					t.Errorf("node %s should have has_conversation=true", sha1[:7])
				}
			}
		}
	})

	t.Run("includes fork point for feature branch", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/graph/branches?per_branch=10", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var data BranchGraphData
		decodeJSON(t, w, &data)

		// Find the feature-x branch entry
		var featureEntry *BranchGraphEntry
		for i := range data.Branches {
			if data.Branches[i].Name == "feature-x" {
				featureEntry = &data.Branches[i]
				break
			}
		}
		if featureEntry == nil {
			t.Fatal("feature-x branch not found")
		}
		if featureEntry.ForkPoint == nil {
			t.Fatal("feature-x should have a fork_point")
		}
		if featureEntry.ForkPoint.ParentBranch != "master" {
			t.Errorf("fork parent: want master, got %s", featureEntry.ForkPoint.ParentBranch)
		}
		if featureEntry.ForkPoint.CommitSHA == "" {
			t.Error("fork_point commit_sha should not be empty")
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/graph/branches", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("status: want 405, got %d", w.Code)
		}
	})
}

func TestHandleCommitsWithBranchParam(t *testing.T) {
	repo := newTestRepo(t)
	chdir(t, repo.path)

	// Create master commits
	repo.writeFile("a.txt", "a")
	repo.commit("Master first")

	repo.writeFile("b.txt", "b")
	repo.commit("Master second")

	// Create feature branch with extra commit
	repo.git("checkout", "-b", "feature-y")
	repo.writeFile("c.txt", "c")
	repo.commit("Feature commit")

	repo.git("checkout", "master")

	srv := NewServer(0, repo.path)

	t.Run("branch param returns branch-specific commits", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/commits?branch=feature-y", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status: want 200, got %d: %s", w.Code, w.Body.String())
		}

		var commits []CommitInfo
		decodeJSON(t, w, &commits)

		// feature-y has 3 commits: Feature commit + Master second + Master first
		if len(commits) != 3 {
			t.Fatalf("expected 3 commits on feature-y, got %d", len(commits))
		}
		if commits[0].Message != "Feature commit" {
			t.Errorf("first commit should be 'Feature commit', got %q", commits[0].Message)
		}
	})

	t.Run("no branch param returns HEAD commits", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/commits", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		var commits []CommitInfo
		decodeJSON(t, w, &commits)

		// master has 2 commits
		if len(commits) != 2 {
			t.Fatalf("expected 2 commits on master (HEAD), got %d", len(commits))
		}
	})
}

func TestSingleBranch(t *testing.T) {
	repo := newTestRepo(t)
	chdir(t, repo.path)

	repo.writeFile("a.txt", "a")
	repo.commit("Only commit")

	srv := NewServer(0, repo.path)

	t.Run("branches endpoint works with single branch", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/branches", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status: want 200, got %d", w.Code)
		}

		var branches []BranchSummary
		decodeJSON(t, w, &branches)

		if len(branches) != 1 {
			t.Fatalf("expected 1 branch, got %d", len(branches))
		}
		if branches[0].Name != "master" {
			t.Errorf("expected branch name 'master', got %q", branches[0].Name)
		}
		if !branches[0].IsCurrent {
			t.Error("single branch should be current")
		}
	})
}

// --- Helper function tests for new ref-scoped functions ---

func TestGetCommitListForRef(t *testing.T) {
	repo := newTestRepo(t)

	repo.writeFile("a.txt", "a")
	repo.commit("Master first")

	repo.git("checkout", "-b", "test-branch")
	repo.writeFile("b.txt", "b")
	repo.commit("Branch commit")

	commits, err := getCommitListForRef("test-branch", 10, repo.path)
	if err != nil {
		t.Fatalf("getCommitListForRef: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("expected 2 commits, got %d", len(commits))
	}
	if commits[0].Message != "Branch commit" {
		t.Errorf("first commit: want 'Branch commit', got %q", commits[0].Message)
	}
}

func TestGetGraphDataForRef(t *testing.T) {
	repo := newTestRepo(t)

	repo.writeFile("a.txt", "a")
	sha1 := repo.commit("First")

	repo.git("checkout", "-b", "test-branch")
	repo.writeFile("b.txt", "b")
	sha2 := repo.commit("Second")

	nodes, err := getGraphDataForRef("test-branch", 10, repo.path)
	if err != nil {
		t.Fatalf("getGraphDataForRef: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
	if nodes[0].SHA != sha2 {
		t.Errorf("first node: want %s, got %s", sha2, nodes[0].SHA)
	}
	if len(nodes[0].Parents) != 1 || nodes[0].Parents[0] != sha1 {
		t.Errorf("first node parents: want [%s], got %v", sha1, nodes[0].Parents)
	}
}

// --- Effort metrics tests ---

func TestHandleCommitsWithEffort(t *testing.T) {
	repo := newTestRepo(t)
	chdir(t, repo.path)

	repo.writeFile("a.txt", "a")
	sha1 := repo.commit("First commit")
	repo.addConversationWithEffort(sha1, "session-1", sampleTranscript(), 2, &storage.Effort{
		Turns:        3,
		InputTokens:  12345,
		OutputTokens: 6789,
	})

	srv := NewServer(0, repo.path)

	t.Run("commit list includes effort", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/commits", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status: want 200, got %d", w.Code)
		}

		var commits []CommitInfo
		decodeJSON(t, w, &commits)

		var found *CommitInfo
		for i, c := range commits {
			if c.SHA == sha1 {
				found = &commits[i]
			}
		}
		if found == nil {
			t.Fatal("commit not found")
		}
		if found.Effort == nil {
			t.Fatal("Effort should not be nil")
		}
		if found.Effort.Turns != 3 {
			t.Errorf("Effort.Turns = %d, want 3", found.Effort.Turns)
		}
		if found.Effort.InputTokens != 12345 {
			t.Errorf("Effort.InputTokens = %d, want 12345", found.Effort.InputTokens)
		}
		if found.Effort.OutputTokens != 6789 {
			t.Errorf("Effort.OutputTokens = %d, want 6789", found.Effort.OutputTokens)
		}
	})
}

func TestHandleCommitDetailWithEffort(t *testing.T) {
	repo := newTestRepo(t)
	chdir(t, repo.path)

	repo.writeFile("a.txt", "a")
	sha1 := repo.commit("First commit")
	repo.addConversationWithEffort(sha1, "session-1", sampleTranscript(), 2, &storage.Effort{
		Turns:                    5,
		InputTokens:              50000,
		OutputTokens:             25000,
		CacheCreationInputTokens: 1000,
		CacheReadInputTokens:     2000,
	})

	srv := NewServer(0, repo.path)

	t.Run("detail includes effort", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/commits/"+sha1, nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status: want 200, got %d: %s", w.Code, w.Body.String())
		}

		var resp ConversationResponse
		decodeJSON(t, w, &resp)
		if resp.Effort == nil {
			t.Fatal("Effort should not be nil")
		}
		if resp.Effort.Turns != 5 {
			t.Errorf("Effort.Turns = %d, want 5", resp.Effort.Turns)
		}
		if resp.Effort.InputTokens != 50000 {
			t.Errorf("Effort.InputTokens = %d, want 50000", resp.Effort.InputTokens)
		}
		if resp.Effort.OutputTokens != 25000 {
			t.Errorf("Effort.OutputTokens = %d, want 25000", resp.Effort.OutputTokens)
		}
		if resp.Effort.CacheCreationInputTokens != 1000 {
			t.Errorf("CacheCreationInputTokens = %d, want 1000", resp.Effort.CacheCreationInputTokens)
		}
		if resp.Effort.CacheReadInputTokens != 2000 {
			t.Errorf("CacheReadInputTokens = %d, want 2000", resp.Effort.CacheReadInputTokens)
		}
	})
}

func TestHandleCommitDetailWithoutEffort(t *testing.T) {
	repo := newTestRepo(t)
	chdir(t, repo.path)

	repo.writeFile("a.txt", "a")
	sha1 := repo.commit("First commit")
	repo.addConversation(sha1, "session-1", sampleTranscript(), 2)

	srv := NewServer(0, repo.path)

	t.Run("detail omits effort when nil", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/commits/"+sha1, nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status: want 200, got %d: %s", w.Code, w.Body.String())
		}

		var resp ConversationResponse
		decodeJSON(t, w, &resp)
		if resp.Effort != nil {
			t.Error("Effort should be nil for conversation without effort data")
		}

		// Also check raw JSON doesn't have effort key
		var raw map[string]interface{}
		w2 := httptest.NewRecorder()
		req2 := httptest.NewRequest("GET", "/api/commits/"+sha1, nil)
		srv.mux.ServeHTTP(w2, req2)
		if err := json.NewDecoder(w2.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		if _, exists := raw["effort"]; exists {
			t.Error("JSON should not contain 'effort' key when Effort is nil")
		}
	})
}

func TestHTMLContainsEffortElements(t *testing.T) {
	repo := newTestRepo(t)
	srv := NewServer(0, repo.path)

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	body := w.Body.String()

	elements := []string{
		`id="meta-turns"`,
		`id="meta-input-tokens"`,
		`id="meta-output-tokens"`,
		"function formatTokenCount(",
	}
	for _, elem := range elements {
		if !strings.Contains(body, elem) {
			t.Errorf("index.html missing effort element: %s", elem)
		}
	}
}
