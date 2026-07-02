package agent

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

var (
	mu       sync.RWMutex
	registry = make(map[Name]Agent)
)

// Register adds an agent to the global registry.
// This is typically called from an init() function in each agent package.
func Register(a Agent) {
	mu.Lock()
	defer mu.Unlock()
	registry[a.Name()] = a
}

// Get returns the agent with the given name, or an error if not found.
func Get(name Name) (Agent, error) {
	mu.RLock()
	defer mu.RUnlock()
	a, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q (supported: %s)", name, supportedNames())
	}
	return a, nil
}

// supportedNames returns a comma-separated list of registered agent names.
func supportedNames() string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, string(name))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// SupportedNames returns a comma-separated list of registered agent names (exported).
func SupportedNames() string {
	mu.RLock()
	defer mu.RUnlock()
	return supportedNames()
}
