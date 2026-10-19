// Package sanitize neutralises restored guest configs so a restored copy of a
// production guest can never reach a real network or host hardware, and
// reports pieces the backup never contained. All functions are pure.
package sanitize

import "strings"

// ParseConfigText parses PVE guest config text ("key: value" lines). Comment
// and blank lines are skipped, parsing stops at the first "[section]" line
// (snapshots / pending), and raw "lxc.*" lines are collected under key "lxc"
// joined by "\n".
func ParseConfigText(s string) map[string]string {
	out := map[string]string{}
	var raw []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			break
		}
		if strings.HasPrefix(trimmed, "lxc.") {
			raw = append(raw, trimmed)
			continue
		}
		k, v, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if len(raw) > 0 {
		out["lxc"] = strings.Join(raw, "\n")
	}
	return out
}

// optValue returns the value of option key in a comma-separated option string.
func optValue(opts, key string) (string, bool) {
	for _, f := range strings.Split(opts, ",") {
		k, v, _ := strings.Cut(f, "=")
		if k == key {
			return v, true
		}
	}
	return "", false
}
