package provider

import (
	"slices"
	"sync"

	"github.com/nwverse/personal-agent-club/internal/protocol"
)

// Known are the providers club knows by name, whether or not this build can reach them. A known provider without a
// registered adapter is listed as unavailable.
var Known = []protocol.Provider{
	{Provider: "cue", Connection: protocol.ConnectionServer},
	{Provider: "grok_bot", Connection: protocol.ConnectionServer},
	{Provider: "groq", Connection: protocol.ConnectionServer},
	{Provider: "muse2api", Connection: protocol.ConnectionServer},
	{Provider: "muse", Connection: protocol.ConnectionBrowser},
	{Provider: "dots", Connection: protocol.ConnectionBrowser},
	{Provider: "claude_duet", Connection: protocol.ConnectionBrowser},
}

// Registry holds the adapters of one club.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
	order    []string
}

// NewRegistry is a registry with adapters.
func NewRegistry(adapters ...Adapter) *Registry {
	r := &Registry{adapters: map[string]Adapter{}}
	for _, a := range adapters {
		r.Register(a)
	}
	return r
}

// Register adds or replaces an adapter.
func (r *Registry) Register(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.adapters[a.Name()]; !ok {
		r.order = append(r.order, a.Name())
	}
	r.adapters[a.Name()] = a
}

// Get is the adapter of a provider.
func (r *Registry) Get(name string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[name]
	return a, ok
}

// IsKnown says whether name is a provider club knows, with or without an adapter.
func (r *Registry) IsKnown(name string) bool {
	if _, ok := r.Get(name); ok {
		return true
	}
	return slices.ContainsFunc(Known, func(p protocol.Provider) bool { return p.Provider == name })
}

// List is every known provider, then every other registered one, each marked available when it has an adapter.
func (r *Registry) List() []protocol.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]protocol.Provider, 0, len(Known)+len(r.order))
	seen := map[string]bool{}
	for _, k := range Known {
		p := k
		if a, ok := r.adapters[k.Provider]; ok {
			p.Available, p.Connection = true, a.Connection()
		}
		out = append(out, p)
		seen[k.Provider] = true
	}
	for _, name := range r.order {
		if seen[name] {
			continue
		}
		out = append(out, protocol.Provider{Provider: name, Connection: r.adapters[name].Connection(), Available: true})
	}
	return out
}

// HasBrowser says whether any registered adapter needs a browser (none in this version).
func (r *Registry) HasBrowser() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, a := range r.adapters {
		if a.Connection() == protocol.ConnectionBrowser {
			return true
		}
	}
	return false
}
