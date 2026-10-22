package probe

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
)

// WindowsServicesScript lists the automatic-start services that are not
// running, one per line: name, exit code, service-specific exit code, and
// the ID and message (on one line) of the newest Service Control Manager
// event since boot about the service (a failure or unexpected stop; matched
// on the event's service-name parameter, the second one for the 7009 and
// 7011 timeouts, equal to its display or key name, so a service whose name
// is part of another's is not confused with it),
// separated by tabs (issue #44), and whether it is delayed-start (code
// review I10). Delayed-start services are listed too; windowsServices
// counts them only when they failed.
const WindowsServicesScript = `$b=(Get-CimInstance Win32_OperatingSystem).LastBootUpTime; ` +
	`$ev=@(Get-WinEvent -FilterHashtable @{LogName='System';ProviderName='Service Control Manager';StartTime=$b} -ErrorAction SilentlyContinue | ` +
	`Where-Object { $_.Id -in 7000,7009,7011,7022,7023,7024,7031,7034 }); ` +
	`Get-CimInstance Win32_Service | Where-Object { $_.StartMode -eq 'Auto' -and $_.State -ne 'Running' } | ` +
	`ForEach-Object { $s=$_; $e=$ev | Where-Object { $i=0; if ($_.Id -in 7009,7011) { $i=1 }; ` +
	`$_.Properties.Count -gt $i -and $_.Properties[$i].Value -in $s.DisplayName,$s.Name } | Select-Object -First 1; ` +
	`$id=''; $m=''; if ($e) { $id=$e.Id; $m=($e.Message -replace '\s+',' ').Trim() }; ` +
	`@($s.Name, $s.ExitCode, $s.ServiceSpecificExitCode, $id, $m, $s.DelayedAutoStart) -join [char]9 }`

// stoppedService is one line of WindowsServicesScript's output. A line with
// only a name (no tabs) carries no stop details.
type stoppedService struct {
	name, exitCode, specificCode, eventID, event string
	details, delayed                             bool
}

// failed reports whether a stopped service actually failed: a Service
// Control Manager failure event, or an error exit code (not 0, and not 1077,
// "not started since boot").
func (s stoppedService) failed() bool {
	return s.eventID != "" || (s.exitCode != "" && s.exitCode != "0" && s.exitCode != "1077")
}

// stoppedServices parses the script's output: trimmed, non-blank lines,
// deduplicated by name, in order.
func stoppedServices(out string) []stoppedService {
	var res []stoppedService
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		if f[0] == "" || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		s := stoppedService{name: f[0], details: len(f) > 1}
		for i, dst := range []*string{&s.exitCode, &s.specificCode, &s.eventID, &s.event} {
			if i+1 < len(f) {
				*dst = f[i+1]
			}
		}
		s.delayed = len(f) > 5 && strings.EqualFold(f[5], "true")
		res = append(res, s)
	}
	return res
}

// why explains a stopped service in one line, from its event and exit codes.
func (s stoppedService) why() string {
	var b strings.Builder
	b.WriteString(s.name + " stopped: ")
	switch {
	case s.eventID != "":
		b.WriteString("event " + s.eventID + ": " + s.event)
	case s.exitCode == "1077":
		b.WriteString("not started since boot")
	default:
		b.WriteString("no Service Control Manager event since boot")
	}
	if s.exitCode == "1066" && s.specificCode != "" && s.specificCode != "0" {
		b.WriteString(" (service-specific exit code " + exitCode(s.specificCode) + ")")
	} else if s.exitCode != "" {
		b.WriteString(" (exit code " + s.exitCode + ")")
	}
	return b.String()
}

// exitCode shows a Windows exit code: in hex from 0x80000000 up, where codes
// are HRESULTs or negative numbers (4294967295 is 0xFFFFFFFF, i.e. -1).
func exitCode(s string) string {
	if n, err := strconv.ParseUint(s, 10, 32); err == nil && n >= 0x80000000 {
		return fmt.Sprintf("0x%08X", n)
	}
	return s
}

