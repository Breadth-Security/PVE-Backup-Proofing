package node

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/report"
)

func TestInstallTemplates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "notification-templates", "default")
	if err := InstallTemplates(dir); err != nil {
		t.Fatal(err)
	}
	tmpl := report.Templates()
	if len(tmpl) != 2 {
		t.Fatalf("embedded templates: %d", len(tmpl))
	}
	for name, want := range tmpl {
		p := filepath.Join(dir, name)
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs", name)
		}
		st, _ := os.Stat(p)
		if st.Mode().Perm() != 0o644 {
			t.Errorf("%s mode %v", name, st.Mode().Perm())
		}
	}
	// Overwrites an outdated template.
	p := filepath.Join(dir, "pbp-body.txt.hbs")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InstallTemplates(dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, tmpl["pbp-body.txt.hbs"]) {
		t.Error("outdated template not replaced")
	}
}

func TestInstallTemplatesUnwritable(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InstallTemplates(filepath.Join(f, "sub")); err == nil {
		t.Error("no error")
	}
}
