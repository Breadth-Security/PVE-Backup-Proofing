package probe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

type linuxRun struct {
	ex  *guard
	in  Input
	out Outcome
}

func (r *linuxRun) exec(ctx context.Context, argv ...string) (ExecResult, error) {
	return r.ex.Exec(ctx, argv)
}

// fail finalizes a FAILED outcome and captures the boot journal. The
// journal is read without recording errors: failing to capture it must not
// turn a real FAILED into ERROR.
func (r *linuxRun) fail(ctx context.Context, reason string) Outcome {
	r.out.Verdict = model.Failed
	r.out.Reason = reason
	if res, err := r.ex.run(ctx, []string{"journalctl", "-b", "--no-pager", "-n", "200"}); err == nil {
		r.out.BootLog = res.Stdout
	}
	return r.out
}

// errorOut finalizes an ERROR outcome: PBP could not judge the guest.
func (r *linuxRun) errorOut(reason string) Outcome {
	r.out.Verdict = model.Error
	r.out.Reason = reason
	return r.out
}

func (r *linuxRun) booted(reason string) Outcome {
	r.out.Verdict = model.Booted
	r.out.Reason = reason
	return r.out
}

// execErr maps an Exec error from step name to an outcome: a blocked agent
// is BOOTED, a command timeout fails the check, an ended probe is ERROR, and
// anything else is a transport ERROR.
func (r *linuxRun) execErr(ctx context.Context, name string, err error) Outcome {
	switch {
	case errors.Is(err, ErrExecBlocked):
		return r.booted(ErrExecBlocked.Error())
	case errors.Is(err, errProbeEnded):
		return r.errorOut(endedReason(err))
	case errors.Is(err, errCommandTimeout):
		return r.fail(ctx, "check timed out: "+name)
	}
	return r.errorOut(transportReason(err))
}

// firstFields returns the first whitespace-separated field of each non-empty line, deduplicated.
func firstFields(out string) []string {
	var res []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		for len(f) > 0 && (f[0] == "●" || f[0] == "×" || f[0] == "*") {
			f = f[1:] // status marker some systemd versions print despite --plain
		}
		if len(f) == 0 || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		res = append(res, f[0])
	}
	return res
}

// openRCTypes are the container ostypes whose init is OpenRC.
var openRCTypes = map[string]bool{"alpine": true, "gentoo": true}

func runLinux(ctx context.Context, ex *guard, in Input) Outcome {
	r := &linuxRun{ex: ex, in: in}
	if openRCTypes[in.OSType] {
		return r.runOpenRC(ctx)
	}

	// Wait for boot to settle, bounded by the probe's time rather than the
	// per-command timeout: on the isolated bridge, DHCP and wait-online units
	// can keep a healthy guest "starting" for a minute or two.
	start := time.Now()
	res, err := r.ex.settle(ctx, settleBudget(ctx, in.SettleTimeout), []string{"systemctl", "is-system-running", "--wait"})
	if errors.Is(err, errSettleTimeout) {
		// Not settled in time: judge the state it is in now.
		waited := time.Since(start).Round(time.Second)
		res, err = r.exec(ctx, "systemctl", "is-system-running")
		if err == nil && res.ExitCode != 127 {
			switch strings.TrimSpace(res.Stdout) {
			case "starting", "initializing":
				// Something in boot hung, but that alone doesn't show the
				// backup is bad.
				return r.errorOut("guest still starting after " + waited.String())
			}
		}
	}
	if err != nil {
		return r.execErr(ctx, "system state", err)
	}
	if res.ExitCode == 127 {
		if in.OSType != "" {
			// A container whose ostype is not OpenRC: unknown init.
			return r.booted("init answering, service state unknown")
		}
		// A VM: its init is unknown until asked.
		return r.runOpenRC(ctx)
	}
	switch state := strings.TrimSpace(res.Stdout); state {
	case "running", "degraded":
	default:
		return r.fail(ctx, "system state: "+state)
	}

	res, err = r.exec(ctx, "findmnt", "-n", "-o", "OPTIONS", "/")
	if err != nil {
		return r.execErr(ctx, "root filesystem", err)
	}
	if res.ExitCode == 0 {
		for _, o := range strings.Split(strings.TrimSpace(res.Stdout), ",") {
			if o == "ro" {
				return r.fail(ctx, "root filesystem read-only")
			}
		}
	}

	res, err = r.exec(ctx, "systemctl", "list-units", "--failed", "--plain", "--no-legend", "--no-pager")
	if err != nil {
		return r.execErr(ctx, "failed units", err)
	}
	if res.ExitCode != 0 {
		return r.fail(ctx, fmt.Sprintf("probe error: systemctl list-units exited %d", res.ExitCode))
	}
	if o, bad := r.triage(ctx, firstFields(res.Stdout)); bad {
		return o
	}

	res, err = r.exec(ctx, "systemctl", "list-unit-files", "--state=enabled", "--no-legend", "--plain")
	if err != nil {
		return r.execErr(ctx, "enabled units", err)
	}
	if res.ExitCode != 0 {
		return r.fail(ctx, fmt.Sprintf("probe error: systemctl list-unit-files exited %d", res.ExitCode))
	}
	c := &checker{ex: ex}
	err = c.runProfiles(ctx, in, firstFields(res.Stdout))
	r.out.Checks = c.results
	if err != nil {
		return r.execErr(ctx, "service checks", err)
	}
	return r.finish(ctx)
}

