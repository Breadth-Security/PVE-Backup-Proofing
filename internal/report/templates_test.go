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

// Every list the daemon sends has a section in the body (issue #101: a key
// the node helper accepts but no template shows would drop guests silently).
func TestBodyShowsEveryList(t *testing.T) {
	body := string(Templates()["pbp-body.txt.hbs"])
	for _, k := range DataKeys {
		if _, isList := TemplateData(sectionsSummary(), "pve1")[k].([]string); !isList {
			continue
		}
		if !strings.Contains(body, "{{#each "+k+"}}") {
			t.Errorf("body has no {{#each %s}}", k)
		}
	}
}