// delayedStartWait is how long after the agent first answers the probe keeps
// polling for delayed-start services that have not started yet: Windows
// starts them about two minutes after the other automatic services, and
// one that fails then must not be missed (code review I10). One still not
// started after that is passed.
const delayedStartWait = 4 * time.Minute

// defaultWindowsSettlePoll is how often stopped automatic services are
// re-checked while they may still be starting.
const defaultWindowsSettlePoll = 15 * time.Second

// runWindows lists automatic-start services that are not running. Services
// may still be starting when the agent first answers, so the list is
// re-polled every WindowsSettlePoll while a service is stopped, the query
// failed, or (for up to delayedStartWait) a delayed-start service has not
// started yet, until the probe's deadline comes; only what is still stopped
// then is FAILED. Without a deadline on ctx it polls once. Per-guest and
// profile checks are not applied to Windows guests.
func runWindows(ctx context.Context, ex Executor, in Input) Outcome {
	every := in.WindowsSettlePoll
	if every <= 0 {
		every = defaultWindowsSettlePoll
	}
	_, settle := ctx.Deadline()
	start := time.Now()
	var last *Outcome
	for {
		out, retry, pending := windowsServices(ctx, ex, in)
		if last != nil && out.Verdict == model.Error && ctx.Err() != nil {
			return *last // the deadline cut a re-poll short
		}
		stopped := out.Verdict == model.Failed && len(out.FailedUnits) > 0
		waiting := pending && time.Since(start) < delayedStartWait
		if (!stopped && !retry && !waiting) || !settle {
			return out
		}
		last = &out
		if !sleep(ctx, every) {
			return out
		}
	}
}

// windowsServices runs one service query and judges it. retry is true when
// the query itself failed (it timed out, or PowerShell exited non-zero, as
// it can while the guest is busy after boot): the settle loop asks again,
// and if it still fails at the end the guest is ERROR, since PBP could not
// judge it (code review I11). pending is true when a delayed-start service
// has not started yet.
func windowsServices(ctx context.Context, ex Executor, in Input) (out Outcome, retry, pending bool) {
	res, err := ex.Exec(ctx, []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", WindowsServicesScript})
	switch {
	case errors.Is(err, ErrExecBlocked):
		return Outcome{Verdict: model.Booted, Reason: ErrExecBlocked.Error()}, false, false
	case errors.Is(err, errProbeEnded):
		return Outcome{Verdict: model.Error, Reason: endedReason(err)}, false, false
	case errors.Is(err, errCommandTimeout):
		return Outcome{Verdict: model.Error, Reason: "windows services check timed out"}, true, false
	case err != nil:
		return Outcome{Verdict: model.Error, Reason: transportReason(err)}, false, false
	case res.ExitCode == 127:
		return Outcome{Verdict: model.Booted, Reason: ErrExecBlocked.Error()}, false, false
	}
	if res.ExitCode != 0 {
		return Outcome{Verdict: model.Error, Reason: fmt.Sprintf("probe error: powershell exited %d", res.ExitCode)}, true, false
	}
	var why []string
	for _, s := range stoppedServices(res.Stdout) {
		ignored := windowsIgnored(in.windowsIgnore(), s.name)
		if s.delayed && !s.failed() {
			// Not started yet, or stopped cleanly after its work.
			pending = pending || (s.exitCode == "1077" && !ignored)
			continue
		}
		if ignored {
			out.IgnoredUnits = append(out.IgnoredUnits, s.name)
			continue
		}
		out.FailedUnits = append(out.FailedUnits, s.name)
		if s.details {
			why = append(why, s.why())
		}
	}
	if len(out.FailedUnits) > 0 {
		out.Verdict = model.Failed
		out.Reason = "stopped auto services: " + nameList(out.FailedUnits)
		out.BootLog = strings.Join(why, "\n")
		return out, false, pending
	}
	out.Verdict = model.Proven
	out.Reason = "windows services ok"
	if k := len(out.IgnoredUnits); k > 0 {
		out.Reason += fmt.Sprintf(", %d ignored", k)
	}
	return out, false, pending
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
