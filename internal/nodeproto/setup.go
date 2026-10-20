package nodeproto

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// SetupArgs are the values `pbp setup` hands to `pbp node setup`.
type SetupArgs struct {
	VMIDRange      [2]int
	Bridge, PubKey string
	DaemonVersion  string
	Storages       []string
	CreateToken    bool
	// RotateToken replaces an existing token; it implies creation, so Line
	// emits --rotate-token instead of --create-token.
	RotateToken bool
}

// shellQuote returns s as a POSIX shell word: unchanged when it is a plain
// word, otherwise single-quoted with embedded quotes written as '\”.
func shellQuote(s string) string {
	plain := s != ""
	for i := 0; i < len(s) && plain; i++ {
		c := s[i]
		plain = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			strings.IndexByte("._,:/=@+-", c) >= 0
	}
	if plain {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Line renders the command the admin pastes into a root shell on the node.
func (a SetupArgs) Line() string {
	parts := []string{
		"pbp", "node", "setup",
		"--vmid-range", strconv.Itoa(a.VMIDRange[0]) + "-" + strconv.Itoa(a.VMIDRange[1]),
		"--bridge", shellQuote(a.Bridge),
		"--pubkey", shellQuote(a.PubKey),
	}
	if len(a.Storages) > 0 {
		parts = append(parts, "--storages", shellQuote(strings.Join(a.Storages, ",")))
	}
	if a.DaemonVersion != "" {
		parts = append(parts, "--daemon-version", shellQuote(a.DaemonVersion))
	}
	switch {
	case a.RotateToken:
		parts = append(parts, "--rotate-token")
	case a.CreateToken:
		parts = append(parts, "--create-token")
	}
	return strings.Join(parts, " ")
}

// SetupReply is what the node prints for the admin to paste back to the host.
type SetupReply struct {
	Format         int    `json:"format"`
	Node           string `json:"node"`
	HelperVersion  string `json:"helper_version"`
	VMIDRange      [2]int `json:"vmid_range"`
	Bridge         string `json:"bridge"`
	PubKey         string `json:"pubkey"`
	HostKey        string `json:"host_key"`
	APIFingerprint string `json:"api_fingerprint"`
	TokenID        string `json:"token_id"`
	TokenSecret    string `json:"token_secret,omitempty"`
}

// Reply block markers.
const (
	ReplyBegin = "-----BEGIN PBP NODE REPLY-----"
	ReplyEnd   = "-----END PBP NODE REPLY-----"
)

const maxReply = 64 << 10

// EncodeReply renders the reply block: markers around base64(JSON) wrapped at
// 64 columns, newline terminated.
func EncodeReply(r SetupReply) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	enc := base64.StdEncoding.EncodeToString(b)
	var sb strings.Builder
	sb.WriteString(ReplyBegin + "\n")
	for len(enc) > 0 {
		n := min(64, len(enc))
		sb.WriteString(enc[:n] + "\n")
		enc = enc[n:]
	}
	sb.WriteString(ReplyEnd + "\n")
	return sb.String(), nil
}

// ReadReply finds the reply block in r, tolerating terminal noise: text
// before the begin marker and after the end marker, CRLF, and surrounding
// spaces. Errors never include reply content (it can hold the token secret).
func ReadReply(r io.Reader) (SetupReply, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), maxReply+1)
	var body strings.Builder
	state := 0 // 0 before begin, 1 in body, 2 done
	for state < 2 && sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case state == 0:
			if line == ReplyBegin {
				state = 1
			}
		case line == ReplyEnd:
			state = 2
		default:
			if body.Len()+len(line) > maxReply {
				return SetupReply{}, errors.New("node reply is too large")
			}
			body.WriteString(strings.Join(strings.Fields(line), ""))
		}
	}
	if state != 2 {
		if err := sc.Err(); err != nil && errors.Is(err, bufio.ErrTooLong) {
			return SetupReply{}, errors.New("node reply is too large")
		}
		if state == 0 {
			return SetupReply{}, errors.New("no node reply found: " + ReplyBegin + " marker missing")
		}
		return SetupReply{}, errors.New("node reply is incomplete: " + ReplyEnd + " marker missing")
	}
	raw, err := base64.StdEncoding.DecodeString(body.String())
	if err != nil {
		return SetupReply{}, errors.New("node reply is not valid base64")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var rep SetupReply
	if err := dec.Decode(&rep); err != nil {
		return SetupReply{}, errors.New("node reply is not valid")
	}
	if _, err := dec.Token(); err != io.EOF {
		return SetupReply{}, errors.New("node reply has trailing data")
	}
	if rep.Format != 1 {
		return SetupReply{}, fmt.Errorf("unsupported node reply format %d", rep.Format)
	}
	return rep, nil
}

// ParsePubKey checks that s holds exactly one ssh-ed25519 public key and
// returns it normalized as "ssh-ed25519 <base64> [comment]".
func ParsePubKey(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("public key is empty")
	}
	pub, comment, opts, rest, err := ssh.ParseAuthorizedKey([]byte(s))
	if err != nil {
		return "", errors.New("not a valid SSH public key")
	}
	if len(opts) > 0 {
		return "", errors.New("public key must not have options")
	}
	if strings.TrimSpace(string(rest)) != "" {
		return "", errors.New("expected exactly one public key")
	}
	if pub.Type() != ssh.KeyAlgoED25519 {
		return "", fmt.Errorf("public key must be ssh-ed25519, not %s", pub.Type())
	}
	out := pub.Type() + " " + base64.StdEncoding.EncodeToString(pub.Marshal())
	if comment = strings.TrimSpace(comment); comment != "" {
		out += " " + comment
	}
	return out, nil
}
