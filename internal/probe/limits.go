package probe

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Limits on what a guest can put in an Outcome. A broken or hostile guest
// controls unit names, command output and its journal; without these a
// reason could fill the notification past the helper's 1 MiB limit, and the
// whole run's notification would be lost (code review I14).
const (
	maxListed    = 10        // names spelled out in a reason
	maxReason    = 1 << 10   // bytes
	maxDetail    = 512       // bytes, per check
	maxBootLog   = 256 << 10 // bytes; the newest part is kept
	clipEllipsis = "…"
)

// nameList joins names for a reason, spelling out at most maxListed.
func nameList(names []string) string {
	if len(names) <= maxListed {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, … and %d more", strings.Join(names[:maxListed], ", "), len(names)-maxListed)
}

// clip cuts s to at most n bytes on a rune boundary, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	n -= len(clipEllipsis)
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + clipEllipsis
}

// clipHead keeps the last n bytes of s, starting at a line if it can.
func clipHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return clipEllipsis + "\n" + s[i+1:]
	}
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return clipEllipsis + s
}

// bound applies the limits to a finished Outcome.
func bound(o Outcome) Outcome {
	o.Reason = clip(o.Reason, maxReason)
	o.BootLog = clipHead(o.BootLog, maxBootLog)
	return o
}
