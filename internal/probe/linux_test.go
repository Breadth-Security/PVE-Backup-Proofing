package probe_test

import (
	"context"
	"errors"
	"slices"
	"strings"
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

// rc-status --crashed exits 0 or 1 in normal operation; its stdout lists the
// crashed services. Any other exit is a probe problem, not the backup's.
func TestOpenRCExitCodes(t *testing.T) {
	ex := probetest.Scripted{"rc-status --crashed": probe.ExecResult{ExitCode: 1}}
	if o := run(ex, config.Checks{}); o.Verdict != model.Proven {
		t.Fatalf("exit 1, no output: %+v", o)
	}
	ex["rc-status --crashed"] = probe.ExecResult{ExitCode: 1, Stdout: "sshd\n"}
	if o := run(ex, config.Checks{}); o.Verdict != model.Failed || o.Reason != "failed: sshd" {
		t.Fatalf("exit 1 with a crashed service: %+v", o)
	}
	ex["rc-status --crashed"] = probe.ExecResult{ExitCode: 2, Stderr: "bad option"}
	if o := run(ex, config.Checks{}); o.Verdict != model.Error || o.Reason != "probe error: rc-status exited 2" {
		t.Fatalf("exit 2: %+v", o)
	}
}

// recordingExec records every argv it is asked to run.
type recordingExec struct {
	probetest.Scripted
	calls *[]string
}

func (r recordingExec) Exec(ctx context.Context, argv []string) (probe.ExecResult, error) {
	*r.calls = append(*r.calls, strings.Join(argv, " "))
	return r.Scripted.Exec(ctx, argv)
}

// A container's ostype says whether it runs OpenRC: Alpine and Gentoo go
// straight to rc-status, never through systemctl's exit 127.
func TestOpenRCByContainerOSType(t *testing.T) {
	for _, ostype := range []string{"alpine", "gentoo"} {
		var calls []string
		ex := recordingExec{probetest.Scripted{
			"rc-status --crashed": out(""),
			sysState:              out("running\n"), // must not be consulted
		}, &calls}
		o := probe.Run(context.Background(), ex, probe.Input{OS: probe.Linux, OSType: ostype})
		if o.Verdict != model.Proven {
			t.Errorf("%s: %+v", ostype, o)
		}
		for _, c := range calls {
			if strings.HasPrefix(c, "systemctl") {
				t.Errorf("%s: ran %q", ostype, c)
			}
		}
	}
}

// A systemd-type container whose systemctl is missing is not sent down the
// OpenRC path (that fallback is for VMs, whose init PBP cannot know).
func TestContainerWithoutSystemctl(t *testing.T) {
	var calls []string
	ex := recordingExec{probetest.Scripted{"rc-status --crashed": out("")}, &calls}
	o := probe.Run(context.Background(), ex, probe.Input{OS: probe.Linux, OSType: "debian"})
	if o.Verdict != model.Booted || o.Reason != "init answering, service state unknown" {
		t.Fatalf("got %+v", o)
	}
	if slices.Contains(calls, "rc-status --crashed") {
		t.Errorf("calls = %q", calls)
	}
}

func TestUnexpectedSystemState(t *testing.T) {
	for _, s := range []string{"initializing", "unknown", ""} {
		ex := healthy()
		ex[sysState] = out(s + "\n")
		o := run(ex, config.Checks{})
		if o.Verdict != model.Failed || o.Reason != "system state: "+s {
			t.Fatalf("%q: %+v", s, o)
		}
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
	// A transport error (SSH, API) says nothing about the backup: ERROR.
	boom := errors.New("boom")
	if o := run(errExec{err: boom}, config.Checks{}); o.Verdict != model.Error || o.Reason != "probe transport error: boom" {
		t.Fatalf("got %+v", o)
	}
	if o := run(errExec{firstOK: true, err: boom}, config.Checks{}); o.Verdict != model.Error || o.Reason != "probe transport error: boom" {
		t.Fatalf("got %+v", o)
	}
	if o := run(errExec{err: probe.ErrExecBlocked}, config.Checks{}); o.Verdict != model.Booted || o.Reason != "agent cannot run commands" {
		t.Fatalf("got %+v", o)
	}
	if o := run(errExec{firstOK: true, err: probe.ErrExecBlocked}, config.Checks{}); o.Verdict != model.Booted || o.Reason != "agent cannot run commands" {
		t.Fatalf("got %+v", o)
	}
}

// A transport error in a profile check is ERROR as well.
func TestProfileCheckTransportError(t *testing.T) {
	ex := cmdFails{healthy(), "ss", errors.New("ssh: connection lost")}
	o := run(ex, config.Checks{})
	if o.Verdict != model.Error || o.Reason != "probe transport error: ssh: connection lost" {
		t.Fatalf("got %+v", o)
	}
}

// cmdFails fails every command whose argv[0] is cmd.
type cmdFails struct {
	probetest.Scripted
	cmd string
	err error
}

func (c cmdFails) Exec(ctx context.Context, argv []string) (probe.ExecResult, error) {
	if argv[0] == c.cmd {
		return probe.ExecResult{}, c.err
	}
	return c.Scripted.Exec(ctx, argv)
}

func TestDispatch(t *testing.T) {
	o := probe.Run(context.Background(), probetest.Scripted{}, probe.Input{OS: probe.Windows})
	if o.Verdict != model.Booted || o.Reason != "agent cannot run commands" {
		t.Fatalf("got %+v", o)
	}
	o = probe.Run(context.Background(), probetest.Scripted{}, probe.Input{OS: probe.Unknown})
	if o.Verdict != model.Booted || o.Reason != "unknown guest OS" {
		t.Fatalf("got %+v", o)
	}
}
