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
	got := mustEncode(t, Request{Op: "exec", VMID: 990001, Argv: []string{"systemctl", "is-system-running", "--wait"}, Timeout: 90 * time.Second, Nonce: nonce})
	want := `exec --timeout-ms 90000 --nonce 0123456789abcdef 990001 -- ["systemctl","is-system-running","--wait"]`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestEncodeErrors(t *testing.T) {
	dl := time.Second
	for name, r := range map[string]Request{
		"bad utf8":         {Op: "exec", VMID: 1, Argv: []string{"a\xff"}, Timeout: dl, Nonce: nonce},
		"nul":              {Op: "exec", VMID: 1, Argv: []string{"a\x00"}, Timeout: dl, Nonce: nonce},
		"bad op":           {Op: "reboot", Timeout: dl, Nonce: nonce},
		"no nonce":         {Op: "bridge", Timeout: dl},
		"bad nonce":        {Op: "bridge", Timeout: dl, Nonce: "ABCDEF0123456789"},
		"no timeout":       {Op: "bridge", Nonce: nonce},
		"negative timeout": {Op: "bridge", Timeout: -time.Second, Nonce: nonce},
		"timeout too long": {Op: "bridge", Timeout: MaxTimeout + time.Millisecond, Nonce: nonce},
	} {
		if _, err := Encode(r); err == nil {
			t.Errorf("%s: Encode accepted", name)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	dl := 15 * time.Minute
	reqs := []Request{
		{Op: "exec", VMID: 990001, Argv: []string{"sh", "-c", "echo 'a b'\nx"}, Timeout: dl, Nonce: nonce},
		{Op: "exec", VMID: 1, Argv: []string{`a"b`, `c\d`, "<>&", "héllo 世界 \U0001F600", "a\tb", "del\x7fx", "\u2028"}, Timeout: dl, Nonce: nonce},
		{Op: "exec", VMID: 999999999, Argv: []string{"x"}, Timeout: MaxTimeout, Nonce: nonce},
		{Op: "bridge", Timeout: dl, Nonce: nonce},
		{Op: "notify", Timeout: dl, Nonce: nonce},
		{Op: "config", Timeout: dl, Nonce: nonce},
		{Op: "templates", Timeout: dl, Nonce: nonce},
		{Op: "version", Timeout: dl, Nonce: nonce},
	}
	for _, r := range reqs {
		s := mustEncode(t, r)
		got, err := Parse(s)
		if err != nil {
			t.Fatalf("%s %q: %v", r.Op, s, err)
		}
		if got.Op != r.Op || got.VMID != r.VMID || got.Nonce != r.Nonce || got.Timeout != r.Timeout || !reflect.DeepEqual(got.Argv, r.Argv) {
			t.Fatalf("%s: got %+v want %+v", r.Op, got, r)
		}
	}
}

func TestParseRejects(t *testing.T) {
	const h = "--timeout-ms 1 --nonce " + nonce
	bad := []string{
		"exec 990001;id",
		"exec --timeout-ms x --nonce " + nonce + ` 1 -- []`,
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
		"exec --timeout-ms -5 --nonce " + nonce + ` 990001 -- ["a"]`,
		"exec --timeout-ms 0 --nonce " + nonce + ` 990001 -- ["a"]`,
		"exec --timeout-ms 01 --nonce " + nonce + ` 990001 -- ["a"]`,
		"exec --timeout-ms 12345678901 --nonce " + nonce + ` 990001 -- ["a"]`,
		"exec --timeout-ms 1 990001 -- [\"a\"]",
		"exec --timeout-ms 1 --nonce ABCDEF0123456789 990001 -- [\"a\"]",
		"exec --timeout-ms 1 --nonce 0123 990001 -- [\"a\"]",
		"exec --deadline 1760900000000 --nonce " + nonce + ` 990001 -- ["a"]`,
		"bridge " + h + " ",
		"bridge " + h + " x",
		"bridge --timeout-ms 1",
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

func TestTimeoutBoundary(t *testing.T) {
	r, err := Parse("bridge --timeout-ms 9999999999 --nonce " + nonce)
	if err != nil || r.Timeout != MaxTimeout {
		t.Errorf("10-digit timeout: %v, %v", r.Timeout, err)
	}
	if _, err := Parse("bridge --timeout-ms 10000000000 --nonce " + nonce); err == nil {
		t.Error("11-digit timeout accepted")
	}
	if r, err := Parse("bridge --timeout-ms 1 --nonce " + nonce); err != nil || r.Timeout != time.Millisecond {
		t.Errorf("1ms timeout: %v, %v", r.Timeout, err)
	}
}

// A timeout that is not a whole number of milliseconds is rounded up, so a
// tiny remaining time still encodes as a valid (1ms) request.
func TestEncodeRoundsTimeoutUp(t *testing.T) {
	for in, want := range map[time.Duration]string{
		time.Nanosecond:               "bridge --timeout-ms 1 --nonce " + nonce,
		1500 * time.Microsecond:       "bridge --timeout-ms 2 --nonce " + nonce,
		time.Second:                   "bridge --timeout-ms 1000 --nonce " + nonce,
		MaxTimeout - time.Microsecond: "bridge --timeout-ms 9999999999 --nonce " + nonce,
	} {
		if got := mustEncode(t, Request{Op: "bridge", Timeout: in, Nonce: nonce}); got != want {
			t.Errorf("%v: got %q want %q", in, got, want)
		}
	}
}

func TestNewNonce(t *testing.T) {
	a, b := NewNonce(), NewNonce()
	if !validNonce(a) || !validNonce(b) || a == b {
		t.Fatalf("bad nonces %q %q", a, b)
	}
}

func TestParseKeepsArgvElementIntact(t *testing.T) {
	r, err := Parse(`exec --timeout-ms 1 --nonce 0123456789abcdef 990001 -- ["sh -c 'id'; reboot"]`)
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
