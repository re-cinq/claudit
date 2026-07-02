package cli

import (
	"fmt"
	"os"
	"sync"

	"github.com/re-cinq/shift-log/internal/config"
)

var (
	debugOnce    sync.Once
	debugEnabled bool
)

// LogWarning prints a warning message to stderr with the shiftlog prefix
func LogWarning(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "shiftlog: warning: "+format+"\n", args...)
}

// LogInfo prints an info message to stderr with the shiftlog prefix
func LogInfo(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "shiftlog: "+format+"\n", args...)
}

// LogDebug prints a debug message to stderr if debug logging is enabled
// in .shiftlog/config. The config is read once per process invocation.
func LogDebug(format string, args ...interface{}) {
	initDebug()
	if debugEnabled {
		fmt.Fprintf(os.Stderr, "shiftlog: debug: "+format+"\n", args...)
	}
}

// IsDebugEnabled returns whether debug logging is enabled.
func IsDebugEnabled() bool {
	initDebug()
	return debugEnabled
}

func initDebug() {
	debugOnce.Do(func() {
		cfg, err := config.Read()
		if err == nil {
			debugEnabled = cfg.Debug
		}
	})
}
