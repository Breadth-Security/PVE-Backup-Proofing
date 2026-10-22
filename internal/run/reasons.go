package run

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
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
