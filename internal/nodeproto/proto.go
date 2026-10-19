// Package nodeproto defines the wire protocol between the PBP daemon and the
// restricted `pbp node` helper that runs on the Proxmox node. The request is
// the SSH command string; refusals and helper failures are signalled with a
// reserved exit code plus a marker on the helper's final stderr line.
package nodeproto

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ExitRefused is the reserved exit code the helper uses for its own failures.
const ExitRefused = 250

// Marker prefixes written by the helper as its final stderr line.
const (
	MarkRefused = "pbp-node: refused: "
	MarkError   = "pbp-node: error: "
	MarkTimeout = "pbp-node: timeout"
)

// MaxRequestLen bounds the size of a request the helper will parse.
const MaxRequestLen = 64 * 1024

// Request is a single helper invocation.
type Request struct {
	Op       string
	VMID     int // exec only
	Argv     []string
	Deadline time.Time
}

var validOps = map[string]bool{
	"exec": true, "bridge": true, "notify": true,
	"config": true, "templates": true, "version": true,
}

// Encode renders r as the SSH command string:
// `<op> --deadline <unix-ms>` plus, for exec, ` <vmid> -- <argv JSON array>`.
func Encode(r Request) string {
	s := fmt.Sprintf("%s --deadline %d", r.Op, r.Deadline.UnixMilli())
	if r.Op == "exec" {
		argv := r.Argv
		if argv == nil {
			argv = []string{}
		}
		b, _ := json.Marshal(argv) // []string cannot fail to marshal
		s += fmt.Sprintf(" %d -- %s", r.VMID, b)
	}
	return s
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

	dl, rest, hasRest := strings.Cut(rest, " ")
	if !plainDecimal(dl, 15) {
		return r, errors.New("invalid deadline")
	}
	ms, err := strconv.ParseInt(dl, 10, 64)
	if err != nil {
		return r, errors.New("invalid deadline")
	}
	r.Deadline = time.UnixMilli(ms)

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

	if !strings.HasPrefix(argv, "[") || json.Unmarshal([]byte(argv), &r.Argv) != nil || r.Argv == nil {
		return Request{}, errors.New("argv is not a JSON string array")
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
// is ExitRefused and the last non-empty stderr line starts with a helper marker.
// kind is "refused", "error" or "timeout".
func HelperFailure(exit int, stderr string) (kind string, reason string, ok bool) {
	if exit != ExitRefused {
		return "", "", false
	}
	s := strings.TrimRight(stderr, " \t\r\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimRight(s, "\r")
	switch {
	case strings.HasPrefix(s, MarkRefused):
		return "refused", strings.TrimSpace(strings.TrimPrefix(s, MarkRefused)), true
	case strings.HasPrefix(s, MarkError):
		return "error", strings.TrimSpace(strings.TrimPrefix(s, MarkError)), true
	case strings.HasPrefix(s, MarkTimeout):
		return "timeout", strings.TrimSpace(strings.TrimPrefix(s, MarkTimeout)), true
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

// NodeConfig is the helper's reply to the config op.
type NodeConfig struct {
	VMIDRange      [2]int        `json:"vmid_range"`
	Bridge         string        `json:"bridge"`
	RequestTimeout time.Duration `json:"request_timeout"`
	Version        string        `json:"version"`
}
