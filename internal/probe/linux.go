package probe

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
)

type linuxRun struct {
	ex  *guard
	in  Input
	out Outcome
	// nesting are failed units that fail only for lack of nesting.
	nesting []string
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
	if res, err := r.ex.run(ctx, []string{"journalctl", "-b", "--no-pager", "-n", "200"}); err == nil && res.ExitCode == 0 {
		r.out.BootLog = res.Stdout
	}
	return r.out
}

// crashLoopRestarts is how many restarts make a service in auto-restart a
// crash loop rather than a slow start.
const crashLoopRestarts = 3

// crashLooping lists services that keep crashing under Restart=: they sit in
// the auto-restart sub-state and never become "failed" (code review I7). One
// counts with crashLoopRestarts restarts or more, or when NRestarts is
// unknown (systemd < 235). Best effort: if the list cannot be read, none.
func (r *linuxRun) crashLooping(ctx context.Context) []string {
	res, err := r.exec(ctx, "systemctl", "list-units", "--type=service", "--state=auto-restart", "--plain", "--no-legend", "--no-pager")
	if err != nil || res.ExitCode != 0 {
		return nil
	}
	var out []string
	for _, u := range firstFields(res.Stdout) {
		n, err := r.exec(ctx, "systemctl", "show", "-p", "NRestarts", "--value", "--", u)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(n.Stdout)
		if k, perr := strconv.Atoi(v); v == "" || n.ExitCode != 0 || (perr == nil && k >= crashLoopRestarts) {
			out = append(out, u)
		}
	}
	return out
}

// openRCFailed lists the services `rc-status --format ini` reports as
// failed: crashed in any section, or stopped in a runlevel section (a
// service of the runlevel that failed to start, which --crashed misses;
// seen on Alpine 3.24). The "Dynamic" sections (dependencies, hotplugged,
// manual) are not the runlevel's own (code review I9).
func openRCFailed(ini string) []string {
	var failed []string
	runlevel := false
	for _, line := range strings.Split(ini, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			runlevel = !strings.HasPrefix(line, "[Dynamic")
			continue
		}
		name, state, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		name, state = strings.TrimSpace(name), strings.TrimSpace(state)
		if (state == "crashed" || (runlevel && state == "stopped")) && !slices.Contains(failed, name) {
			failed = append(failed, name)
		}
	}
	return failed
}

// rootOptions returns the mount options of the root filesystem: from
// findmnt, or, when findmnt is missing or fails, from /proc/self/mounts (the
// last "/" entry, which an overmount puts last). nil means neither could be
// read (code review I8).
func (r *linuxRun) rootOptions(ctx context.Context) ([]string, error) {
	res, err := r.exec(ctx, "findmnt", "-n", "-o", "OPTIONS", "/")
	if err != nil {
		return nil, err
	}
	if s := strings.TrimSpace(res.Stdout); res.ExitCode == 0 && s != "" {
		return strings.Split(s, ","), nil
	}
	res, err = r.exec(ctx, "cat", "/proc/self/mounts")
	if err != nil || res.ExitCode != 0 {
		return nil, err
	}
	var opts []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if f := strings.Fields(line); len(f) >= 4 && f[1] == "/" {
			opts = strings.Split(f[3], ",")
		}
	}
	return opts, nil
}

// waitUnsupported reports whether `systemctl is-system-running --wait` was
// refused as an unknown option (systemd older than 240).
func waitUnsupported(res ExecResult) bool {
	if res.ExitCode == 0 || strings.TrimSpace(res.Stdout) != "" {
		return false
	}
	e := strings.ToLower(res.Stderr)
	return strings.Contains(e, "unrecognized option") || strings.Contains(e, "unknown option")
}

// pollSettle polls `systemctl is-system-running` until the state is past
// starting/initializing, or until deadline, when it returns errSettleTimeout
// like guard.settle does. It replaces --wait on old systemd.
func (r *linuxRun) pollSettle(ctx context.Context, deadline time.Time) (ExecResult, error) {
	every := min(5*time.Second, max(time.Until(deadline)/20, 10*time.Millisecond))
	for {
		res, err := r.exec(ctx, "systemctl", "is-system-running")
		if err != nil {
			return res, err
		}
		switch strings.TrimSpace(res.Stdout) {
		case "starting", "initializing":
		default:
			return res, nil
		}
		if !time.Now().Before(deadline) || !sleep(ctx, every) {
			return res, errSettleTimeout
		}
	}
}

