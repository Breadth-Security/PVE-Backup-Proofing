package report

import (
	"strings"
	"testing"
)

func TestTemplates(t *testing.T) {
	got := Templates()
	if len(got) != 2 || !strings.Contains(string(got["pbp-subject.txt.hbs"]), "{{") || len(got["pbp-body.txt.hbs"]) == 0 {
		t.Fatalf("got %d templates: %v", len(got), got)
	}
	// Callers get their own copy.
	got["pbp-body.txt.hbs"][0] = 0
	if Templates()["pbp-body.txt.hbs"][0] == 0 {
		t.Error("Templates returned shared backing memory")
	}
}
