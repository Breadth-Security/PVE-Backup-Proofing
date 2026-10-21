package probe_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/probe"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/probe/probetest"
)

const winCmd = "powershell.exe -NoProfile -NonInteractive -Command " + probe.WindowsServicesScript

func runWin(ex probe.Executor, ck config.Checks) probe.Outcome {
	return probe.Run(context.Background(), ex, probe.Input{OS: probe.Windows, Checks: ck})
}

func TestWindowsFailedAndIgnored(t *testing.T) {
	ex := probetest.Scripted{winCmd: out("sppsvc\r\nSpooler\r\n")}
	o := runWin(ex, config.Checks{WindowsIgnoreServices: []string{"SPPSVC"}})
	if o.Verdict != model.Failed || o.Reason != "stopped auto services: Spooler" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
	if !reflect.DeepEqual(o.IgnoredUnits, []string{"sppsvc"}) || !reflect.DeepEqual(o.FailedUnits, []string{"Spooler"}) {
		t.Fatalf("ignored=%v failed=%v", o.IgnoredUnits, o.FailedUnits)
	}
}

// Each stopped service's line carries why it stopped (issue #44): its exit
// codes and the newest Service Control Manager event since boot naming it.
// The Bonjour and WslInstaller lines are real output from a copy of VM115 on
// pve2.
// The reason stays the list of names; the explanations go in the boot log,
// for the services that count (not the ignored ones).
func TestWindowsStopReasons(t *testing.T) {
	ex := probetest.Scripted{winCmd: out("" +
		"Bonjour Service\t1066\t4294967295\t7024\tThe Bonjour Service service terminated with the following service-specific error: %%4294967295\r\n" +
		"WslInstaller\t0\t0\t\t\r\n" +
		"Spooler\t1077\t0\t\t\r\n" +
		"AppSvc\t1066\t5\t7024\tThe App service terminated with the following service-specific error: 5\r\n" +
		"sppsvc\t1067\t0\t7031\tignored anyway\r\n")}
	o := runWin(ex, config.Checks{WindowsIgnoreServices: []string{"sppsvc"}})
	if o.Verdict != model.Failed || o.Reason != "stopped auto services: Bonjour Service, WslInstaller, Spooler, AppSvc" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
	want := "Bonjour Service stopped: event 7024: The Bonjour Service service terminated with the following service-specific error: %%4294967295 (service-specific exit code 0xFFFFFFFF)\n" +
		"WslInstaller stopped: no Service Control Manager event since boot (exit code 0)\n" +
		"Spooler stopped: not started since boot (exit code 1077)\n" +
		"AppSvc stopped: event 7024: The App service terminated with the following service-specific error: 5 (service-specific exit code 5)"
	if o.BootLog != want {
		t.Errorf("boot log:\n%s\nwant:\n%s", o.BootLog, want)
	}
}

func TestWindowsDedupeAndBlanks(t *testing.T) {
	ex := probetest.Scripted{winCmd: out(" A \r\n\r\nB\nA\n  \n")}
	o := runWin(ex, config.Checks{})
	if o.Reason != "stopped auto services: A, B" {
		t.Fatalf("got %q", o.Reason)
	}
}

func TestWindowsProven(t *testing.T) {
	o := runWin(probetest.Scripted{winCmd: out("")}, config.Checks{})
	if o.Verdict != model.Proven || o.Reason != "windows services ok" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}

func TestWindowsProvenWithIgnored(t *testing.T) {
	o := runWin(probetest.Scripted{winCmd: out("sppsvc\r\n")}, config.Checks{WindowsIgnoreServices: []string{"sppsvc"}})
	if o.Verdict != model.Proven || o.Reason != "windows services ok, 1 ignored" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}

