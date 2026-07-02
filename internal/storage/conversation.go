package storage

import (
	"fmt"
	"strings"

	"github.com/re-cinq/shift-log/internal/agent"
	agentclaude "github.com/re-cinq/shift-log/internal/agent/claude"
	_ "github.com/re-cinq/shift-log/internal/agent/gemini"   // register Gemini agent
	_ "github.com/re-cinq/shift-log/internal/agent/opencode" // register OpenCode agent
	"github.com/re-cinq/shift-log/internal/git"
)

// GetStoredConversation retrieves and parses a stored conversation from a commit's git note.
// Returns nil, nil if no note exists for the commit.
func GetStoredConversation(commitSHA string) (*StoredConversation, error) {
	if !git.HasNote(commitSHA) {
		return nil, nil
	}

	noteContent, err := git.GetNote(commitSHA)
	if err != nil {
		return nil, fmt.Errorf("could not read conversation: %w", err)
	}

	stored, err := UnmarshalStoredConversation(noteContent)
	if err != nil {
		return nil, fmt.Errorf("could not parse conversation: %w", err)
	}

	return stored, nil
}

// ParseTranscript decompresses the stored transcript and parses it into a Transcript.
// Uses the agent-specific parser based on the stored Agent field.
func (sc *StoredConversation) ParseTranscript() (*agent.Transcript, error) {
	data, err := sc.GetTranscript()
	if err != nil {
		return nil, err
	}

	// Look up the agent-specific parser if an agent is specified
	if sc.Agent != "" {
		ag, err := agent.Get(agent.Name(sc.Agent))
		if err == nil {
			return ag.ParseTranscript(strings.NewReader(string(data)))
		}
	}

	// Default to Claude JSONL parser for backward compatibility
	return agentclaude.ParseJSONLTranscript(strings.NewReader(string(data)))
}

// FindParentConversationBoundary finds the most recent parent commit with a conversation
// and returns its SHA and the last entry UUID from that conversation.
// Returns empty strings if no parent conversation is found or session IDs differ.
func FindParentConversationBoundary(commitSHA, currentSessionID string) (parentSHA, lastEntryUUID string) {
	parents, err := git.GetParentCommits(commitSHA)
	if err != nil || len(parents) == 0 {
		return "", ""
	}

	for _, parent := range parents {
		if !git.HasNote(parent) {
			continue
		}

		noteContent, err := git.GetNote(parent)
		if err != nil {
			continue
		}

		stored, err := UnmarshalStoredConversation(noteContent)
		if err != nil {
			continue
		}

		if stored.SessionID != currentSessionID {
			return "", ""
		}

		transcript, err := stored.ParseTranscript()
		if err != nil {
			continue
		}

		lastUUID := transcript.GetLastEntryUUID()
		if lastUUID != "" {
			return parent, lastUUID
		}
	}

	return "", ""
}
