package probe

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

type linuxRun struct {
	ex  Executor
	in  Input
	out Outcome
}

func (r *linuxRun) exec(ctx context.Context, argv ...string) (ExecResult, error) {
	return r.ex.Exec(ctx, argv)
}

// fail finalizes a FAILED outcome and captures the boot journal.
func (r *linuxRun) fail(ctx context.Context, reason string) Outcome {
	r.out.Verdict = model.Failed
	r.out.Reason = reason
	if res, err := r.exec(ctx, "journalctl", "-b", "--no-pager", "-n", "200"); err == nil {
		r.out.BootLog = res.Stdout
	}
	return r.out
}

func (r *linuxRun) booted(reason string) Outcome {
	r.out.Verdict = model.Booted
	r.out.Reason = reason
	return r.out
}

// execErr maps an Exec error to an outcome.
func (r *linuxRun) execErr(ctx context.Context, err error) Outcome {
	if errors.Is(err, ErrExecBlocked) {
		return r.booted(ErrExecBlocked.Error())
	}
	return r.fail(ctx, "probe error: "+err.Error())
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

func runLinux(ctx context.Context, ex Executor, in Input) Outcome {
	r := &linuxRun{ex: ex, in: in}

	res, err := r.exec(ctx, "systemctl", "is-system-running", "--wait")
	if err != nil {
		if errors.Is(err, ErrExecBlocked) {
			return r.booted(ErrExecBlocked.Error())
		}
		return r.booted("cannot run commands: " + err.Error())
	}
	if res.ExitCode == 127 {
		return r.runOpenRC(ctx)
	}
	switch state := strings.TrimSpace(res.Stdout); state {
	case "maintenance", "emergency", "starting", "stopping", "offline":
		return r.fail(ctx, "system state: "+state)
	}

	res, err = r.exec(ctx, "findmnt", "-n", "-o", "OPTIONS", "/")
	if err != nil {
		return r.execErr(ctx, err)
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
		return r.execErr(ctx, err)
	}
	if res.ExitCode != 0 {
		return r.fail(ctx, fmt.Sprintf("probe error: systemctl list-units exited %d", res.ExitCode))
	}
	var realFailed []string
	for _, u := range firstFields(res.Stdout) {
		if matchAny(in.Checks.IgnoreUnits, u) {
			r.out.IgnoredUnits = append(r.out.IgnoredUnits, u)
		} else {
			realFailed = append(realFailed, u)
		}
	}
	if len(realFailed) > 0 {
		r.out.FailedUnits = realFailed
		covered, untraced := traceMissing(ctx, ex, realFailed, in.Missing)
		r.out.NotCovered = covered
		if len(untraced) > 0 {
			return r.fail(ctx, "failed: "+strings.Join(untraced, ", "))
		}
	}

	res, err = r.exec(ctx, "systemctl", "list-unit-files", "--state=enabled", "--no-legend", "--plain")
	if err != nil {
		return r.execErr(ctx, err)
	}
	if res.ExitCode != 0 {
		return r.fail(ctx, fmt.Sprintf("probe error: systemctl list-unit-files exited %d", res.ExitCode))
	}
	c := &checker{ex: ex}
	err = c.runProfiles(ctx, in, firstFields(res.Stdout))
	r.out.Checks = c.results
	if err != nil {
		return r.execErr(ctx, err)
	}
	return r.finish(ctx)
}

func (r *linuxRun) finish(ctx context.Context) Outcome {
	for _, c := range r.out.Checks {
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

func (r *linuxRun) runOpenRC(ctx context.Context) Outcome {
	res, err := r.exec(ctx, "rc-status", "--crashed")
	if err != nil {
		return r.execErr(ctx, err)
	}
	if res.ExitCode == 127 {
		return r.booted("init answering, service state unknown")
	}
	var real []string
	for _, u := range firstFields(res.Stdout) {
		if matchAny(r.in.Checks.IgnoreUnits, u) {
			r.out.IgnoredUnits = append(r.out.IgnoredUnits, u)
		} else {
			real = append(real, u)
		}
	}
	if len(real) > 0 {
		r.out.FailedUnits = real
		covered, untraced := traceMissing(ctx, r.ex, real, r.in.Missing)
		r.out.NotCovered = covered
		if len(untraced) > 0 {
			return r.fail(ctx, "failed: "+strings.Join(untraced, ", "))
		}
	}
	return r.finish(ctx)
}
