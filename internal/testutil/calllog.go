// Package testutil holds helpers shared by the test doubles.
package testutil

import (
	"slices"
	"sync"
)

// CallLog is a mutex-safe, append-only record of the calls a fake received.
// The zero value is ready to use.
type CallLog struct {
	mu    sync.Mutex
	calls []string
}

// Add appends one call.
func (l *CallLog) Add(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

// Snapshot returns a copy of the calls recorded so far, in order.
func (l *CallLog) Snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// Reset forgets every recorded call.
func (l *CallLog) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = nil
}
