package cli

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Spinner displays an animated spinner on stderr while a long operation runs.
// It is TTY-aware: if stderr is not a terminal, no output is produced.
type Spinner struct {
	message string
	stop    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	running bool
}

// NewSpinner creates a new spinner with the given message.
func NewSpinner(message string) *Spinner {
	return &Spinner{
		message: message,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Start begins the spinner animation in a background goroutine.
// Does nothing if stderr is not a TTY.
func (s *Spinner) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return
	}

	if !isTerminal(os.Stderr) {
		close(s.done)
		return
	}

	s.running = true
	go s.run()
}

// Stop halts the spinner and clears its line.
func (s *Spinner) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		<-s.done
		return
	}
	s.running = false
	s.mu.Unlock()

	close(s.stop)
	<-s.done
}

func (s *Spinner) run() {
	defer close(s.done)

	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	i := 0
	for {
		select {
		case <-s.stop:
			// Clear the spinner line
			fmt.Fprintf(os.Stderr, "\r\033[K")
			return
		case <-ticker.C:
			fmt.Fprintf(os.Stderr, "\r%s %s", frames[i%len(frames)], s.message)
			i++
		}
	}
}
