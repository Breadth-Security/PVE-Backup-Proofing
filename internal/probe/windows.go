package probe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
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

// defaultWindowsSettlePoll is how often stopped automatic services are
// re-checked while they may still be starting.
const defaultWindowsSettlePoll = 15 * time.Second

// runWindows lists automatic-start services that are not running. Services
// may still be starting when the agent first answers, so a non-empty list is
// re-polled every WindowsSettlePoll until it is empty or the probe's
// deadline comes; only what is still stopped then is FAILED. Without a
// deadline on ctx it polls once. Per-guest and profile checks are not
// applied to Windows guests in v1.
func runWindows(ctx context.Context, ex Executor, in Input) Outcome {
	every := in.WindowsSettlePoll
	if every <= 0 {
		every = defaultWindowsSettlePoll
	}
	_, settle := ctx.Deadline()
	var last *Outcome
	for {
		out := windowsServices(ctx, ex, in)
		if last != nil && out.Verdict == model.Error && ctx.Err() != nil {
			return *last // the deadline cut a re-poll short
		}
		if out.Verdict != model.Failed || len(out.FailedUnits) == 0 || !settle {
			return out
		}
		last = &out
		if !sleep(ctx, every) {
			return out
		}
	}
}

// windowsServices runs one service query and judges it.
func windowsServices(ctx context.Context, ex Executor, in Input) Outcome {
	res, err := ex.Exec(ctx, []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", windowsScript})
	switch {
	case errors.Is(err, ErrExecBlocked):
		return Outcome{Verdict: model.Booted, Reason: ErrExecBlocked.Error()}
	case errors.Is(err, errProbeEnded):
		return Outcome{Verdict: model.Error, Reason: endedReason(err)}
	case errors.Is(err, errCommandTimeout):
		return Outcome{Verdict: model.Failed, Reason: "check timed out: windows services"}
	case err != nil:
		return Outcome{Verdict: model.Error, Reason: transportReason(err)}
	case res.ExitCode == 127:
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

// sleep waits d or until ctx ends; it reports whether ctx is still live.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
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
