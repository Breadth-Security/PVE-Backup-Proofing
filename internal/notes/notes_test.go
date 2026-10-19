package notes

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
)

func TestLine(t *testing.T) {
	at := time.Date(2026, 10, 19, 2, 14, 0, 0, time.UTC)
	want := "PBP PROVEN 2026-10-19 02:14 · agent ok, 3 profiles ok"
	if got := Line(model.Proven, at, "agent ok, 3 profiles ok"); got != want {
		t.Fatal(got)
	}
}

// pbp composes a PBP line on the given day of October 2026 at hh:mm.
func pbp(v model.Verdict, day, hour, min int, reason string) string {
	return Line(v, time.Date(2026, 10, day, hour, min, 0, 0, time.UTC), reason)
}

func TestMerge(t *testing.T) {
	l := pbp(model.Failed, 19, 2, 31, "failed: postgresql.service")
	old := pbp(model.Proven, 18, 2, 10, "ok")
	cases := map[string]struct{ in, want string }{
		"empty":             {"", l},
		"vzdump name":       {"kasm.cic.unifiedlab.lan", l + "\nkasm.cic.unifiedlab.lan"},
		"multiline":         {"db01\nmonthly keep", l + "\ndb01\nmonthly keep"},
		"replace first":     {old + "\ndb01", l + "\ndb01"},
		"replace middle":    {"db01\n" + old + "\nnote", l + "\ndb01\nnote"},
		"user PBP prefix":   {"PBPX backup of x", l + "\nPBPX backup of x"},
		"only pbp line":     {old, l},
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
		"proven after name": {"db01\n" + pbp(model.Proven, 18, 2, 10, "ok"), true},
		"failed":            {pbp(model.Failed, 18, 2, 10, "boom"), false},
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

func TestLineCollapsesNewlines(t *testing.T) {
	at := time.Date(2026, 10, 19, 2, 14, 0, 0, time.UTC)
	want := "PBP ERROR 2026-10-19 02:14 · restore failed: task error: line one line two"
	got := Line(model.Error, at, "restore failed: task error:\r\n  line one\n\nline two\n")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if merged := Merge("db01", got); merged != want+"\ndb01" {
		t.Fatalf("merge %q", merged)
	}
}

func TestLineCapsReason(t *testing.T) {
	at := time.Date(2026, 10, 19, 2, 14, 0, 0, time.UTC)
	prefix := "PBP FAILED 2026-10-19 02:14 · "
	exact := strings.Repeat("ü", maxReason)
	if got := Line(model.Failed, at, exact); got != prefix+exact {
		t.Errorf("reason of exactly %d chars changed: %q", maxReason, got)
	}
	long := strings.Repeat("x", 150) + strings.Repeat("é", 100)
	got := strings.TrimPrefix(Line(model.Failed, at, long), prefix)
	if n := utf8.RuneCountInString(got); n != maxReason {
		t.Errorf("capped reason has %d chars, want %d", n, maxReason)
	}
	if !strings.HasSuffix(got, "…") || !strings.HasPrefix(got, strings.Repeat("x", 150)+"é") {
		t.Errorf("capped reason = %q", got)
	}
	if !utf8.ValidString(got) {
		t.Error("cap split a multi-byte character")
	}
}

// Line formats in the time's own location; the result must still be
// recognised as a PBP line.
func TestLineNonUTC(t *testing.T) {
	at := time.Date(2026, 10, 19, 23, 45, 0, 0, time.FixedZone("x", 5*3600+1800))
	got := Line(model.Proven, at, "ok")
	if want := "PBP PROVEN 2026-10-19 23:45 · ok"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	next := pbp(model.Failed, 20, 0, 0, "x")
	if !HasProven(got) || Merge(got+"\ndb01", next) != next+"\ndb01" {
		t.Fatal("non-UTC line not recognised as a PBP line")
	}
}

func TestMergeDropsStackedPBPLines(t *testing.T) {
	l := pbp(model.Failed, 19, 2, 31, "boom")
	in := strings.Join([]string{
		pbp(model.Proven, 18, 2, 10, "ok"), pbp(model.Error, 17, 2, 10, "e"), "db01", pbp(model.Booted, 16, 2, 10, "b"),
	}, "\n")
	if got, want := Merge(in, l), l+"\ndb01"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// The matcher looks at every line: a PROVEN line after a FAILED one counts.
func TestHasProvenAfterFailedMidNotes(t *testing.T) {
	in := strings.Join([]string{"db01", pbp(model.Failed, 18, 2, 10, "boom"), pbp(model.Proven, 17, 2, 10, "ok"), "monthly"}, "\n")
	if !HasProven(in) {
		t.Fatal("PROVEN line after a FAILED line not found")
	}
}

// Every verdict's line is recognised by the parser built from the same
// constants.
func TestLineRoundTrips(t *testing.T) {
	for _, v := range []model.Verdict{model.Proven, model.Failed, model.Booted, model.Skipped, model.Error} {
		l := pbp(v, 19, 2, 14, "r")
		if got := Merge(l+"\ndb01", "new"); got != "new\ndb01" {
			t.Errorf("%s: line not recognised, merged to %q", v, got)
		}
		if HasProven(l) != (v == model.Proven) {
			t.Errorf("%s: HasProven = %v", v, HasProven(l))
		}
	}
}
