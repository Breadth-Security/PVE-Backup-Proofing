package testutil

import (
	"slices"
	"sync"
	"testing"
)

func TestCallLog(t *testing.T) {
	var l CallLog
	if got := l.Snapshot(); len(got) != 0 {
		t.Fatalf("zero value not empty: %q", got)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); l.Add("c"); _ = l.Snapshot() }()
	}
	wg.Wait()
	snap := l.Snapshot()
	if len(snap) != 8 {
		t.Fatalf("got %d calls", len(snap))
	}
	snap[0] = "mutated"
	if l.Snapshot()[0] != "c" {
		t.Error("Snapshot shares storage")
	}
	l.Reset()
	if got := l.Snapshot(); len(got) != 0 || slices.Contains(got, "c") {
		t.Errorf("after Reset: %q", got)
	}
}
