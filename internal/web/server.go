package web

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"os/exec"
	"runtime"
)

//go:embed static
var staticFiles embed.FS

// Server represents the shiftlog web server
type Server struct {
	port    int
	repoDir string
	mux     *http.ServeMux
}

// NewServer creates a new web server instance
func NewServer(port int, repoDir string) *Server {
	s := &Server{
		port:    port,
		repoDir: repoDir,
		mux:     http.NewServeMux(),
	}
	s.setupRoutes()
	return s
}

func (s *Server) setupRoutes() {
	// Static files
	staticFS, _ := fs.Sub(staticFiles, "static")
	s.mux.Handle("/", http.FileServer(http.FS(staticFS)))

	// API endpoints
	s.mux.HandleFunc("/api/commits", s.handleCommits)
	s.mux.HandleFunc("/api/commits/", s.handleCommitDetail)
	s.mux.HandleFunc("/api/graph", s.handleGraph)
	s.mux.HandleFunc("/api/resume/", s.handleResume)
	s.mux.HandleFunc("/api/branches", s.handleBranches)
	s.mux.HandleFunc("/api/graph/branches", s.handleBranchGraph)
}

// Handler returns the HTTP handler for the server.
func (s *Server) Handler() http.Handler { return s.mux }

// Start starts the web server
func (s *Server) Start(openBrowser bool) error {
	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	url := fmt.Sprintf("http://%s", addr)

	fmt.Printf("Starting server at %s\n", url)
	fmt.Println("Press Ctrl+C to stop")

	if openBrowser {
		go openURL(url) //nolint:errcheck // Fire and forget
	}

	return http.ListenAndServe(addr, s.mux)
}

// openURL opens the given URL in the default browser
func openURL(url string) error {
	var cmd string
	var args []string

	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{url}
	case "linux":
		cmd = "xdg-open"
		args = []string{url}
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start", url}
	default:
		return fmt.Errorf("unsupported platform")
	}

	return exec.Command(cmd, args...).Start()
}
