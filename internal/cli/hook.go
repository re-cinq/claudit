package cli

import (
	"encoding/json"
	"io"
	"os"
)

// ReadHookInput reads JSON from stdin and unmarshals it into the provided struct.
// Returns nil error on success, or returns nil with a warning logged on failure.
// This is designed to be used by hook commands that should fail silently.
func ReadHookInput(v interface{}) error {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		LogWarning("failed to read stdin: %v", err)
		return err
	}

	if err := json.Unmarshal(input, v); err != nil {
		LogWarning("failed to parse hook JSON: %v", err)
		return err
	}

	return nil
}
