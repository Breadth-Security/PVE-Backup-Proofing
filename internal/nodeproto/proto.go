// Package nodeproto defines the wire protocol between the PBP daemon and the
// restricted `pbp node` helper that runs on the Proxmox node. The request is
// the SSH command string; refusals and helper failures are signalled with a
// reserved exit code plus a marker on the helper's final stderr line.
package nodeproto

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ExitRefused is the reserved exit code the helper uses for its own failures.
const ExitRefused = 250

// Failure kinds carried by a helper marker line.
const (
	KindRefused = "refused"
	KindError   = "error"
	KindTimeout = "timeout"
)

// NewNonce returns a fresh per-request nonce (16 lowercase hex chars). The
// helper echoes it in its marker line so a guest command cannot forge one.
func NewNonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("nodeproto: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func validNonce(n string) bool {
	if len(n) != 16 {
		return false
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func markerPrefix(nonce string) string { return "pbp-node[" + nonce + "]: " }

// Marker renders the helper's final stderr line (without newline) for a
// failure of the given kind. reason is ignored for KindTimeout.
func Marker(kind, nonce, reason string) string {
	if kind == KindTimeout {
		return markerPrefix(nonce) + "timeout"
	}
	return markerPrefix(nonce) + kind + ": " + reason
}

// MaxRequestLen bounds the size of a request the helper will parse.
const MaxRequestLen = 64 * 1024

// Request is a single helper invocation.
type Request struct {
	Op       string
	VMID     int // exec only
	Argv     []string
	Deadline time.Time
	Nonce    string // 16 lowercase hex chars, see NewNonce
}

var validOps = map[string]bool{
	"exec": true, "bridge": true, "notify": true,
	"config": true, "templates": true, "version": true,
}

// Encode renders r as the SSH command string:
// `<op> --deadline <unix-ms> --nonce <hex>` plus, for exec,
// ` <vmid> -- <argv JSON array>`. It fails if r could not be parsed back
// byte-exactly (bad op or nonce, invalid UTF-8 or NUL in argv).
func Encode(r Request) (string, error) {
	if !validOps[r.Op] {
		return "", fmt.Errorf("unknown op %q", r.Op)
	}
	if !validNonce(r.Nonce) {
		return "", errors.New("invalid nonce")
	}
	s := fmt.Sprintf("%s --deadline %d --nonce %s", r.Op, r.Deadline.UnixMilli(), r.Nonce)
	if r.Op == "exec" {
		argv := r.Argv
		if argv == nil {
			argv = []string{}
		}
		for _, a := range argv {
			if !utf8.ValidString(a) {
				return "", errors.New("argv is not valid UTF-8")
			}
			if strings.ContainsRune(a, 0) {
				return "", errors.New("argv contains NUL")
			}
		}
		b, err := json.Marshal(argv)
		if err != nil {
			return "", err
		}
		// json escapes < 0x20 but not DEL; Parse rejects raw DEL.
		js := strings.ReplaceAll(string(b), "\x7f", `\u007f`)
		s += fmt.Sprintf(" %d -- %s", r.VMID, js)
	}
	return s, nil
}

// plainDecimal reports whether s is 1..maxDigits ASCII digits with no leading zero.
func plainDecimal(s string, maxDigits int) bool {
	if len(s) == 0 || len(s) > maxDigits || s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Parse strictly decodes a request string produced by Encode.
func Parse(s string) (Request, error) {
	var r Request
	if len(s) > MaxRequestLen {
		return r, errors.New("request too long")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return r, errors.New("control character in request")
		}
	}
	op, rest, found := strings.Cut(s, " --deadline ")
	if !found || !validOps[op] {
		return r, errors.New("unknown or malformed request")
	}
	r.Op = op

	dl, rest, found := strings.Cut(rest, " --nonce ")
	if !found || !plainDecimal(dl, 15) {
		return r, errors.New("invalid deadline")
	}
	ms, err := strconv.ParseInt(dl, 10, 64)
	if err != nil {
		return r, errors.New("invalid deadline")
	}
	r.Deadline = time.UnixMilli(ms)

	nonce, rest, hasRest := strings.Cut(rest, " ")
	if !validNonce(nonce) {
		return Request{}, errors.New("invalid nonce")
	}
	r.Nonce = nonce

	if op != "exec" {
		if hasRest {
			return r, errors.New("unexpected arguments")
		}
		return r, nil
	}
	if !hasRest {
		return r, errors.New("exec requires a vmid and argv")
	}
	vmid, argv, found := strings.Cut(rest, " -- ")
	if !found || !plainDecimal(vmid, 9) {
		return r, errors.New("invalid vmid or missing argv")
	}
	r.VMID, _ = strconv.Atoi(vmid)

	if !strings.HasPrefix(argv, "[") || !strings.HasSuffix(argv, "]") || !utf8.ValidString(argv) {
		return Request{}, errors.New("argv is not a canonical JSON string array")
	}
	var raw []json.RawMessage
	if json.Unmarshal([]byte(argv), &raw) != nil || raw == nil {
		return Request{}, errors.New("argv is not a JSON string array")
	}
	r.Argv = make([]string, len(raw))
	for i, e := range raw {
		if len(e) == 0 || e[0] != '"' || json.Unmarshal(e, &r.Argv[i]) != nil {
			return Request{}, errors.New("argv element is not a string")
		}
	}
	if len(r.Argv) == 0 {
		return Request{}, errors.New("argv is empty")
	}
	for _, a := range r.Argv {
		if strings.ContainsRune(a, 0) {
			return Request{}, errors.New("argv contains NUL")
		}
	}
	return r, nil
}

// HelperFailure reports whether a finished SSH command was a failure of the
// helper itself rather than a guest result. That is so only when the exit code
// is ExitRefused and the last non-empty stderr line is a helper marker carrying
// this request's nonce. kind is "refused", "error" or "timeout".
func HelperFailure(exit int, stderr, nonce string) (kind string, reason string, ok bool) {
	if exit != ExitRefused || !validNonce(nonce) {
		return "", "", false
	}
	s := strings.TrimRight(stderr, " \t\r\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimRight(s, "\r")
	p := markerPrefix(nonce)
	switch {
	case s == p+"timeout":
		return KindTimeout, "", true
	case strings.HasPrefix(s, p+"refused: "):
		return KindRefused, strings.TrimSpace(strings.TrimPrefix(s, p+"refused: ")), true
	case strings.HasPrefix(s, p+"error: "):
		return KindError, strings.TrimSpace(strings.TrimPrefix(s, p+"error: ")), true
	}
	return "", "", false
}

// versionKey classifies a version string: dev builds, or a MAJOR.MINOR pair.
func versionKey(v string) (key string, dev, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "dev" || strings.HasPrefix(v, "0.0.0~dev") {
		return "", true, true
	}
	if i := strings.IndexAny(v, "~-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return "", false, false
	}
	for _, p := range parts[:2] {
		if _, err := strconv.ParseUint(p, 10, 31); err != nil {
			return "", false, false
		}
	}
	return parts[0] + "." + parts[1], false, true
}

// Compatible reports whether daemon and helper versions share MAJOR.MINOR.
// Dev builds are compatible only with other dev builds; unparseable versions
// are never compatible.
func Compatible(a, b string) bool {
	ka, deva, oka := versionKey(a)
	kb, devb, okb := versionKey(b)
	if !oka || !okb {
		return false
	}
	if deva || devb {
		return deva && devb
	}
	return ka == kb
}

// BridgeState is the helper's reply to the bridge op.
type BridgeState struct {
	Name        string          `json:"name"`
	OVS         bool            `json:"ovs"`
	DisableIPv6 string          `json:"disable_ipv6"`
	Addrs       json.RawMessage `json:"addrs"`
	Members     []string        `json:"members"`
}

// NodeConfig is the helper's reply to the config op. RequestTimeout travels
// as a duration string (e.g. "15m0s").
type NodeConfig struct {
	VMIDRange      [2]int        `json:"vmid_range"`
	Bridge         string        `json:"bridge"`
	RequestTimeout time.Duration `json:"request_timeout"`
	Version        string        `json:"version"`
}

type nodeConfigWire struct {
	VMIDRange      [2]int `json:"vmid_range"`
	Bridge         string `json:"bridge"`
	RequestTimeout string `json:"request_timeout"`
	Version        string `json:"version"`
}

// MarshalJSON encodes RequestTimeout as a duration string.
func (c NodeConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(nodeConfigWire{c.VMIDRange, c.Bridge, c.RequestTimeout.String(), c.Version})
}

// UnmarshalJSON decodes RequestTimeout from a duration string.
func (c *NodeConfig) UnmarshalJSON(b []byte) error {
	var w nodeConfigWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	d, err := time.ParseDuration(w.RequestTimeout)
	if err != nil {
		return fmt.Errorf("request_timeout: %w", err)
	}
	*c = NodeConfig{w.VMIDRange, w.Bridge, d, w.Version}
	return nil
}
