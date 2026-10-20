package nodeproto

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// shellSplit is a minimal POSIX word splitter (single quotes, double quotes,
// backslash) used to check that Line survives a paste into sh.
func shellSplit(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	in := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ', '\t', '\n':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		case '\'':
			in = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, fmt.Errorf("unterminated single quote")
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case '"':
			in = true
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, fmt.Errorf("unterminated double quote")
			}
		case '\\':
			in = true
			i++
			if i < len(s) {
				cur.WriteByte(s[i])
			}
		default:
			in = true
			cur.WriteByte(c)
		}
	}
	if in {
		words = append(words, cur.String())
	}
	return words, nil
}

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

func TestSetupLineRoundTrip(t *testing.T) {
	a := SetupArgs{
		VMIDRange: [2]int{990000, 990049}, Bridge: "vmbr99",
		PubKey: testKey + " pbp it's me", DaemonVersion: "0.3.0",
		Storages: []string{"a", "b"}, CreateToken: true,
	}
	words, err := shellSplit(a.Line())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pbp", "node", "setup", "--vmid-range", "990000-990049", "--bridge", "vmbr99",
		"--pubkey", testKey + " pbp it's me", "--storages", "a,b", "--daemon-version", "0.3.0", "--create-token"}
	if !reflect.DeepEqual(words, want) {
		t.Fatalf("got %q\nwant %q", words, want)
	}
	if !strings.Contains(a.Line(), `'\''`) {
		t.Errorf("embedded quote not escaped POSIX style: %s", a.Line())
	}

	b := SetupArgs{VMIDRange: [2]int{1, 2}, Bridge: "vmbr0", PubKey: testKey}
	words, _ = shellSplit(b.Line())
	want = []string{"pbp", "node", "setup", "--vmid-range", "1-2", "--bridge", "vmbr0", "--pubkey", testKey}
	if !reflect.DeepEqual(words, want) {
		t.Fatalf("minimal: got %q", words)
	}

	c := a
	c.CreateToken, c.RotateToken = true, true
	words, _ = shellSplit(c.Line())
	if n := strings.Count(strings.Join(words, " "), "-token"); n != 1 || words[len(words)-1] != "--rotate-token" {
		t.Fatalf("rotate: got %q", words)
	}
}

func TestSetupLineQuotesHostile(t *testing.T) {
	a := SetupArgs{VMIDRange: [2]int{1, 2}, Bridge: "vm br;$(x)", PubKey: testKey + " `id` \"q\"", Storages: []string{"a b", "c"}}
	words, err := shellSplit(a.Line())
	if err != nil {
		t.Fatal(err)
	}
	if words[6] != "vm br;$(x)" || words[8] != testKey+" `id` \"q\"" || words[10] != "a b,c" {
		t.Fatalf("got %q", words)
	}
}

func sampleReply() SetupReply {
	return SetupReply{
		Format: 1, Node: "pve1", HelperVersion: "0.3.0", VMIDRange: [2]int{990000, 990049},
		Bridge: "vmbr99", PubKey: testKey, HostKey: testKey,
		APIFingerprint: "AB:CD:EF", TokenID: "pbp@pve!proof", TokenSecret: "s3cret",
	}
}

func TestReplyRoundTrip(t *testing.T) {
	want := sampleReply()
	s, err := EncodeReply(want)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if lines[0] != ReplyBegin || lines[len(lines)-1] != ReplyEnd {
		t.Fatalf("markers: %q", s)
	}
	for _, l := range lines[1 : len(lines)-1] {
		if len(l) > 64 {
			t.Fatalf("line too long: %d", len(l))
		}
	}
	got, err := ReadReply(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v", got)
	}
	// No token: secret omitted.
	want.TokenSecret = ""
	s, _ = EncodeReply(want)
	got, err = ReadReply(strings.NewReader(s))
	if err != nil || got != want {
		t.Fatalf("no-secret: %+v %v", got, err)
	}
}

func TestReadReplyTolerant(t *testing.T) {
	s, _ := EncodeReply(sampleReply())
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = "   " + lines[i] + "  "
	}
	noisy := "root@pve1:~# pbp node setup ...\r\nsome output\r\n" + strings.Join(lines, "\r\n") + "\r\nroot@pve1:~# \r\n"
	got, err := ReadReply(strings.NewReader(noisy))
	if err != nil {
		t.Fatal(err)
	}
	if got != sampleReply() {
		t.Fatalf("got %+v", got)
	}
}

