package nodesetup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfirm(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"y\n", true}, {"yes\n", true}, {"Y\n", true}, {" YES \r\n", true}, {"y", true},
		{"n\n", false}, {"\n", false}, {"", false}, {"yep\n", false}, {"no\n", false}, {"ye\n", false},
	}
	for _, tc := range tests {
		var out strings.Builder
		if got := Confirm(strings.NewReader(tc.in), &out, "Apply these changes?"); got != tc.want {
			t.Errorf("Confirm(%q) = %v, want %v", tc.in, got, tc.want)
		}
		if out.String() != "Apply these changes? [y/N] " {
			t.Errorf("prompt %q", out.String())
		}
	}
}

func TestIsTerminalFalseForFileAndPipe(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "f"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if IsTerminal(int(f.Fd())) {
		t.Error("regular file reported as a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	if IsTerminal(int(r.Fd())) {
		t.Error("pipe reported as a terminal")
	}
}
