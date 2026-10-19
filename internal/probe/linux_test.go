package probe_test

import (
	"context"
	"errors"
	"testing"

	"git.breadthanalytics.com/tdiana/pbv/internal/config"
	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/probe"
	"git.breadthanalytics.com/tdiana/pbv/internal/probe/probetest"
)

const (
	sysState = "systemctl is-system-running --wait"
	findmnt  = "findmnt -n -o OPTIONS /"
	failedC  = "systemctl list-units --failed --plain --no-legend --no-pager"
	enabledC = "systemctl list-unit-files --state=enabled --no-legend --plain"
	ssC      = "ss -ltnH"
)

func out(s string) probe.ExecResult { return probe.ExecResult{Stdout: s} }

func healthy() probetest.Scripted {
	return probetest.Scripted{
		sysState: out("running\n"),
		findmnt:  out("rw,relatime\n"),
		failedC:  out(""),
		enabledC: out("nginx.service enabled enabled\n"),
		ssC:      out("LISTEN 0 511 0.0.0.0:443 0.0.0.0:*\nLISTEN 0 4096 [::]:22 [::]:*\n"),
	}
}

func run(ex probe.Executor, ck config.Checks) probe.Outcome {
	return probe.Run(context.Background(), ex, probe.Input{OS: probe.Linux, Checks: ck})
}

func TestHealthyProven(t *testing.T) {
	o := run(healthy(), config.Checks{})
	if o.Verdict != model.Proven || o.Reason != "1 checks ok" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
	if len(o.Checks) != 1 || o.Checks[0].Name != "nginx:ports:80,443" || !o.Checks[0].OK {
		t.Fatalf("checks: %+v", o.Checks)
	}
}

func TestDegradedIgnoredUnit(t *testing.T) {
	ex := healthy()
	ex[sysState] = out("degraded\n")
	ex[failedC] = out("● chronyd.service loaded failed failed chrony\n")
	o := run(ex, config.Checks{IgnoreUnits: []string{"chronyd.service"}})
	if o.Verdict != model.Proven || len(o.IgnoredUnits) != 1 || o.IgnoredUnits[0] != "chronyd.service" {
		t.Fatalf("got %+v", o)
	}
	if o.Reason != "1 checks ok, 1 ignored" {
		t.Fatalf("reason %q", o.Reason)
	}
}

func TestFailedUnit(t *testing.T) {
	ex := healthy()
	ex[failedC] = out("postgresql.service loaded failed failed PG\n")
	ex["journalctl -b --no-pager -n 200"] = out("boot log")
	o := run(ex, config.Checks{})
	if o.Verdict != model.Failed || o.Reason != "failed: postgresql.service" || o.BootLog != "boot log" {
		t.Fatalf("got %+v", o)
	}
}

func TestSystemStatesFail(t *testing.T) {
	for _, s := range []string{"emergency", "maintenance", "starting", "stopping", "offline"} {
		ex := healthy()
		ex[sysState] = out(s + "\n")
		o := run(ex, config.Checks{})
		if o.Verdict != model.Failed || o.Reason != "system state: "+s {
			t.Fatalf("%s: %+v", s, o)
		}
	}
}

func TestReadOnlyRoot(t *testing.T) {
	ex := healthy()
	ex[findmnt] = out("ro,relatime\n")
	o := run(ex, config.Checks{})
	if o.Verdict != model.Failed || o.Reason != "root filesystem read-only" {
		t.Fatalf("got %+v", o)
	}
}

func TestProfileCommandFails(t *testing.T) {
	ex := healthy()
	ex[enabledC] = out("postgresql@16-main.service enabled\npostgresql.service enabled\n")
	ex["sh -c pg_isready -q"] = probe.ExecResult{ExitCode: 2}
	o := run(ex, config.Checks{})
	if o.Verdict != model.Failed || o.Reason != "check failed: postgresql:commands:pg_isready -q" {
		t.Fatalf("got %+v", o)
	}
}

