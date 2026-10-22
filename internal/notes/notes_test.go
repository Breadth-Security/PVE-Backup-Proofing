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
	want := "db01 · PBP PROVEN 2026-10-19 02:14 · agent ok, 3 profiles ok"
	if got := Line("db01", model.Proven, at, "agent ok, 3 profiles ok"); got != want {
		t.Fatal(got)
	}
	if got := Line("", model.Proven, at, "ok"); got != "PBP PROVEN 2026-10-19 02:14 · ok" {
		t.Fatal(got)
	}
}

// pbp composes a PBP line for db01 on the given day of October 2026 at
// hh:mm; old composes one in the name-less format of 0.5 to 0.7.
func pbp(v model.Verdict, day, hour, min int, reason string) string {
	return Line("db01", v, time.Date(2026, 10, day, hour, min, 0, 0, time.UTC), reason)
}

func old(v model.Verdict, day, hour, min int, reason string) string {
	return Line("", v, time.Date(2026, 10, day, hour, min, 0, 0, time.UTC), reason)
}

func TestMerge(t *testing.T) {
	l := pbp(model.Failed, 19, 2, 31, "failed: postgresql.service")
	prev := pbp(model.Proven, 18, 2, 10, "ok")
	oldPrev := old(model.Proven, 18, 2, 10, "ok")
	cases := map[string]struct{ in, want string }{
		"empty":                {"", l},
		"vzdump name absorbed": {"db01", l},
		"name with spaces":     {"  db01 ", l},
		"custom template":      {"db01 (105) nightly", l + "\ndb01 (105) nightly"},
		"other first line":     {"kasm.cic.unifiedlab.lan", l + "\nkasm.cic.unifiedlab.lan"},
		"multiline":            {"db01\nmonthly keep", l + "\nmonthly keep"},
		"name not first":       {"pre-upgrade\ndb01", l + "\npre-upgrade\ndb01"},
		"replace first":        {prev + "\nnote", l + "\nnote"},
		"replace old format":   {oldPrev + "\ndb01", l},
		"replace middle":       {"db01\n" + oldPrev + "\nnote", l + "\nnote"},
		"user PBP prefix":      {"PBPX backup of x", l + "\nPBPX backup of x"},
		"only pbp line":        {prev, l},
		"trailing newlines":    {"db01\n\n", l},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := Merge(c.in, l, "db01"); got != c.want {
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
		"proven after name":  {"db01\n" + pbp(model.Proven, 18, 2, 10, "ok"), true},
		"proven, old format": {old(model.Proven, 18, 2, 10, "ok") + "\ndb01", true},
		"name with sep":      {Line("a · b", model.Proven, time.Now(), "ok"), true},
		"failed":             {pbp(model.Failed, 18, 2, 10, "boom"), false},
		"empty":              {"", false},
		"provenance":         {"PBP PROVENANCE notes", false},
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
	want := "db 01 · PBP ERROR 2026-10-19 02:14 · restore failed: task error: line one line two"
	got := Line("db\n01", model.Error, at, "restore failed: task error:\r\n  line one\n\nline two\n")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if merged := Merge("note", got, "db 01"); merged != want+"\nnote" {
		t.Fatalf("merge %q", merged)
	}
}

func TestLineCapsReason(t *testing.T) {
	at := time.Date(2026, 10, 19, 2, 14, 0, 0, time.UTC)
	prefix := "PBP FAILED 2026-10-19 02:14 · "
	exact := strings.Repeat("ü", maxReason)
	if got := Line("", model.Failed, at, exact); got != prefix+exact {
		t.Errorf("reason of exactly %d chars changed: %q", maxReason, got)
	}
	long := strings.Repeat("x", 150) + strings.Repeat("é", 100)
	got := strings.TrimPrefix(Line("", model.Failed, at, long), prefix)
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
	got := Line("db01", model.Proven, at, "ok")
	if want := "db01 · PBP PROVEN 2026-10-19 23:45 · ok"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	next := pbp(model.Failed, 20, 0, 0, "x")
	if !HasProven(got) || Merge(got+"\nnote", next, "db01") != next+"\nnote" {
		t.Fatal("non-UTC line not recognised as a PBP line")
	}
}

func TestMergeDropsStackedPBPLines(t *testing.T) {
	l := pbp(model.Failed, 19, 2, 31, "boom")
	in := strings.Join([]string{
		pbp(model.Proven, 18, 2, 10, "ok"), old(model.Error, 17, 2, 10, "e"), "note", pbp(model.Booted, 16, 2, 10, "b"),
	}, "\n")
	if got, want := Merge(in, l, "db01"), l+"\nnote"; got != want {
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
		if got := Merge(l+"\nnote", "new", "db01"); got != "new\nnote" {
			t.Errorf("%s: line not recognised, merged to %q", v, got)
		}
		if HasProven(l) != (v == model.Proven) {
			t.Errorf("%s: HasProven = %v", v, HasProven(l))
		}
	}
}

// Notes 0.5 to 0.7 wrote (PBP line first, the name below it) get the name
// back in front, keeping the verdict (issue #76); anything else is left.
func TestMigrate(t *testing.T) {
	o := old(model.Proven, 18, 2, 10, "12 checks ok")
	cases := map[string]struct {
		in, want string
		changed  bool
	}{
		"default template": {o + "\ndb01", "db01 · " + o, true},
		"custom template":  {o + "\ndb01 (105) nightly", "db01 · " + o + "\ndb01 (105) nightly", true},
		"only pbp line":    {o, "db01 · " + o, true},
		"new format":       {pbp(model.Proven, 18, 2, 10, "ok") + "\nnote", pbp(model.Proven, 18, 2, 10, "ok") + "\nnote", false},
		"no pbp line":      {"db01", "db01", false},
		"old line second":  {"db01\n" + o, "db01\n" + o, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, changed := Migrate(c.in, "db01")
			if got != c.want || changed != c.changed {
				t.Fatalf("got %q, %v; want %q, %v", got, changed, c.want, c.changed)
			}
		})
	}
	if _, changed := Migrate(o, ""); changed {
		t.Error("migrated without a name")
	}
}

// A name longer than maxName is cut in the line, but a first line holding
// the whole name is still recognised as the name and not repeated.
func TestMergeAbsorbsLongName(t *testing.T) {
	name := strings.Repeat("host-", 15) + "example.lan"
	l := Line(name, model.Proven, time.Date(2026, 10, 19, 2, 14, 0, 0, time.UTC), "ok")
	if got := Merge(name+"\nnote", l, name); got != l+"\nnote" {
		t.Fatalf("got %q", got)
	}
	o := old(model.Proven, 18, 2, 10, "ok")
	got, _ := Migrate(o+"\n"+name, name)
	if strings.Contains(got, "\n") {
		t.Fatalf("name repeated: %q", got)
	}
}

func TestMigrateCRLF(t *testing.T) {
	o := old(model.Proven, 18, 2, 10, "ok")
	got, changed := Migrate(o+"\r\ndb01\r\nnote", "db01")
	if !changed || got != "db01 · "+o+"\nnote" {
		t.Fatalf("got %q, %v", got, changed)
	}
}
