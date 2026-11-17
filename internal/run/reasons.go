package run

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/sanitize"
)

// maxReasonRunes keeps a reason within the backup's notes line (200 runes
// with the verdict and date in front).
const maxReasonRunes = 160

// docsToken is where the docs explain the API token's rights and what to
// grant for the guests the default role cannot proof.
const docsToken = "docs/security.md#guests-pbp-cannot-proof"

var (
	// rePermission is PVE's refusal for a missing privilege.
	rePermission = regexp.MustCompile(`Permission check failed \(([^,()]+), ([^()]+)\)`)
	// reInternal marks text that still carries API internals.
	reInternal = regexp.MustCompile(`(?i)pve api |/api2/|/nodes/|Permission check failed|\b[45]\d\d: `)
)

// permissionDenied reports the privilege a PVE refusal names, if err is
// one: an API error or a task log line ("403 Permission check failed (/,
// Sys.Modify)").
func permissionDenied(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	if m := rePermission.FindStringSubmatch(err.Error()); m != nil {
		return strings.TrimSpace(m[2]), true
	}
	return "", false
}

// lacksPrivilege is the reason for a step PVE refused for lack of priv.
func lacksPrivilege(what, priv string) string {
	return fmt.Sprintf("%s: PBP's token lacks %s (%s)", what, priv, docsToken)
}

// scrub makes PVE text fit for a reason (issue #73): the temporary VMID
// becomes the guest's own label (dropped after "VM"/"CT", which the label
// already says), consecutive repeated segments go, text still carrying API
// internals becomes a pointer to the guest's log, and the result fits the
// notes line.
func scrub(s string, vmid int, label string) string {
	id := strconv.Itoa(vmid)
	s = regexp.MustCompile(`\b(CT|VM) `+id+`\b`).ReplaceAllStringFunc(s, func(m string) string {
		if strings.HasPrefix(m, label[:2]) {
			return label
		}
		return m[:2]
	})
	// "restore … to 990000: …" and volume names such as vm-990000-disk-0
	// read better shortened than with the VMID cut out.
	s = regexp.MustCompile(`\s+to\s+`+id+`\b`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`-`+id+`-`).ReplaceAllString(s, "-…-")
	s = regexp.MustCompile(`\s*\b`+id+`\b`).ReplaceAllString(s, "")
	if reInternal.MatchString(s) {
		return "see the guest's log"
	}
	parts := strings.Split(s, ": ")
	out := parts[:1]
	for _, p := range parts[1:] {
		if p != out[len(out)-1] {
			out = append(out, p)
		}
	}
	return cutRunes(strings.Join(out, ": "), maxReasonRunes)
}

// cutRunes cuts s to n runes, marking the cut.
func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// maxNotedRunes is what a reason may take of the notes line, after "PBP
// BOOTED 2026-10-19 02:14 · ".
const maxNotedRunes = 170

// withSuffix appends a teardown note to reason, shortening reason so the
// note survives the notes line's cut.
func withSuffix(reason, suffix string) string {
	return cutRunes(reason, maxNotedRunes-utf8.RuneCountInString(suffix)) + suffix
}

// rootOnlyReason is the reason for a guest whose copy needs something only
// root@pam can restore or remove (issue #140): the raw lxc.* keys first,
// then the config keys with what they name, at most three of each. A list
// too long for the notes line is cut; the docs pointer always stays.
func rootOnlyReason(raw []string, keys []sanitize.KeyDetail) string {
	var parts []string
	if len(raw) > 0 {
		parts = append(parts, "raw lxc settings ("+firstOf(raw, 3)+")")
	}
	if len(keys) > 0 {
		named := make([]string, len(keys))
		for i, k := range keys {
			named[i] = k.Key
			if k.Detail != "" {
				named[i] += " (" + k.Detail + ")"
			}
		}
		parts = append(parts, firstOf(named, 3))
	}
	tail := ": only root@pam can (" + docsToken + ")"
	return cutRunes("PBP cannot proof "+strings.Join(parts, ", "), maxReasonRunes-utf8.RuneCountInString(tail)) + tail
}

// firstOf joins at most max items and counts the rest ("a, b, c and 2 more").
func firstOf(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(" and %d more", len(items)-max)
}