func TestWindowsExecUnavailable(t *testing.T) {
	for name, ex := range map[string]probe.Executor{
		"127": probetest.Scripted{},
		"err": errExec{err: probe.ErrExecBlocked},
	} {
		o := runWin(ex, config.Checks{})
		if o.Verdict != model.Booted || o.Reason != "agent cannot run commands" {
			t.Errorf("%s: got %v %q", name, o.Verdict, o.Reason)
		}
	}
}

func TestWindowsTransportError(t *testing.T) {
	o := runWin(errExec{err: errors.New("agent exec: 500 internal")}, config.Checks{})
	if o.Verdict != model.Error || o.Reason != "probe transport error: agent exec: 500 internal" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}

func TestWindowsCommandTimesOut(t *testing.T) {
	ck := config.Checks{CommandTimeout: 20 * time.Millisecond}
	o := probe.Run(context.Background(), hanging(probetest.Scripted{}, winCmd), probe.Input{OS: probe.Windows, Checks: ck})
	// PBP could not judge the services: ERROR, not FAILED (code review I11).
	if o.Verdict != model.Error || o.Reason != "windows services check timed out" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}

func TestWindowsNonZeroExit(t *testing.T) {
	o := runWin(probetest.Scripted{winCmd: {ExitCode: 1}}, config.Checks{})
	if o.Verdict != model.Error || o.Reason != "probe error: powershell exited 1" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}

// seqExec answers the service query with each output in turn, repeating
// the last, and counts the polls. After stopAfter polls (if > 0) it calls
// stop, which ends the probe deterministically instead of by wall clock.
type seqExec struct {
	outs      []string
	polls     *int
	stopAfter int
	stop      context.CancelFunc
}

func (s seqExec) Exec(context.Context, []string) (probe.ExecResult, error) {
	i := min(*s.polls, len(s.outs)-1)
	*s.polls++
	if s.stopAfter > 0 && *s.polls == s.stopAfter {
		s.stop()
	}
	return out(s.outs[i]), nil
}

// runWinSettle probes with a generous deadline (so settling is on) and, if
// stopAfter > 0, ends the probe after that many polls.
func runWinSettle(t *testing.T, outs []string, stopAfter int) (probe.Outcome, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	polls := 0
	o := probe.Run(ctx, seqExec{outs, &polls, stopAfter, cancel}, probe.Input{OS: probe.Windows, WindowsSettlePoll: time.Millisecond})
	return o, polls
}

// Automatic services may still be starting when the agent first answers:
// PBP re-polls until none is left stopped.
func TestWindowsSettlesToProven(t *testing.T) {
	o, polls := runWinSettle(t, []string{"Spooler\r\nWinRM\r\n", "WinRM\r\n", ""}, 0)
	if o.Verdict != model.Proven || polls != 3 {
		t.Fatalf("got %v %q after %d polls", o.Verdict, o.Reason, polls)
	}
}

// Only what is still stopped when the probe ends is FAILED.
func TestWindowsSettleFailsWithWhatRemains(t *testing.T) {
	o, polls := runWinSettle(t, []string{"Spooler\r\nWinRM\r\n", "WinRM\r\n"}, 3)
	if o.Verdict != model.Failed || o.Reason != "stopped auto services: WinRM" || polls != 3 {
		t.Fatalf("got %v %q after %d polls", o.Verdict, o.Reason, polls)
	}
	if !reflect.DeepEqual(o.FailedUnits, []string{"WinRM"}) {
		t.Errorf("failed units %v", o.FailedUnits)
	}
}

// A re-poll cut short by the end of the probe keeps the last full answer.
func TestWindowsSettleDeadlineDuringPoll(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ex := &firstThenHang{first: "Spooler\r\n", stop: cancel}
	o := probe.Run(ctx, ex, probe.Input{OS: probe.Windows, WindowsSettlePoll: time.Millisecond, Checks: config.Checks{CommandTimeout: time.Hour}})
	if o.Verdict != model.Failed || o.Reason != "stopped auto services: Spooler" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}

