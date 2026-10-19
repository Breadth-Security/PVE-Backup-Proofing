package probe

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

const windowsScript = `Get-CimInstance Win32_Service | Where-Object { $_.StartMode -eq 'Auto' -and $_.State -ne 'Running' -and $_.DelayedAutoStart -ne $true } | ForEach-Object { $_.Name }`

// serviceNames returns trimmed, non-blank, deduplicated lines in order.
func serviceNames(out string) []string {
	var res []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		n := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		res = append(res, n)
	}
	return res
}

// runWindows lists automatic-start services that are not running. Per-guest
// and profile checks are not applied to Windows guests in v1.
func runWindows(ctx context.Context, ex Executor, in Input) Outcome {
	res, err := ex.Exec(ctx, []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", windowsScript})
	if err != nil && !errors.Is(err, ErrExecBlocked) {
		return Outcome{Verdict: model.Error, Reason: transportReason(err)}
	}
	if err != nil || res.ExitCode == 127 {
		return Outcome{Verdict: model.Booted, Reason: ErrExecBlocked.Error()}
	}
	if res.ExitCode != 0 {
		return Outcome{Verdict: model.Failed, Reason: fmt.Sprintf("probe error: powershell exited %d", res.ExitCode)}
	}
	var out Outcome
	for _, n := range serviceNames(res.Stdout) {
		if windowsIgnored(in.Checks.WindowsIgnoreServices, n) {
			out.IgnoredUnits = append(out.IgnoredUnits, n)
		} else {
			out.FailedUnits = append(out.FailedUnits, n)
		}
	}
	if len(out.FailedUnits) > 0 {
		out.Verdict = model.Failed
		out.Reason = "stopped auto services: " + strings.Join(out.FailedUnits, ", ")
		return out
	}
	out.Verdict = model.Proven
	out.Reason = "windows services ok"
	if k := len(out.IgnoredUnits); k > 0 {
		out.Reason += fmt.Sprintf(", %d ignored", k)
	}
	return out
}

// windowsIgnored matches a service name against glob patterns, ignoring case.
func windowsIgnored(patterns []string, name string) bool {
	name = strings.ToLower(name)
	for _, p := range patterns {
		if globMatch(strings.ToLower(p), name) {
			return true
		}
	}
	return false
}