func TestGuestCheckByTag(t *testing.T) {
	ex := healthy()
	ck := config.Checks{Guests: []config.GuestChecks{{
		Match:    config.GuestMatch{Tag: "db"},
		CheckSet: config.CheckSet{Services: []string{"postgresql"}, Ports: []int{22, 5432}},
	}}}
	ex["systemctl is-active postgresql"] = out("active\n")
	o := probe.Run(context.Background(), ex, probe.Input{OS: probe.Linux, Checks: ck, Guest: probe.GuestInfo{Tags: []string{"x", "db"}}})
	if o.Verdict != model.Failed || o.Reason != "check failed: guest:ports:5432" {
		t.Fatalf("got %+v", o)
	}
	names := map[string]bool{}
	for _, c := range o.Checks {
		names[c.Name] = c.OK
	}
	if !names["guest:services:postgresql"] || !names["guest:ports:22"] || names["guest:ports:5432"] {
		t.Fatalf("checks %+v", o.Checks)
	}
	// untagged guest: no guest checks
	o = run(ex, ck)
	if o.Verdict != model.Proven {
		t.Fatalf("got %+v", o)
	}
}

func TestEmptyProfileRequiresActive(t *testing.T) {
	ex := healthy()
	ex[enabledC] = out("cron.service enabled\n")
	ex["systemctl is-active cron.service"] = out("inactive\n")
	o := run(ex, config.Checks{})
	if o.Verdict != model.Failed || o.Reason != "check failed: cron:services:cron.service" {
		t.Fatalf("got %+v", o)
	}
}

func TestOpenRC(t *testing.T) {
	ex := probetest.Scripted{"rc-status --crashed": out(" nginx    [ crashed ]\n")}
	o := run(ex, config.Checks{})
	if o.Verdict != model.Failed || o.Reason != "failed: nginx" {
		t.Fatalf("got %+v", o)
	}
	ex["rc-status --crashed"] = out("")
	if o = run(ex, config.Checks{}); o.Verdict != model.Proven {
		t.Fatalf("got %+v", o)
	}
}

func TestNoInitTools(t *testing.T) {
	o := run(probetest.Scripted{}, config.Checks{})
	if o.Verdict != model.Booted || o.Reason != "init answering, service state unknown" {
		t.Fatalf("got %+v", o)
	}
}

type errExec struct {
	firstOK bool
	err     error
}

func (e errExec) Exec(_ context.Context, argv []string) (probe.ExecResult, error) {
	if e.firstOK && argv[0] == "systemctl" && argv[1] == "is-system-running" {
		return out("running"), nil
	}
	return probe.ExecResult{}, e.err
}

func TestExecErrors(t *testing.T) {
	boom := errors.New("boom")
	if o := run(errExec{err: boom}, config.Checks{}); o.Verdict != model.Booted || o.Reason != "cannot run commands: boom" {
		t.Fatalf("got %+v", o)
	}
	if o := run(errExec{firstOK: true, err: boom}, config.Checks{}); o.Verdict != model.Failed || o.Reason != "probe error: boom" {
		t.Fatalf("got %+v", o)
	}
	if o := run(errExec{err: probe.ErrExecBlocked}, config.Checks{}); o.Verdict != model.Booted || o.Reason != "agent cannot run commands" {
		t.Fatalf("got %+v", o)
	}
	if o := run(errExec{firstOK: true, err: probe.ErrExecBlocked}, config.Checks{}); o.Verdict != model.Booted || o.Reason != "agent cannot run commands" {
		t.Fatalf("got %+v", o)
	}
}

func TestDispatch(t *testing.T) {
	o := probe.Run(context.Background(), probetest.Scripted{}, probe.Input{OS: probe.Windows})
	if o.Verdict != model.Booted || o.Reason != "windows probe not implemented" {
		t.Fatalf("got %+v", o)
	}
	o = probe.Run(context.Background(), probetest.Scripted{}, probe.Input{OS: probe.Unknown})
	if o.Verdict != model.Booted || o.Reason != "unknown guest OS" {
		t.Fatalf("got %+v", o)
	}
}