// firstThenHang answers once, then ends the probe during the next poll and
// hangs until the context is done.
type firstThenHang struct {
	first string
	done  bool
	stop  context.CancelFunc
}

func (f *firstThenHang) Exec(ctx context.Context, _ []string) (probe.ExecResult, error) {
	if !f.done {
		f.done = true
		return out(f.first), nil
	}
	f.stop()
	<-ctx.Done()
	return probe.ExecResult{}, ctx.Err()
}

func TestWindowsWrappedExecBlocked(t *testing.T) {
	o := runWin(errExec{err: fmt.Errorf("agent: %w", probe.ErrExecBlocked)}, config.Checks{})
	if o.Verdict != model.Booted || o.Reason != "agent cannot run commands" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}

// startedExec signals started on its first call, then hangs until ctx ends.
type startedExec struct{ started chan struct{} }

func (e startedExec) Exec(ctx context.Context, _ []string) (probe.ExecResult, error) {
	close(e.started)
	<-ctx.Done()
	return probe.ExecResult{}, ctx.Err()
}

// The probe ending before the first answer is ERROR, never BOOTED or FAILED.
func TestWindowsProbeEnded(t *testing.T) {
	in := probe.Input{OS: probe.Windows, Checks: config.Checks{CommandTimeout: time.Hour}}

	// A deadline that has already passed: the command sees the probe end.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if o := probe.Run(ctx, startedExec{make(chan struct{})}, in); o.Verdict != model.Error || o.Reason != "probe timed out" {
		t.Errorf("deadline: got %v %q", o.Verdict, o.Reason)
	}

	// Cancelled once the command is running.
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	ex := startedExec{make(chan struct{})}
	go func() { <-ex.started; cancel() }()
	if o := probe.Run(ctx, ex, in); o.Verdict != model.Error || o.Reason != "probe cancelled" {
		t.Errorf("cancel: got %v %q", o.Verdict, o.Reason)
	}
}

// resultsExec answers the service query with each result in turn, repeating
// the last.
type resultsExec struct {
	res   []probe.ExecResult
	polls *int
}

func (r resultsExec) Exec(context.Context, []string) (probe.ExecResult, error) {
	i := min(*r.polls, len(r.res)-1)
	*r.polls++
	return r.res[i], nil
}

// A failed service query (PowerShell exiting non-zero, as it can while the
// guest is still busy after boot) is retried like a stopped service, not
// FAILED at once (code review I11).
func TestWindowsQueryErrorRetried(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	polls := 0
	o := probe.Run(ctx, resultsExec{[]probe.ExecResult{{ExitCode: 1}, {ExitCode: 1}, out("")}, &polls},
		probe.Input{OS: probe.Windows, WindowsSettlePoll: time.Millisecond})
	if o.Verdict != model.Proven || polls != 3 {
		t.Fatalf("got %v %q after %d polls, want PROVEN after 3", o.Verdict, o.Reason, polls)
	}
}

// Delayed-start automatic services are checked too, but only count when
// they failed (an error exit code other than 1077, or a Service Control
// Manager failure event): one that has not started yet, or that stopped
// cleanly after its work, is fine (code review I10). The sixth column says
// whether the service is delayed.
func TestWindowsDelayedStartServices(t *testing.T) {
	ex := probetest.Scripted{winCmd: out("" +
		"DelayedBroken\t1067\t0\t7034\tThe DelayedBroken service terminated unexpectedly.\tTrue\r\n" +
		"DelayedErrExit\t1\t0\t\t\tTrue\r\n" +
		"DelayedNotYet\t1077\t0\t\t\tTrue\r\n" +
		"clr_optimization_v4.0.30319_64\t0\t0\t\t\tTrue\r\n" +
		"Spooler\t1077\t0\t\t\tFalse\r\n")}
	o := runWin(ex, config.Checks{})
	if o.Verdict != model.Failed || o.Reason != "stopped auto services: DelayedBroken, DelayedErrExit, Spooler" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}