func (r *linuxRun) finish(ctx context.Context) Outcome {
	for _, c := range r.out.Checks {
		if !c.OK && c.Detail == timedOutDetail {
			return r.fail(ctx, "check timed out: "+c.Name)
		}
		if !c.OK {
			return r.fail(ctx, "check failed: "+c.Name)
		}
	}
	r.out.Verdict = model.Proven
	r.out.Reason = fmt.Sprintf("%d checks ok", len(r.out.Checks))
	if k := len(r.out.IgnoredUnits); k > 0 {
		r.out.Reason += fmt.Sprintf(", %d ignored", k)
	}
	return r.out
}

// openRCSettleScript waits until OpenRC has finished booting: the runlevel
// is past sysinit/boot, the openrc process that is changing runlevel has
// exited (until then the default runlevel's services may still show as
// stopped, not yet started), and no service is starting or stopping. It
// exits 127 when rc-status is missing, so the check below decides what that
// means.
const openRCSettleScript = `command -v rc-status >/dev/null 2>&1 || exit 127
while :; do
  rl=$(rc-status -r 2>/dev/null)
  case "$rl" in
    sysinit|boot|"") ;;
    *) if ! pidof openrc >/dev/null 2>&1 &&
          ! rc-status -a 2>/dev/null | grep -qE '\[ *(starting|stopping) *\]'; then
         exit 0
       fi ;;
  esac
  sleep 1
done`

func (r *linuxRun) runOpenRC(ctx context.Context) Outcome {
	// Wait for boot to finish first, bounded by the probe's time like the
	// systemd path: services in the default runlevel, where crashes show
	// up, start only after the boot runlevel, which on the isolated bridge
	// waits on networking. Checking earlier would miss them.
	start := time.Now()
	_, err := r.ex.settle(ctx, settleBudget(ctx, r.in.SettleTimeout), []string{"sh", "-c", openRCSettleScript})
	if errors.Is(err, errSettleTimeout) {
		return r.errorOut("guest still starting after " + time.Since(start).Round(time.Second).String())
	}
	if err != nil {
		return r.execErr(ctx, "openrc boot", err)
	}

	res, err := r.exec(ctx, "rc-status", "--crashed")
	if err != nil {
		return r.execErr(ctx, "openrc services", err)
	}
	if res.ExitCode == 127 {
		return r.booted("init answering, service state unknown")
	}
	// Exit 0 or 1 are both normal; stdout lists the crashed services.
	if res.ExitCode != 0 && res.ExitCode != 1 {
		return r.errorOut(fmt.Sprintf("probe error: rc-status exited %d", res.ExitCode))
	}
	if o, bad := r.triage(ctx, firstFields(res.Stdout)); bad {
		return o
	}
	return r.finish(ctx)
}

// triage splits failed units into ignored and real, traces real ones against
// the missing pieces, and reports a FAILED outcome when any stay unexplained.
// FailedUnits lists only untraced failures; covered ones go to NotCovered.
func (r *linuxRun) triage(ctx context.Context, failed []string) (Outcome, bool) {
	var real []string
	for _, u := range failed {
		if matchAny(r.in.Checks.IgnoreUnits, u) {
			r.out.IgnoredUnits = append(r.out.IgnoredUnits, u)
		} else {
			real = append(real, u)
		}
	}
	if len(real) == 0 {
		return Outcome{}, false
	}
	covered, untraced := traceMissing(ctx, r.ex, real, r.in.Missing)
	r.out.FailedUnits = untraced
	r.out.NotCovered = covered
	if len(untraced) > 0 {
		return r.fail(ctx, "failed: "+strings.Join(untraced, ", ")), true
	}
	return Outcome{}, false
}
