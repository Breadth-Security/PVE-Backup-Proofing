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
	// Addresses are the node's global IP addresses, space separated. They
	// only make the host's message useful when its address for the node
	// reaches another machine.
	Addresses string `json:"addresses,omitempty"`
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
// before the begin marker and after the end marker (also on the markers'
// own lines, such as a shell prompt), lines of any length outside the
// block, CRLF, and surrounding spaces. Errors never include reply content
// (it can hold the token secret).
func ReadReply(r io.Reader) (SetupReply, error) {
	br := bufio.NewReader(r)
	var body strings.Builder
	add := func(s string) error {
		s = strings.Join(strings.Fields(s), "")
		if body.Len()+len(s) > maxReply {
			return errors.New("node reply is too large")
		}
		body.WriteString(s)
		return nil
	}
	state := 0 // 0 before begin, 1 in body, 2 done
	for state < 2 {
		line, long, err := readCappedLine(br, maxReply)
		if err != nil && !errors.Is(err, io.EOF) {
			return SetupReply{}, fmt.Errorf("reading the node reply: %w", err)
		}
		switch {
		case state == 0:
			// A line too long to be the marker line is noise.
			if _, after, ok := strings.Cut(line, ReplyBegin); ok && !long {
				state = 1
				if aerr := add(after); aerr != nil {
					return SetupReply{}, aerr
				}
			}
		case long:
			return SetupReply{}, errors.New("node reply is too large")
		default:
			before, _, end := strings.Cut(line, ReplyEnd)
			if aerr := add(before); aerr != nil {
				return SetupReply{}, aerr
			}
			if end {
				state = 2
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	if state != 2 {
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

// readCappedLine reads one line (without its line ending) of any length,
// keeping at most limit bytes of it; long reports that more was dropped.
// At the end of input it returns what it has with io.EOF.
func readCappedLine(br *bufio.Reader, limit int) (line string, long bool, err error) {
	var sb strings.Builder
	for {
		frag, isPrefix, rerr := br.ReadLine()
		if room := limit - sb.Len(); room > 0 {
			if len(frag) > room {
				frag, long = frag[:room], true
			}
			sb.Write(frag)
		} else if len(frag) > 0 {
			long = true
		}
		if rerr != nil {
			return sb.String(), long, rerr
		}
		if !isPrefix {
			return sb.String(), long, nil
		}
	}
}
