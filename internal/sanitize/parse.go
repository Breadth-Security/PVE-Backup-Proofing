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

// optBool returns the boolean value of option key in a comma-separated
// option string, using PVE's spellings (yes|on|true|1, no|off|false|0, any
// case). ok is false when the option is absent or not a boolean.
func optBool(opts, key string) (val, ok bool) {
	v, found := optValue(opts, key)
	if !found {
		return false, false
	}
	switch strings.ToLower(v) {
	case "1", "yes", "on", "true":
		return true, true
	case "0", "no", "off", "false":
		return false, true
	}
	return false, false
}

// backupEnabled reports whether the option string includes the volume in
// backups: absent means def, and any value that is not a recognised true
// spelling means no.
func backupEnabled(opts string, def bool) bool {
	if _, found := optValue(opts, "backup"); !found {
		return def
	}
	b, _ := optBool(opts, "backup")
	return b
}
