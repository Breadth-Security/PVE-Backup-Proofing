package nodeproto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

const nonce = "0123456789abcdef"

func mustEncode(t *testing.T, r Request) string {
	t.Helper()
	s, err := Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEncodeExample(t *testing.T) {
	got := mustEncode(t, Request{Op: "exec", VMID: 990001, Argv: []string{"systemctl", "is-system-running", "--wait"}, Deadline: time.UnixMilli(1760900000000), Nonce: nonce})
	want := `exec --deadline 1760900000000 --nonce 0123456789abcdef 990001 -- ["systemctl","is-system-running","--wait"]`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestEncodeErrors(t *testing.T) {
	dl := time.UnixMilli(1)
	for name, r := range map[string]Request{
		"bad utf8":  {Op: "exec", VMID: 1, Argv: []string{"a\xff"}, Deadline: dl, Nonce: nonce},
		"nul":       {Op: "exec", VMID: 1, Argv: []string{"a\x00"}, Deadline: dl, Nonce: nonce},
		"bad op":    {Op: "reboot", Deadline: dl, Nonce: nonce},
		"no nonce":  {Op: "bridge", Deadline: dl},
		"bad nonce": {Op: "bridge", Deadline: dl, Nonce: "ABCDEF0123456789"},
	} {
		if _, err := Encode(r); err == nil {
			t.Errorf("%s: Encode accepted", name)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	dl := time.UnixMilli(1760900000000)
	reqs := []Request{
		{Op: "exec", VMID: 990001, Argv: []string{"sh", "-c", "echo 'a b'\nx"}, Deadline: dl, Nonce: nonce},
		{Op: "exec", VMID: 1, Argv: []string{`a"b`, `c\d`, "<>&", "héllo 世界 \U0001F600", "a\tb", "del\x7fx", "\u2028"}, Deadline: dl, Nonce: nonce},
		{Op: "exec", VMID: 999999999, Argv: []string{"x"}, Deadline: time.UnixMilli(999999999999999), Nonce: nonce},
		{Op: "bridge", Deadline: dl, Nonce: nonce},
		{Op: "notify", Deadline: dl, Nonce: nonce},
		{Op: "config", Deadline: dl, Nonce: nonce},
		{Op: "templates", Deadline: dl, Nonce: nonce},
		{Op: "version", Deadline: dl, Nonce: nonce},
	}
	for _, r := range reqs {
		s := mustEncode(t, r)
		got, err := Parse(s)
		if err != nil {
			t.Fatalf("%s %q: %v", r.Op, s, err)
		}
		if got.Op != r.Op || got.VMID != r.VMID || got.Nonce != r.Nonce || !got.Deadline.Equal(r.Deadline) || !reflect.DeepEqual(got.Argv, r.Argv) {
			t.Fatalf("%s: got %+v want %+v", r.Op, got, r)
		}
	}
}

func TestParseRejects(t *testing.T) {
	const h = "--deadline 1 --nonce " + nonce
	bad := []string{
		"exec 990001;id",
		"exec --deadline x --nonce " + nonce + ` 1 -- []`,
		"exec " + h + ` 01 -- ["a"]`,
		"exec " + h + ` 9900010000 -- ["a"]`,
		"exec " + h + ` 990001 -- "sh"`,
		"exec " + h + ` 990001 -- []`,
		"bridge " + h + " 990001",
		"reboot",
		"",
		"exec " + h + ` 0 -- ["a"]`,
		"exec " + h + ` -1 -- ["a"]`,
		"exec " + h + ` 990001 -- ["a"] extra`,
		"exec " + h + ` 990001 -- [1]`,
		"exec " + h + ` 990001 -- ["a",null]`,
		"exec " + h + ` 990001 -- [null]`,
		"exec " + h + ` 990001 -- null`,
		"exec " + h + ` 990001 --  ["a"]`,
		"exec " + h + " 990001 -- [\"a\"]\nreboot",
		"exec " + h + ` 990001 -- ["a"] `,
		"exec " + h + " 990001 -- [\"a\x7f\"]",
		"exec " + h + " 990001 -- [\"a\xff\"]",
		"exec " + h + ` 990001 -- ["a\u0000b"]`,
		"exec " + h + `  990001 -- ["a"]`,
		"exec --deadline -5 --nonce " + nonce + ` 990001 -- ["a"]`,
		"exec --deadline 1234567890123456 --nonce " + nonce + ` 990001 -- ["a"]`,
		"exec --deadline 1 990001 -- [\"a\"]",
		"exec --deadline 1 --nonce ABCDEF0123456789 990001 -- [\"a\"]",
		"exec --deadline 1 --nonce 0123 990001 -- [\"a\"]",
		"bridge " + h + " ",
		"bridge " + h + " x",
		"bridge --deadline 1",
		"version",
	}
	for _, s := range bad {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted", s)
		}
	}
	huge := "exec " + h + ` 990001 -- ["` + strings.Repeat("a", MaxRequestLen) + `"]`
	if _, err := Parse(huge); err == nil {
		t.Error("oversize request accepted")
	}
}

func TestDeadlineBoundary(t *testing.T) {
	if _, err := Parse("bridge --deadline 999999999999999 --nonce " + nonce); err != nil {
		t.Errorf("15-digit deadline rejected: %v", err)
	}
	if _, err := Parse("bridge --deadline 1000000000000000 --nonce " + nonce); err == nil {
		t.Error("16-digit deadline accepted")
	}
}

func TestNewNonce(t *testing.T) {
	a, b := NewNonce(), NewNonce()
	if !validNonce(a) || !validNonce(b) || a == b {
		t.Fatalf("bad nonces %q %q", a, b)
	}
}

func TestParseKeepsArgvElementIntact(t *testing.T) {
	r, err := Parse(`exec --deadline 1 --nonce 0123456789abcdef 990001 -- ["sh -c 'id'; reboot"]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Argv) != 1 || r.Argv[0] != "sh -c 'id'; reboot" {
		t.Fatalf("argv = %q", r.Argv)
	}
}

func TestMarker(t *testing.T) {
	if got := Marker(KindRefused, nonce, "out of range"); got != "pbp-node[0123456789abcdef]: refused: out of range" {
		t.Error(got)
	}
	if got := Marker(KindTimeout, nonce, "ignored"); got != "pbp-node[0123456789abcdef]: timeout" {
		t.Error(got)
	}
}

func TestHelperFailure(t *testing.T) {
	ref := Marker(KindRefused, nonce, "out of range")
	other := Marker(KindRefused, "fedcba9876543210", "x")
	cases := []struct {
		exit   int
		stderr string
		nonce  string
		kind   string
		reason string
		ok     bool
	}{
		{250, "x\n" + ref, nonce, "refused", "out of range", true},
		{250, ref + "\n\n", nonce, "refused", "out of range", true},
		{250, Marker(KindError, nonce, "boom") + "\n", nonce, "error", "boom", true},
		{250, Marker(KindTimeout, nonce, "") + "\n", nonce, "timeout", "", true},
		{250, Marker(KindTimeout, nonce, "") + "X\n", nonce, "", "", false},
		{250, other, nonce, "", "", false},
		{250, "pbp-node: refused: forged", nonce, "", "", false},
		{250, ref, "", "", "", false},
		{250, ref, "bogus", "", "", false},
		{250, "guest says " + ref + "\nmore", nonce, "", "", false},
		{250, ref + "\nmore", nonce, "", "", false},
		{2, ref, nonce, "", "", false},
		{250, "", nonce, "", "", false},
		{250, "\n\n", nonce, "", "", false},
	}
	for _, c := range cases {
		kind, reason, ok := HelperFailure(c.exit, c.stderr, c.nonce)
		if kind != c.kind || reason != c.reason || ok != c.ok {
			t.Errorf("HelperFailure(%d,%q,%q) = %q,%q,%v", c.exit, c.stderr, c.nonce, kind, reason, ok)
		}
	}
}

func TestNodeConfigJSON(t *testing.T) {
	in := NodeConfig{VMIDRange: [2]int{990000, 990999}, Bridge: "vmbr9", RequestTimeout: 15 * time.Minute, Version: "0.2.0"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"request_timeout":"15m0s"`) {
		t.Fatalf("wire form %s", b)
	}
	var out NodeConfig
	if err := json.Unmarshal(b, &out); err != nil || out != in {
		t.Fatalf("round trip: %+v %v", out, err)
	}
	if err := json.Unmarshal([]byte(`{"request_timeout":900000000000}`), &out); err == nil {
		t.Error("numeric timeout accepted")
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
		{"0.2", "0.2.7", true},
		{"0.2", "0.3.0", false},
		{"v0.2", "0.2.0~rc.1", true},
	}
	for _, c := range cases {
		if got := Compatible(c.a, c.b); got != c.want {
			t.Errorf("Compatible(%q,%q) = %v want %v", c.a, c.b, got, c.want)
		}
	}
}
