package nodeproto

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEncodeExample(t *testing.T) {
	got := Encode(Request{Op: "exec", VMID: 990001, Argv: []string{"systemctl", "is-system-running", "--wait"}, Deadline: time.UnixMilli(1760900000000)})
	want := `exec --deadline 1760900000000 990001 -- ["systemctl","is-system-running","--wait"]`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	dl := time.UnixMilli(1760900000000)
	reqs := []Request{
		{Op: "exec", VMID: 990001, Argv: []string{"sh", "-c", "echo 'a b'\nx"}, Deadline: dl},
		{Op: "bridge", Deadline: dl},
		{Op: "notify", Deadline: dl},
		{Op: "config", Deadline: dl},
		{Op: "templates", Deadline: dl},
		{Op: "version", Deadline: dl},
	}
	for _, r := range reqs {
		s := Encode(r)
		got, err := Parse(s)
		if err != nil {
			t.Fatalf("%s: %v", r.Op, err)
		}
		if got.Op != r.Op || got.VMID != r.VMID || !got.Deadline.Equal(r.Deadline) || !reflect.DeepEqual(got.Argv, r.Argv) {
			t.Fatalf("%s: got %+v want %+v", r.Op, got, r)
		}
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		`exec 990001;id`,
		`exec --deadline x 1 -- []`,
		`exec --deadline 1 01 -- ["a"]`,
		`exec --deadline 1 9900010000 -- ["a"]`,
		`exec --deadline 1 990001 -- "sh"`,
		`exec --deadline 1 990001 -- []`,
		`bridge --deadline 1 990001`,
		`reboot`,
		``,
		`exec --deadline 1 0 -- ["a"]`,
		`exec --deadline 1 -1 -- ["a"]`,
		`exec --deadline 1 990001 -- ["a"] extra`,
		`exec --deadline 1 990001 -- [1]`,
		`exec --deadline 1 990001 -- null`,
		`exec --deadline 1 990001 --  ["a"]`,
		"exec --deadline 1 990001 -- [\"a\"]\nreboot",
		`exec --deadline 1 990001 -- ["a\u0000b"]`,
		`exec --deadline 1  990001 -- ["a"]`,
		`exec --deadline -5 990001 -- ["a"]`,
		`bridge --deadline 1 `,
		`bridge --deadline 1 x`,
		`version`,
	}
	for _, s := range bad {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted", s)
		}
	}
	huge := `exec --deadline 1 990001 -- ["` + strings.Repeat("a", MaxRequestLen) + `"]`
	if _, err := Parse(huge); err == nil {
		t.Error("oversize request accepted")
	}
}

func TestParseKeepsArgvElementIntact(t *testing.T) {
	r, err := Parse(`exec --deadline 1 990001 -- ["sh -c 'id'; reboot"]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Argv) != 1 || r.Argv[0] != "sh -c 'id'; reboot" {
		t.Fatalf("argv = %q", r.Argv)
	}
}

func TestHelperFailure(t *testing.T) {
	cases := []struct {
		exit   int
		stderr string
		kind   string
		reason string
		ok     bool
	}{
		{250, "x\npbp-node: refused: out of range", "refused", "out of range", true},
		{250, "x\npbp-node: refused: out of range\n\n", "refused", "out of range", true},
		{250, "pbp-node: error: boom\n", "error", "boom", true},
		{250, "pbp-node: timeout\n", "timeout", "", true},
		{250, "guest says pbp-node: refused: x\nmore", "", "", false},
		{250, "pbp-node: refused: x\nmore", "", "", false},
		{2, "pbp-node: refused: x", "", "", false},
		{250, "", "", "", false},
		{250, "\n\n", "", "", false},
	}
	for _, c := range cases {
		kind, reason, ok := HelperFailure(c.exit, c.stderr)
		if kind != c.kind || reason != c.reason || ok != c.ok {
			t.Errorf("HelperFailure(%d,%q) = %q,%q,%v", c.exit, c.stderr, kind, reason, ok)
		}
	}
}

func TestCompatible(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.2.0", "0.2.5", true},
		{"0.2.0", "0.3.0", false},
		{"0.2.0~rc.1", "0.2.0", true},
		{"0.2.0-rc.1", "v0.2.0", true},
		{"v0.2.0", "0.2.9", true},
		{"1.2.0", "0.2.0", false},
		{"dev", "dev", true},
		{"dev", "0.2.0", false},
		{"0.2.0", "dev", false},
		{"0.0.0~dev+abc1234", "dev", true},
		{"0.0.0~dev+abc1234", "0.0.0~dev+def5678", true},
		{"0.0.0~dev+abc1234", "0.0.0", false},
		{"0.2.0+build5", "0.2.1", true},
		{"garbage", "garbage", false},
		{"", "", false},
		{"0", "0", false},
	}
	for _, c := range cases {
		if got := Compatible(c.a, c.b); got != c.want {
			t.Errorf("Compatible(%q,%q) = %v want %v", c.a, c.b, got, c.want)
		}
	}
}
