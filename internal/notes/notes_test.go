package notes

import (
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

func TestLine(t *testing.T) {
	at := time.Date(2026, 10, 19, 2, 14, 0, 0, time.UTC)
	want := "PBP PROVEN 2026-10-19 02:14 · agent ok, 3 profiles ok"
	if got := Line(model.Proven, at, "agent ok, 3 profiles ok"); got != want {
		t.Fatal(got)
	}
}

func TestMerge(t *testing.T) {
	l := "PBP FAILED 2026-10-19 02:31 · failed: postgresql.service"
	cases := map[string]struct{ in, want string }{
		"empty":             {"", l},
		"vzdump name":       {"kasm.cic.unifiedlab.lan", l + "\nkasm.cic.unifiedlab.lan"},
		"multiline":         {"db01\nmonthly keep", l + "\ndb01\nmonthly keep"},
		"replace first":     {"PBP PROVEN 2026-10-18 02:10 · ok\ndb01", l + "\ndb01"},
		"replace middle":    {"db01\nPBP PROVEN 2026-10-18 02:10 · ok\nnote", l + "\ndb01\nnote"},
		"user PBP prefix":   {"PBPX backup of x", l + "\nPBPX backup of x"},
		"only pbp line":     {"PBP PROVEN 2026-10-18 02:10 · ok", l},
		"trailing newlines": {"db01\n\n", l + "\ndb01"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := Merge(c.in, l); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestHasProven(t *testing.T) {
	cases := map[string]struct {
		in   string
		want bool
	}{
		"proven after name": {"db01\nPBP PROVEN 2026-10-18 02:10 · ok", true},
		"failed":            {"PBP FAILED 2026-10-18 02:10 · boom", false},
		"empty":             {"", false},
		"provenance":        {"PBP PROVENANCE notes", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := HasProven(c.in); got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}
