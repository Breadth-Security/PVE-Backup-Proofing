package probe_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/probe"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/probe/probetest"
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
		sysState:     out("running\n"),
		findmnt:      out("rw,relatime\n"),
		failedC:      out(""),
		enabledC:     out("nginx.service enabled enabled\n"),
		ssC:          out("LISTEN 0 511 0.0.0.0:443 0.0.0.0:*\nLISTEN 0 4096 [::]:22 [::]:*\n"),
		autoRestartC: out(""),
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
	// "starting" from --wait means an old systemd ignored --wait; it is
	// polled (TestSystemdIgnoresWait).
	for _, s := range []string{"emergency", "maintenance", "stopping", "offline"} {
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
	if o.Verdict != model.Failed || o.Reason != "check failed: postgresql (command pg_isready -q)" {
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
	if o.Verdict != model.Failed || o.Reason != "check failed: port 5432" {
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
	if o.Verdict != model.Failed || o.Reason != "check failed: cron (service cron.service)" {
		t.Fatalf("got %+v", o)
	}
}

func TestOpenRC(t *testing.T) {
	ex := probetest.Scripted{"rc-status --crashed": out(" nginx    [ crashed ]\n"), "findmnt -n -o OPTIONS /": out("rw\n")}
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
	ex := probetest.Scripted{"rc-status --crashed": probe.ExecResult{ExitCode: 1}, "findmnt -n -o OPTIONS /": out("rw\n")}
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

// openRCSettle identifies the command that waits for OpenRC to finish
// booting (it polls `rc-status -r`).
func openRCSettle(argv []string) bool {
	return len(argv) == 3 && argv[0] == "sh" && argv[1] == "-c" && strings.Contains(argv[2], "rc-status -r")
}

// settlingOpenRC answers the OpenRC settle wait: it blocks for delay (or
// until its context ends, if hang), then reports the runlevel reached.
type settlingOpenRC struct {
	probetest.Scripted
	delay time.Duration
	hang  bool
	calls *[]string
}

// settledThen answers the OpenRC settle script at once and delegates the rest.
type settledThen struct{ probe.Executor }

func (s settledThen) Exec(ctx context.Context, argv []string) (probe.ExecResult, error) {
	if openRCSettle(argv) {
		return out(""), nil
	}
	return s.Executor.Exec(ctx, argv)
}

func (s settlingOpenRC) Exec(ctx context.Context, argv []string) (probe.ExecResult, error) {
	if s.calls != nil {
		*s.calls = append(*s.calls, strings.Join(argv, " "))
	}
	if openRCSettle(argv) {
		if s.hang {
			<-ctx.Done()
			return probe.ExecResult{}, fmt.Errorf("pct exec: %w", ctx.Err())
		}
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return probe.ExecResult{}, fmt.Errorf("pct exec: %w", ctx.Err())
		}
		return out(""), nil
	}
	return s.Scripted.Exec(ctx, argv)
}

// An Alpine guest is checked only after OpenRC has finished booting: on the
// isolated bridge the boot runlevel waits on networking, and services in the
// default runlevel (where crashes show up) start only after it. Seen on pve1:
// rc-status --crashed was empty for ~15s, then listed the crashed service.
func TestOpenRCWaitsForBoot(t *testing.T) {
	var calls []string
	ex := settlingOpenRC{Scripted: probetest.Scripted{"rc-status --crashed": out("pbp-it-crash\n"), "findmnt -n -o OPTIONS /": out("rw\n")}, delay: 50 * time.Millisecond, calls: &calls}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	o := probe.Run(ctx, ex, probe.Input{OS: probe.Linux, OSType: "alpine", Checks: config.Checks{CommandTimeout: 10 * time.Millisecond}})
	if o.Verdict != model.Failed || o.Reason != "failed: pbp-it-crash" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
	settled := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "sh -c ") && strings.Contains(c, "rc-status -r") })
	crashed := slices.Index(calls, "rc-status --crashed")
	if settled < 0 || crashed < 0 || settled > crashed {
		t.Fatalf("settle wait must run before rc-status --crashed: %q", calls)
	}
}

