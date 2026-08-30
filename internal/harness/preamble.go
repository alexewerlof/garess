package harness

import "sync"

// Preamble is a thread-safe holder for the current system-instruction text
// (AGENTS.md/SYSTEM.md + skills). Agents resolve it per run via
// InstructionProvider, so updating it (e.g. /agents reload) takes effect on
// the next run without rebuilding the agent.
type Preamble struct {
	mu   sync.RWMutex
	text string
}

// NewPreamble returns an empty preamble holder.
func NewPreamble() *Preamble { return &Preamble{} }

// Set replaces the instruction text.
func (p *Preamble) Set(text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.text = text
}

// Get returns the current instruction text.
func (p *Preamble) Get() (string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.text, nil
}