// pendingJobs returns " (waiting on: <units>)" naming the units of the jobs
// systemd still has queued, and keeps the job list as the boot log. Like the
// journal in fail, it is read without recording errors; with no answer it
// returns "".
func (r *linuxRun) pendingJobs(ctx context.Context) string {
	res, err := r.ex.run(ctx, []string{"systemctl", "list-jobs", "--no-legend", "--plain", "--no-pager"})
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	r.out.BootLog = res.Stdout
	var units []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		// "<id> <unit> <type> <state>"
		if f := strings.Fields(line); len(f) >= 2 && !slices.Contains(units, f[1]) {
			units = append(units, f[1])
		}
	}
	if len(units) == 0 {
		return ""
	}
	return " (waiting on: " + strings.Join(units, ", ") + ")"
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
	budget := settleBudget(ctx, in.SettleTimeout)
	res, err := r.ex.settle(ctx, budget, []string{"systemctl", "is-system-running", "--wait"})
	if err == nil && waitUnsupported(res) {
		// systemd < 240 has no --wait (code review I12): poll instead.
		res, err = r.pollSettle(ctx, start.Add(budget))
	}
	if errors.Is(err, errSettleTimeout) {
		// Not settled in time: judge the state it is in now.
		waited := time.Since(start).Round(time.Second)
		res, err = r.exec(ctx, "systemctl", "is-system-running")
		if err == nil && res.ExitCode != 127 {
			switch strings.TrimSpace(res.Stdout) {
			case "starting", "initializing":
				// Something in boot hung, but that alone doesn't show the
				// backup is bad. Name what it waits on.
				return r.errorOut("guest still starting after " + waited.String() + r.pendingJobs(ctx))
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

	opts, err := r.rootOptions(ctx)
	if err != nil {
		return r.execErr(ctx, "root filesystem", err)
	}
	if opts == nil {
		// Never pass a guest whose root PBP could not check (code review I8).
		return r.errorOut("cannot read the root filesystem's mount options")
	}
	if slices.Contains(opts, "ro") {
		return r.fail(ctx, "root filesystem read-only")
	}

	res, err = r.exec(ctx, "systemctl", "list-units", "--failed", "--plain", "--no-legend", "--no-pager")
	if err != nil {
		return r.execErr(ctx, "failed units", err)
	}
	if res.ExitCode != 0 {
		return r.fail(ctx, fmt.Sprintf("probe error: systemctl list-units exited %d", res.ExitCode))
	}
	failed := firstFields(res.Stdout)
	for _, u := range r.crashLooping(ctx) {
		if !slices.Contains(failed, u) {
			failed = append(failed, u)
		}
	}
	if o, bad := r.triage(ctx, failed); bad {
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
	if n := len(r.nesting); n > 0 {
		// The production guest has the same failures: they say nothing
		// about the backup, but the guest cannot be proven either.
		return r.booted(fmt.Sprintf("container without nesting: %d units cannot start sandboxed (%s); enable nesting=1",
			n, strings.Join(r.nesting, ", ")))
	}
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

	res, err := r.exec(ctx, "rc-status", "--format", "ini")
	if err != nil {
		return r.execErr(ctx, "openrc services", err)
	}
	var failed []string
	if res.ExitCode == 0 && strings.Contains(res.Stdout, "[") {
		failed = openRCFailed(res.Stdout)
	} else {
		// An OpenRC without --format ini: crashed services only.
		res, err = r.exec(ctx, "rc-status", "--crashed")
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
		failed = firstFields(res.Stdout)
	}

	opts, err := r.rootOptions(ctx)
	if err != nil {
		return r.execErr(ctx, "root filesystem", err)
	}
	if opts == nil {
		return r.errorOut("cannot read the root filesystem's mount options")
	}
	if slices.Contains(opts, "ro") {
		return r.fail(ctx, "root filesystem read-only")
	}

	if o, bad := r.triage(ctx, failed); bad {
		return o
	}
	// Per-guest checks (ports, commands, services via rc-service). The
	// built-in profiles match systemd unit names, so they do not apply.
	c := &checker{ex: r.ex, openrc: true}
	err = c.runProfiles(ctx, r.in, nil)
	r.out.Checks = c.results
	if err != nil {
		return r.execErr(ctx, "guest checks", err)
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
	if r.in.NoNesting && len(untraced) > 0 {
		r.nesting, untraced = nestingFailures(ctx, r.ex, untraced)
	}
	if len(untraced) > 0 {
		return r.fail(ctx, "failed: "+strings.Join(untraced, ", ")), true
	}
	return Outcome{}, false
}