// OpenRC that never finishes booting before the probe deadline is ERROR
// ("guest still starting"), never PROVEN.
func TestOpenRCNeverSettles(t *testing.T) {
	ex := settlingOpenRC{Scripted: probetest.Scripted{"rc-status --crashed": out(""), "findmnt -n -o OPTIONS /": out("rw\n")}, hang: true}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	o := probe.Run(ctx, ex, probe.Input{OS: probe.Linux, OSType: "alpine"})
	if o.Verdict != model.Error || !strings.HasPrefix(o.Reason, "guest still starting after ") {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
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
		ex := settlingOpenRC{Scripted: probetest.Scripted{
			"rc-status --crashed": out(""), "findmnt -n -o OPTIONS /": out("rw\n"),
			sysState: out("running\n"), // must not be consulted
		}, calls: &calls}
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
	ex := recordingExec{probetest.Scripted{"rc-status --crashed": out(""), "findmnt -n -o OPTIONS /": out("rw\n")}, &calls}
	o := probe.Run(context.Background(), ex, probe.Input{OS: probe.Linux, OSType: "debian"})
	if o.Verdict != model.Booted || o.Reason != "init answering, service state unknown" {
		t.Fatalf("got %+v", o)
	}
	if slices.Contains(calls, "rc-status --crashed") {
		t.Errorf("calls = %q", calls)
	}
}

func TestUnexpectedSystemState(t *testing.T) {
	for _, s := range []string{"unknown", ""} {
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

// hangExec blocks on the listed commands until its context ends, like a
// command stuck inside the guest; everything else is scripted.
type hangExec struct {
	probetest.Scripted
	hang map[string]bool
}

func (h hangExec) Exec(ctx context.Context, argv []string) (probe.ExecResult, error) {
	if h.hang[strings.Join(argv, " ")] {
		<-ctx.Done()
		return probe.ExecResult{}, fmt.Errorf("agent exec: %w", ctx.Err())
	}
	return h.Scripted.Exec(ctx, argv)
}

func hanging(ex probetest.Scripted, cmds ...string) hangExec {
	h := hangExec{ex, map[string]bool{}}
	for _, c := range cmds {
		h.hang[c] = true
	}
	return h
}

// A single command that times out fails its check (FAILED), and the other
// checks still run.
func TestCheckTimesOut(t *testing.T) {
	ex := healthy()
	ex["sh -c ok-cmd"] = out("")
	ck := config.Checks{CommandTimeout: 20 * time.Millisecond, Guests: []config.GuestChecks{
		{Match: config.GuestMatch{VMID: 7}, CheckSet: config.CheckSet{Commands: []string{"hang-cmd", "ok-cmd"}}},
	}}
	o := probe.Run(context.Background(), hanging(ex, "sh -c hang-cmd"), probe.Input{OS: probe.Linux, Checks: ck, Guest: probe.GuestInfo{VMID: 7}})
	if o.Verdict != model.Failed || o.Reason != "check timed out: command hang-cmd" {
		t.Fatalf("got %+v", o)
	}
	want := []model.CheckResult{
		{Name: "nginx:ports:80,443", OK: true},
		{Name: "guest:commands:hang-cmd", OK: false, Detail: "timed out"},
		{Name: "guest:commands:ok-cmd", OK: true},
	}
	if !reflect.DeepEqual(o.Checks, want) {
		t.Errorf("checks = %+v", o.Checks)
	}
}

// slowExec answers the listed commands only after a delay, like a guest
// whose boot is still settling; everything else is scripted.
type slowExec struct {
	probetest.Scripted
	delay map[string]time.Duration
}

func (s slowExec) Exec(ctx context.Context, argv []string) (probe.ExecResult, error) {
	if d, ok := s.delay[strings.Join(argv, " ")]; ok {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return probe.ExecResult{}, fmt.Errorf("agent exec: %w", ctx.Err())
		}
	}
	return s.Scripted.Exec(ctx, argv)
}

const sysStateNow = "systemctl is-system-running"

// Waiting for boot to settle is bounded by the probe deadline, not by the
// per-command timeout: a slow but healthy boot is PROVEN.
func TestSlowBootSettlesWithinProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ex := slowExec{healthy(), map[string]time.Duration{sysState: 100 * time.Millisecond}}
	o := probe.Run(ctx, ex, probe.Input{OS: probe.Linux, Checks: config.Checks{CommandTimeout: 20 * time.Millisecond}})
	if o.Verdict != model.Proven {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}

// When boot has not settled before the probe deadline, PBP asks once more
// without --wait and judges the current state.
func TestBootNotSettledAtDeadline(t *testing.T) {
	for _, tc := range []struct {
		state     string
		verdict   model.Verdict
		reasonPfx string
	}{
		{"starting", model.Error, "guest still starting after "},
		{"initializing", model.Error, "guest still starting after "},
		{"degraded", model.Proven, "1 checks ok"},
		{"running", model.Proven, "1 checks ok"},
		{"emergency", model.Failed, "system state: emergency"},
		{"maintenance", model.Failed, "system state: maintenance"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		ex := healthy()
		ex[sysStateNow] = out(tc.state + "\n")
		o := probe.Run(ctx, hanging(ex, sysState), probe.Input{OS: probe.Linux, Checks: config.Checks{CommandTimeout: 20 * time.Millisecond}})
		cancel()
		if o.Verdict != tc.verdict || !strings.HasPrefix(o.Reason, tc.reasonPfx) {
			t.Errorf("%s: got %v %q", tc.state, o.Verdict, o.Reason)
		}
	}
}

// A guest still starting at the deadline names the jobs boot is waiting on,
// and keeps the job list as its boot log (issue #29: ifupdown2 waiting for
// DHCPv6 on the isolated bridge). Without a job list the reason is unchanged.
func TestBootNotSettledNamesPendingJobs(t *testing.T) {
	const jobs = "99 networking.service start running\n104 app.service start waiting\n105 networking.service stop waiting\n"
	for name, tc := range map[string]struct {
		list   string
		suffix string
	}{
		"jobs":    {jobs, " (waiting on: networking.service, app.service)"},
		"no jobs": {"", ""},
	} {
		ex := healthy()
		ex[sysStateNow] = out("starting\n")
		ex["systemctl list-jobs --no-legend --plain --no-pager"] = out(tc.list)
		o := probe.Run(context.Background(), hanging(ex, sysState), probe.Input{OS: probe.Linux, SettleTimeout: 50 * time.Millisecond})
		if o.Verdict != model.Error || !strings.HasPrefix(o.Reason, "guest still starting after ") || !strings.HasSuffix(o.Reason, "s"+tc.suffix) {
			t.Errorf("%s: got %v %q", name, o.Verdict, o.Reason)
		}
		if o.BootLog != tc.list {
			t.Errorf("%s: boot log %q, want the job list", name, o.BootLog)
		}
	}
}

// systemd older than 240 (RHEL/CentOS 7, Ubuntu 18.04) has no `--wait`
// for is-system-running: it exits 1 with "unrecognized option". PBP then
// polls the state instead of failing the guest on an empty state (code
// review I12).
func TestOldSystemdWithoutWait(t *testing.T) {
	refused := probe.ExecResult{ExitCode: 1, Stderr: "systemctl: unrecognized option '--wait'\n"}
	ex := healthy()
	ex[sysState] = refused
	ex[sysStateNow] = out("running\n")
	o := probe.Run(context.Background(), ex, probe.Input{OS: probe.Linux, SettleTimeout: time.Second})
	if o.Verdict != model.Proven {
		t.Fatalf("old systemd, running: got %v %q, want PROVEN", o.Verdict, o.Reason)
	}

	ex = healthy()
	ex[sysState] = refused
	ex[sysStateNow] = out("starting\n")
	o = probe.Run(context.Background(), ex, probe.Input{OS: probe.Linux, SettleTimeout: 100 * time.Millisecond})
	if o.Verdict != model.Error || !strings.HasPrefix(o.Reason, "guest still starting after ") {
		t.Fatalf("old systemd, never settles: got %v %q", o.Verdict, o.Reason)
	}
}

// stateSeq answers one command with each result in turn, repeating the
// last; other commands come from the script.
type stateSeq struct {
	probetest.Scripted
	cmd   string
	res   []probe.ExecResult
	calls *int
}

func (s stateSeq) Exec(ctx context.Context, argv []string) (probe.ExecResult, error) {
	if strings.Join(argv, " ") != s.cmd {
		return s.Scripted.Exec(ctx, argv)
	}
	i := min(*s.calls, len(s.res)-1)
	*s.calls++
	return s.res[i], nil
}

// systemd 232-239 accept --wait but ignore it, answering "starting" at once:
// that is polled like an old systemd that refuses --wait, not FAILED.
func TestSystemdIgnoresWait(t *testing.T) {
	ex := healthy()
	ex[sysState] = probe.ExecResult{ExitCode: 1, Stdout: "starting\n"}
	calls := 0
	seq := stateSeq{ex, sysStateNow, []probe.ExecResult{{ExitCode: 1, Stdout: "starting\n"}, out("running\n")}, &calls}
	o := probe.Run(context.Background(), seq, probe.Input{OS: probe.Linux, SettleTimeout: 5 * time.Second})
	if o.Verdict != model.Proven || calls != 2 {
		t.Fatalf("got %v %q after %d polls, want PROVEN after 2", o.Verdict, o.Reason, calls)
	}
}

// A poll that times out while the guest is busy booting counts as still
// starting, not as a failed check.
func TestOldSystemdPollTimesOut(t *testing.T) {
	ex := healthy()
	ex[sysState] = probe.ExecResult{ExitCode: 1, Stderr: "systemctl: unrecognized option '--wait'\n"}
	o := probe.Run(context.Background(), hanging(ex, sysStateNow),
		probe.Input{OS: probe.Linux, SettleTimeout: 100 * time.Millisecond, Checks: config.Checks{CommandTimeout: 20 * time.Millisecond}})
	if o.Verdict != model.Error || !strings.HasPrefix(o.Reason, "guest still starting after ") {
		t.Fatalf("got %v %q, want ERROR guest still starting", o.Verdict, o.Reason)
	}
}

// A guest that is still starting is ERROR even when no probe deadline was
// set: the settle wait has its own default bound.
func TestBootSettleWithoutProbeDeadline(t *testing.T) {
	ex := healthy()
	ex[sysStateNow] = out("starting\n")
	o := probe.Run(context.Background(), hanging(ex, sysState), probe.Input{OS: probe.Linux, SettleTimeout: 50 * time.Millisecond})
	if o.Verdict != model.Error || !strings.HasPrefix(o.Reason, "guest still starting after ") {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}

// A shutdown while waiting for boot to settle is ERROR (probe cancelled),
// never FAILED.
func TestBootSettleCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	o := probe.Run(ctx, hanging(healthy(), sysState), probe.Input{OS: probe.Linux})
	if o.Verdict != model.Error || o.Reason != "probe cancelled" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}

func TestBaseCommandTimesOut(t *testing.T) {
	ck := config.Checks{CommandTimeout: 20 * time.Millisecond}
	for cmd, name := range map[string]string{
		findmnt:      "root filesystem",
		failedC:      "failed units",
		enabledC:     "enabled units",
		ssC:          "nginx (ports 80,443)",
		autoRestartC: "crash-looping services",
	} {
		o := probe.Run(context.Background(), hanging(healthy(), cmd), probe.Input{OS: probe.Linux, Checks: ck})
		if o.Verdict != model.Failed || o.Reason != "check timed out: "+name {
			t.Errorf("%s: got %v %q", cmd, o.Verdict, o.Reason)
		}
	}
	ex := probetest.Scripted{"rc-status --format ini": probe.ExecResult{ExitCode: 1}}
	o := probe.Run(context.Background(), settledThen{hanging(ex, "rc-status --crashed")}, probe.Input{OS: probe.Linux, OSType: "alpine", Checks: ck})
	if o.Verdict != model.Failed || o.Reason != "check timed out: openrc services" {
		t.Errorf("rc-status: got %v %q", o.Verdict, o.Reason)
	}
}

// When the whole probe deadline expires, PBP could not finish judging the
// guest: ERROR, not FAILED.
func TestProbeDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	ck := config.Checks{CommandTimeout: time.Hour}
	o := probe.Run(ctx, hanging(healthy(), enabledC), probe.Input{OS: probe.Linux, Checks: ck})
	if o.Verdict != model.Error || o.Reason != "probe timed out" {
		t.Fatalf("got %+v", o)
	}
	// Also when the deadline hits while tracing a failed unit.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel2()
	ex := healthy()
	ex[sysState] = out("degraded\n")
	ex[failedC] = out("a.mount loaded failed failed x\n")
	o = probe.Run(ctx2, hanging(ex, showC("a.mount")), probe.Input{OS: probe.Linux, Checks: ck})
	if o.Verdict != model.Error || o.Reason != "probe timed out" {
		t.Fatalf("tracing: got %+v", o)
	}
}

func TestWrappedExecBlockedIsBooted(t *testing.T) {
	o := run(errExec{err: fmt.Errorf("agent exec: %w", probe.ErrExecBlocked)}, config.Checks{})
	if o.Verdict != model.Booted || o.Reason != "agent cannot run commands" {
		t.Fatalf("got %+v", o)
	}
}

func TestGuestCheckMatchByVMIDAndName(t *testing.T) {
	cs := config.CheckSet{Commands: []string{"probe-me"}}
	for name, tc := range map[string]struct {
		match config.GuestMatch
		guest probe.GuestInfo
		runs  bool
	}{
		"vmid":          {config.GuestMatch{VMID: 7}, probe.GuestInfo{VMID: 7}, true},
		"name":          {config.GuestMatch{Name: "db1"}, probe.GuestInfo{VMID: 9, Name: "db1"}, true},
		"other vmid":    {config.GuestMatch{VMID: 7}, probe.GuestInfo{VMID: 8}, false},
		"other name":    {config.GuestMatch{Name: "db1"}, probe.GuestInfo{Name: "db2"}, false},
		"zero vmid":     {config.GuestMatch{VMID: 0, Name: "db1"}, probe.GuestInfo{VMID: 0, Name: "x"}, false},
		"unnamed guest": {config.GuestMatch{Name: "db1"}, probe.GuestInfo{}, false},
	} {
		ex := healthy()
		ex["sh -c probe-me"] = probe.ExecResult{ExitCode: 1}
		ck := config.Checks{Guests: []config.GuestChecks{{Match: tc.match, CheckSet: cs}}}
		o := probe.Run(context.Background(), ex, probe.Input{OS: probe.Linux, Checks: ck, Guest: tc.guest})
		if ran := o.Verdict == model.Failed && o.Reason == "check failed: command probe-me"; ran != tc.runs {
			t.Errorf("%s: ran=%v, got %v %q", name, ran, o.Verdict, o.Reason)
		}
	}
}

// A unit listed twice is judged, reported and traced once.
func TestFailedUnitsDeduplicated(t *testing.T) {
	ex := healthy()
	ex[sysState] = out("degraded\n")
	ex[failedC] = out("a.service loaded failed failed A\n● a.service loaded failed failed A\nb.service loaded failed failed B\n")
	o := run(ex, config.Checks{})
	if o.Verdict != model.Failed || !reflect.DeepEqual(o.FailedUnits, []string{"a.service", "b.service"}) {
		t.Fatalf("got %+v", o)
	}
}

// Ignored failures never excuse a real one beside them.
func TestMixedIgnoredAndRealFailures(t *testing.T) {
	ex := healthy()
	ex[sysState] = out("degraded\n")
	ex[failedC] = out("chronyd.service loaded failed failed C\nnginx.service loaded failed failed N\n")
	o := run(ex, config.Checks{IgnoreUnits: []string{"chronyd.*"}})
	if o.Verdict != model.Failed || o.Reason != "failed: nginx.service" ||
		!reflect.DeepEqual(o.IgnoredUnits, []string{"chronyd.service"}) || !reflect.DeepEqual(o.FailedUnits, []string{"nginx.service"}) {
		t.Fatalf("got %+v", o)
	}
}

func TestListUnitsNonZeroExit(t *testing.T) {
	ex := healthy()
	ex[failedC] = probe.ExecResult{ExitCode: 1}
	o := run(ex, config.Checks{})
	if o.Verdict != model.Failed || o.Reason != "probe error: systemctl list-units exited 1" {
		t.Fatalf("got %+v", o)
	}
}

// `errors=remount-ro` is a mount option about error handling, not a
// read-only root.
func TestRemountRoOptionIsNotReadOnly(t *testing.T) {
	ex := healthy()
	ex[findmnt] = out("rw,relatime,errors=remount-ro\n")
	if o := run(ex, config.Checks{}); o.Verdict != model.Proven {
		t.Fatalf("got %+v", o)
	}
}

// A profile's ports are any-of: one of two listening passes.
func TestProfilePortsAnyOf(t *testing.T) {
	ck := config.Checks{Profiles: []config.Profile{{Name: "nginx", Unit: "nginx.service", CheckSet: config.CheckSet{Ports: []int{80, 443}}}}}
	o := run(healthy(), ck) // listens on 443 only
	if o.Verdict != model.Proven || len(o.Checks) != 1 || o.Checks[0].Name != "nginx:ports:80,443" || !o.Checks[0].OK {
		t.Fatalf("got %+v", o)
	}
}

// The boot journal is attached only from a journalctl that succeeded.
func TestBootLogOnlyFromSuccessfulJournalctl(t *testing.T) {
	const journal = "journalctl -b --no-pager -n 200"
	for name, tc := range map[string]struct {
		res  probe.ExecResult
		want string
	}{
		"ok":     {probe.ExecResult{Stdout: "boot log"}, "boot log"},
		"failed": {probe.ExecResult{Stdout: "partial", Stderr: "No journal files", ExitCode: 1}, ""},
		"absent": {probe.ExecResult{ExitCode: 127}, ""},
	} {
		ex := healthy()
		ex[failedC] = out("a.service loaded failed failed A\n")
		ex[journal] = tc.res
		if o := run(ex, config.Checks{}); o.Verdict != model.Failed || o.BootLog != tc.want {
			t.Errorf("%s: got %v bootlog %q", name, o.Verdict, o.BootLog)
		}
	}
}

const (
	procMounts = "cat /proc/self/mounts"
	procTCP    = "cat /proc/net/tcp /proc/net/tcp6"
)

// Without findmnt (or when it fails), the root's options come from
// /proc/self/mounts, so a read-only root is never silently missed (code
// review I8). The last "/" line counts (an overmount).
func TestRootReadOnlyWithoutFindmnt(t *testing.T) {
	for name, tc := range map[string]struct {
		mounts  string
		verdict model.Verdict
	}{
		"ro":        {"sysfs /sys sysfs rw 0 0\n/dev/sda1 / ext4 ro,relatime 0 0\n", model.Failed},
		"rw":        {"/dev/sda1 / ext4 rw,relatime 0 0\nproc /proc proc rw 0 0\n", model.Proven},
		"overmount": {"/dev/sda1 / ext4 ro 0 0\noverlay / overlay rw,lowerdir=x 0 0\n", model.Proven},
	} {
		ex := healthy()
		delete(ex, findmnt) // missing: exit 127
		ex[procMounts] = out(tc.mounts)
		if o := run(ex, config.Checks{}); o.Verdict != tc.verdict {
			t.Errorf("%s: got %v %q, want %v", name, o.Verdict, o.Reason, tc.verdict)
		}
	}
	ex := healthy()
	delete(ex, findmnt)
	if o := run(ex, config.Checks{}); o.Verdict != model.Error {
		t.Errorf("neither findmnt nor /proc/self/mounts: got %v %q, want ERROR", o.Verdict, o.Reason)
	}
}

// Without ss, listening ports come from /proc/net/tcp and tcp6 (state 0A),
// so a guest without ss is not FAILED on its port checks (code review I8).
func TestListeningPortsWithoutSS(t *testing.T) {
	tcp := "  sl  local_address rem_address   st tx_queue rx_queue\n" +
		"   0: 00000000:01BB 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0 0 1 1 0\n" +
		"   1: 0100007F:1F90 0100007F:9C40 01 00000000:00000000 00:00000000 00000000     0 0 2 1 0\n" +
		"  sl  local_address                         remote_address                        st\n" +
		"   0: 00000000000000000000000000000000:0016 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0 0 3 1 0\n"
	ex := healthy()
	delete(ex, ssC)
	ex[procTCP] = out(tcp)
	o := run(ex, config.Checks{})
	if o.Verdict != model.Proven {
		t.Fatalf("got %v %q, want PROVEN (443 and 22 listen; 8080 only connected)", o.Verdict, o.Reason)
	}
	ex = healthy()
	delete(ex, ssC)
	if o := run(ex, config.Checks{}); o.Verdict != model.Error || !strings.HasPrefix(o.Reason, "cannot list listening ports") {
		t.Errorf("neither ss nor /proc/net/tcp: got %v %q, want ERROR", o.Verdict, o.Reason)
	}
}

const autoRestartC = "systemctl list-units --type=service --state=auto-restart,auto-restart-queued --plain --no-legend --no-pager"

func showRestarts(unit string) string { return "systemctl show -p Result -p NRestarts -- " + unit }

// A service that keeps crashing under Restart=always never reaches
// "failed": it sits in auto-restart. It counts as a failed unit, subject to
// the ignore list, once it restarted after a failed run, after 3 clean
// restarts, or when NRestarts is unknown (systemd < 235); one that restarted
// once or twice after clean exits is not judged yet (code review I7).
func TestCrashLoopingServiceFails(t *testing.T) {
	ex := healthy()
	ex[autoRestartC] = out("redis-server.service loaded activating auto-restart Advanced key-value store\n" +
		"slow.service loaded activating auto-restart Slow\nclean.service loaded activating auto-restart Clean\n" +
		"oneshot.service loaded activating auto-restart Oneshot\nold.service loaded activating auto-restart Old\n")
	ex[showRestarts("redis-server.service")] = out("Result=exit-code\nNRestarts=17\n")
	ex[showRestarts("slow.service")] = out("Result=exit-code\nNRestarts=1\n") // RestartSec=10: one crash so far
	ex[showRestarts("clean.service")] = out("Result=success\nNRestarts=3\n")
	ex[showRestarts("oneshot.service")] = out("Result=success\nNRestarts=1\n")
	ex[showRestarts("old.service")] = out("Result=exit-code\n")
	o := run(ex, config.Checks{})
	want := "failed: redis-server.service, slow.service, clean.service, old.service"
	if o.Verdict != model.Failed || o.Reason != want {
		t.Fatalf("got %v %q, want FAILED %q", o.Verdict, o.Reason, want)
	}
	o = run(ex, config.Checks{IgnoreUnits: []string{"redis-server.service", "slow.service", "clean.service", "old.service"}})
	if o.Verdict != model.Proven {
		t.Errorf("ignored: got %v %q, want PROVEN", o.Verdict, o.Reason)
	}
}

// A crash-loop check that cannot run is not skipped: a guest PBP could not
// check for crash loops is not PROVEN.
func TestCrashLoopCheckErrors(t *testing.T) {
	ex := healthy()
	ex[autoRestartC] = probe.ExecResult{ExitCode: 1}
	if o := run(ex, config.Checks{}); o.Verdict != model.Failed || o.Reason != "probe error: systemctl list-units exited 1" {
		t.Errorf("list exits 1: got %v %q", o.Verdict, o.Reason)
	}
	ex = healthy()
	ex[autoRestartC] = out("a.service loaded activating auto-restart A\n")
	o := probe.Run(context.Background(), hanging(ex, showRestarts("a.service")),
		probe.Input{OS: probe.Linux, Checks: config.Checks{CommandTimeout: 20 * time.Millisecond}})
	if o.Verdict != model.Failed || o.Reason != "check timed out: crash-looping services" {
		t.Errorf("show hangs: got %v %q", o.Verdict, o.Reason)
	}
}

const rcIni = "rc-status --format ini"

// alpine is an OpenRC container that has finished booting.
func alpine(s probetest.Scripted) probe.Outcome {
	return alpineChecks(s, config.Checks{})
}

func alpineChecks(s probetest.Scripted, ck config.Checks) probe.Outcome {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return probe.Run(ctx, settlingOpenRC{Scripted: s}, probe.Input{OS: probe.Linux, OSType: "alpine",
		Guest: probe.GuestInfo{VMID: 101, Name: "alp"}, Checks: ck})
}

// The real `rc-status --format ini` output from an Alpine 3.24 container on
// pve2: pbpbroken failed to start (stopped in its runlevel, not listed by
// --crashed) and pbpdies crashed (code review I9).
const alpineIni = "[default]\nnetworking =  started \npbpbroken =  stopped \npbpdies =  crashed \ncrond =  started \n" +
	"[Dynamic hotplugged]\n[Dynamic needed/wanted]\nlocalmount =  started \n[Dynamic manual]\n"

func TestOpenRCFailedToStart(t *testing.T) {
	o := alpine(probetest.Scripted{rcIni: out(alpineIni), findmnt: out("rw,relatime\n")})
	if o.Verdict != model.Failed || o.Reason != "failed: pbpbroken, pbpdies" {
		t.Fatalf("got %v %q, want FAILED naming pbpbroken and pbpdies", o.Verdict, o.Reason)
	}
	// A stopped service outside the runlevel (a dynamic dependency) is not judged.
	ini := "[default]\nsshd =  started \n[Dynamic needed/wanted]\nhwdrivers =  stopped \n"
	if o := alpine(probetest.Scripted{rcIni: out(ini), findmnt: out("rw\n")}); o.Verdict != model.Proven {
		t.Errorf("dynamic stopped: got %v %q, want PROVEN", o.Verdict, o.Reason)
	}
	// "failed" is a failure in any section; "scheduled" is not.
	ini = "[default]\nsshd =  started \nqueued =  scheduled \n[Dynamic needed/wanted]\nnetmount =  failed \n"
	if o := alpine(probetest.Scripted{rcIni: out(ini), findmnt: out("rw\n")}); o.Verdict != model.Failed || o.Reason != "failed: netmount" {
		t.Errorf("failed state: got %v %q, want FAILED naming netmount", o.Verdict, o.Reason)
	}
}

// An OpenRC too old for --format ini falls back to --crashed.
func TestOpenRCWithoutIniFormat(t *testing.T) {
	o := alpine(probetest.Scripted{
		rcIni:                 {ExitCode: 1, Stderr: "rc-status: unrecognized option '--format'\n"},
		"rc-status --crashed": out("pbpdies\n"),
		findmnt:               out("rw\n"),
	})
	if o.Verdict != model.Failed || o.Reason != "failed: pbpdies" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}

// OpenRC guests get the read-only-root check and the per-guest checks too;
// a per-guest service is checked with rc-service (code review I9).
func TestOpenRCRootAndGuestChecks(t *testing.T) {
	healthyIni := "[default]\nsshd =  started \n"
	o := alpine(probetest.Scripted{rcIni: out(healthyIni), procMounts: out("/dev/root / ext4 ro 0 0\n")})
	if o.Verdict != model.Failed || o.Reason != "root filesystem read-only" {
		t.Errorf("read-only root: got %v %q", o.Verdict, o.Reason)
	}
	ck := config.Checks{Guests: []config.GuestChecks{{Match: config.GuestMatch{VMID: 101},
		CheckSet: config.CheckSet{Services: []string{"sshd", "nginx"}, Commands: []string{"test -f /etc/alpine-release"}}}}}
	o = alpineChecks(probetest.Scripted{
		rcIni:                               out(healthyIni),
		findmnt:                             out("rw\n"),
		"rc-service sshd status":            out(" * status: started\n"),
		"rc-service nginx status":           {ExitCode: 3, Stdout: " * status: stopped\n"},
		"sh -c test -f /etc/alpine-release": out(""),
	}, ck)
	if o.Verdict != model.Failed || o.Reason != "check failed: service nginx" {
		t.Fatalf("per-guest checks: got %v %q, want nginx failed", o.Verdict, o.Reason)
	}
	if len(o.Checks) != 3 {
		t.Errorf("checks = %+v, want sshd, nginx and the command", o.Checks)
	}
}

// What a guest prints cannot grow an Outcome without bound: a reason names
// at most 10 units (all stay in FailedUnits), a check's detail and the
// reason are clipped, and the boot log keeps its newest 256 KiB (code
// review I14).
func TestOutcomeBounded(t *testing.T) {
	ex := healthy()
	ex[sysState] = out("degraded\n")
	var units []string
	for i := range 50 {
		units = append(units, fmt.Sprintf("u%02d.service loaded failed failed U", i))
	}
	ex[failedC] = out(strings.Join(units, "\n") + "\n")
	ex["journalctl -b --no-pager -n 200"] = out(strings.Repeat("old line\n", 64<<10) + "newest line\n")
	o := run(ex, config.Checks{})
	want := "failed: u00.service, u01.service, u02.service, u03.service, u04.service, u05.service, u06.service, " +
		"u07.service, u08.service, u09.service, … and 40 more"
	if o.Verdict != model.Failed || o.Reason != want || len(o.FailedUnits) != 50 {
		t.Fatalf("got %v %q with %d failed units", o.Verdict, o.Reason, len(o.FailedUnits))
	}
	if len(o.BootLog) > 256<<10+8 || !strings.HasSuffix(o.BootLog, "newest line\n") || !strings.HasPrefix(o.BootLog, "…\nold line") {
		t.Errorf("boot log: %d bytes, starts %q", len(o.BootLog), o.BootLog[:20])
	}

	ex = healthy()
	ex[enabledC] = out("postgresql.service enabled\n")
	ex["sh -c pg_isready -q"] = probe.ExecResult{ExitCode: 2, Stderr: strings.Repeat("é", 4096)}
	o = run(ex, config.Checks{})
	if len(o.Checks) != 1 || len(o.Checks[0].Detail) > 512 || !utf8.ValidString(o.Checks[0].Detail) || !strings.HasSuffix(o.Checks[0].Detail, "…") {
		t.Errorf("check detail not clipped: %+v", o.Checks)
	}
}

// noAnswerExec stalls on one command until its ctx ends, then reports that
// the transport never answered (the session was never opened).
type noAnswerExec struct {
	probetest.Scripted
	cmd string
}

func (n noAnswerExec) Exec(ctx context.Context, argv []string) (probe.ExecResult, error) {
	if strings.Join(argv, " ") == n.cmd {
		<-ctx.Done()
		return probe.ExecResult{}, fmt.Errorf("exec in container 990000: %w: %w", probe.ErrNoAnswer, ctx.Err())
	}
	return n.Scripted.Exec(ctx, argv)
}

// A command whose session never opened because the connection to the node
// died is a transport fault, ERROR, even when the per-command timeout ends
// it before the connection is found dead: it is not a check that timed out
// (review of #48, I1).
func TestStalledTransportIsError(t *testing.T) {
	ck := config.Checks{CommandTimeout: 20 * time.Millisecond}
	o := probe.Run(context.Background(), noAnswerExec{healthy(), ssC}, probe.Input{OS: probe.Linux, Checks: ck})
	if o.Verdict != model.Error || !strings.HasPrefix(o.Reason, "probe transport error: ") {
		t.Fatalf("got %v %q, want ERROR probe transport error", o.Verdict, o.Reason)
	}
}

// A guest whose root is read-only by design can say so in checks.guests;
// others still fail (code review 5.10).
func TestReadOnlyRootAllowedPerGuest(t *testing.T) {
	ex := healthy()
	ex[findmnt] = out("ro,relatime\n")
	ck := config.Checks{Guests: []config.GuestChecks{{Match: config.GuestMatch{Tag: "haos"}, AllowReadOnlyRoot: true}}}
	o := probe.Run(context.Background(), ex, probe.Input{OS: probe.Linux, Checks: ck, Guest: probe.GuestInfo{Tags: []string{"haos"}}})
	if o.Verdict != model.Proven {
		t.Errorf("allowed: got %v %q", o.Verdict, o.Reason)
	}
	o = probe.Run(context.Background(), ex, probe.Input{OS: probe.Linux, Checks: ck, Guest: probe.GuestInfo{Tags: []string{"web"}}})
	if o.Verdict != model.Failed || o.Reason != "root filesystem read-only" {
		t.Errorf("not matched: got %v %q", o.Verdict, o.Reason)
	}
}

// A Gentoo container running systemd: the ostype sends it to OpenRC, which
// finds no rc-status, so the systemd path judges it (code review 5.15).
func TestGentooWithSystemd(t *testing.T) {
	o := probe.Run(context.Background(), healthy(), probe.Input{OS: probe.Linux, OSType: "gentoo"})
	if o.Verdict != model.Proven {
		t.Fatalf("got %v %q, want PROVEN", o.Verdict, o.Reason)
	}
	// Neither init: still BOOTED, without looping.
	o = probe.Run(context.Background(), probetest.Scripted{}, probe.Input{OS: probe.Linux, OSType: "gentoo"})
	if o.Verdict != model.Booted || o.Reason != "init answering, service state unknown" {
		t.Fatalf("no init tools: got %v %q", o.Verdict, o.Reason)
	}
}

// A timeout reading the /proc fallbacks (no findmnt, no ss) is ERROR, not
// a FAILED "check timed out": a slow cat says nothing about the backup.
func TestProcFallbackTimeoutIsError(t *testing.T) {
	ck := config.Checks{CommandTimeout: 20 * time.Millisecond}
	ex := healthy()
	delete(ex, findmnt)
	o := probe.Run(context.Background(), hanging(ex, procMounts), probe.Input{OS: probe.Linux, Checks: ck})
	if o.Verdict != model.Error {
		t.Errorf("mounts: got %v %q, want ERROR", o.Verdict, o.Reason)
	}
	ex = healthy()
	delete(ex, ssC)
	o = probe.Run(context.Background(), hanging(ex, procTCP), probe.Input{OS: probe.Linux, Checks: ck})
	if o.Verdict != model.Error || !strings.HasPrefix(o.Reason, "cannot list listening ports") {
		t.Errorf("ports: got %v %q, want ERROR", o.Verdict, o.Reason)
	}
}

// A checks.guests entry can ignore units for the guests it matches only:
// sep5.service fails by design on CT114, and must still fail elsewhere
// (issue #68).
func TestPerGuestIgnoreUnits(t *testing.T) {
	ex := healthy()
	ex[sysState] = out("degraded\n")
	ex[failedC] = out("sep5.service loaded failed failed sep5\n")
	ck := config.Checks{Guests: []config.GuestChecks{{Match: config.GuestMatch{VMID: 114}, IgnoreUnits: []string{"sep5*"}}}}
	o := probe.Run(context.Background(), ex, probe.Input{OS: probe.Linux, Checks: ck, Guest: probe.GuestInfo{VMID: 114}})
	if o.Verdict != model.Proven || !slices.Equal(o.IgnoredUnits, []string{"sep5.service"}) {
		t.Errorf("CT114: got %v %q ignored %v", o.Verdict, o.Reason, o.IgnoredUnits)
	}
	o = probe.Run(context.Background(), ex, probe.Input{OS: probe.Linux, Checks: ck, Guest: probe.GuestInfo{VMID: 115}})
	if o.Verdict != model.Failed || o.Reason != "failed: sep5.service" {
		t.Errorf("another guest: got %v %q", o.Verdict, o.Reason)
	}
}