func TestReadReplyRejects(t *testing.T) {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return ReplyBegin + "\n" + base64.StdEncoding.EncodeToString(b) + "\n" + ReplyEnd + "\n"
	}
	good, _ := EncodeReply(sampleReply())
	r2 := sampleReply()
	r2.Format = 2
	var m map[string]any
	b, _ := json.Marshal(sampleReply())
	_ = json.Unmarshal(b, &m)
	m["extra"] = 1
	big := ReplyBegin + "\n" + strings.Repeat("QUJD\n", 20000) + ReplyEnd + "\n"
	for name, in := range map[string]string{
		"empty":         "",
		"no begin":      strings.TrimPrefix(good, ReplyBegin),
		"missing end":   strings.TrimSuffix(strings.TrimRight(good, "\n"), ReplyEnd),
		"bad base64":    ReplyBegin + "\n!!!notbase64\n" + ReplyEnd + "\n",
		"not json":      ReplyBegin + "\n" + base64.StdEncoding.EncodeToString([]byte("hi")) + "\n" + ReplyEnd + "\n",
		"unknown field": enc(m),
		"format 2":      enc(r2),
		"trailing json": ReplyBegin + "\n" + base64.StdEncoding.EncodeToString(append(b, []byte(" {}")...)) + "\n" + ReplyEnd + "\n",
		"oversize":      big,
	} {
		_, err := ReadReply(strings.NewReader(in))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: error leaks secret: %v", name, err)
		}
	}
}

func TestParsePubKey(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"plain":   {testKey, testKey},
		"lf":      {testKey + "\n", testKey},
		"crlf":    {"  " + testKey + "\r\n", testKey},
		"comment": {testKey + " pbp@host", testKey + " pbp@host"},
	} {
		got, err := ParsePubKey(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v", name, got, err)
		}
	}
	rsa := "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQC7 x"
	for name, in := range map[string]string{
		"rsa":     rsa,
		"two":     testKey + "\n" + testKey + "\n",
		"garbage": "not a key",
		"empty":   "  \n",
		"trail":   testKey + " c\nsomething",
	} {
		if _, err := ParsePubKey(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Paste noise the scanner used to choke on: a very long line before the
// block (a terminal redraw), and prompts on the markers' own lines.
func TestReadReplyLongNoiseAndPrompts(t *testing.T) {
	s, _ := EncodeReply(sampleReply())
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	lines[0] = "root@pve1:~# " + lines[0]
	lines[len(lines)-1] = lines[len(lines)-1] + " root@pve1:~# "
	in := strings.Repeat("\x1b[K=", 300<<10) + "\n" + strings.Join(lines, "\n")
	got, err := ReadReply(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if got != sampleReply() {
		t.Fatalf("got %+v", got)
	}
}

// A body line over the cap is still refused, and a block with no end at
// the end of input (no trailing newline) is incomplete.
func TestReadReplyLongBodyAndNoNewline(t *testing.T) {
	long := ReplyBegin + "\n" + strings.Repeat("A", maxReply+10) + "\n" + ReplyEnd + "\n"
	if _, err := ReadReply(strings.NewReader(long)); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("long body line: %v, want too large", err)
	}
	s, _ := EncodeReply(sampleReply())
	if _, err := ReadReply(strings.NewReader(strings.TrimRight(s, "\n"))); err != nil {
		t.Errorf("no trailing newline: %v", err)
	}
	cut := s[:strings.Index(s, ReplyEnd)]
	if _, err := ReadReply(strings.NewReader(cut)); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("no end marker: %v, want incomplete", err)
	}
}

type failingReader struct{ data string }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.data == "" {
		return 0, errors.New("input/output error")
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

// A read error is reported as one, not as a missing marker, and carries no
// reply content.
func TestReadReplyIOError(t *testing.T) {
	s, _ := EncodeReply(sampleReply())
	half := s[:len(s)/2]
	_, err := ReadReply(&failingReader{data: half})
	if err == nil || !strings.Contains(err.Error(), "reading the node reply: input/output error") {
		t.Fatalf("err %v, want the read error", err)
	}
	if strings.Contains(err.Error(), half[len(ReplyBegin)+1:len(ReplyBegin)+20]) {
		t.Errorf("error carries reply content: %v", err)
	}
}
