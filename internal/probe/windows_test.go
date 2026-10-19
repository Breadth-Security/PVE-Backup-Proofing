package probe_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/pbv/internal/config"
	"git.breadthanalytics.com/tdiana/pbv/internal/model"
	"git.breadthanalytics.com/tdiana/pbv/internal/probe"
	"git.breadthanalytics.com/tdiana/pbv/internal/probe/probetest"
)

const winCmd = `powershell.exe -NoProfile -NonInteractive -Command Get-CimInstance Win32_Service | Where-Object { $_.StartMode -eq 'Auto' -and $_.State -ne 'Running' -and $_.DelayedAutoStart -ne $true } | ForEach-Object { $_.Name }`

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
	if o.Verdict != model.Failed || o.Reason != "check timed out: windows services" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}

func TestWindowsNonZeroExit(t *testing.T) {
	o := runWin(probetest.Scripted{winCmd: {ExitCode: 1}}, config.Checks{})
	if o.Verdict != model.Failed || o.Reason != "probe error: powershell exited 1" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}

// seqExec answers the service query with each output in turn, repeating
// the last, and counts the polls.
type seqExec struct {
	outs  []string
	polls *int
}

func (s seqExec) Exec(_ context.Context, argv []string) (probe.ExecResult, error) {
	i := min(*s.polls, len(s.outs)-1)
	*s.polls++
	return out(s.outs[i]), nil
}

func runWinSettle(t *testing.T, outs []string, deadline time.Duration) (probe.Outcome, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	polls := 0
	o := probe.Run(ctx, seqExec{outs, &polls}, probe.Input{OS: probe.Windows, WindowsSettlePoll: time.Millisecond})
	return o, polls
}

// Automatic services may still be starting when the agent first answers:
// PBP re-polls until none is left stopped.
func TestWindowsSettlesToProven(t *testing.T) {
	o, polls := runWinSettle(t, []string{"Spooler\r\nWinRM\r\n", "WinRM\r\n", ""}, time.Minute)
	if o.Verdict != model.Proven || polls != 3 {
		t.Fatalf("got %v %q after %d polls", o.Verdict, o.Reason, polls)
	}
}

// Only what is still stopped when the probe deadline comes is FAILED.
func TestWindowsSettleFailsWithWhatRemains(t *testing.T) {
	o, polls := runWinSettle(t, []string{"Spooler\r\nWinRM\r\n", "WinRM\r\n"}, 50*time.Millisecond)
	if o.Verdict != model.Failed || o.Reason != "stopped auto services: WinRM" || polls < 3 {
		t.Fatalf("got %v %q after %d polls", o.Verdict, o.Reason, polls)
	}
	if !reflect.DeepEqual(o.FailedUnits, []string{"WinRM"}) {
		t.Errorf("failed units %v", o.FailedUnits)
	}
}

// A re-poll cut short by the probe deadline keeps the last full answer.
func TestWindowsSettleDeadlineDuringPoll(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ex := &firstThenHang{first: "Spooler\r\n"}
	o := probe.Run(ctx, ex, probe.Input{OS: probe.Windows, WindowsSettlePoll: time.Millisecond, Checks: config.Checks{CommandTimeout: time.Hour}})
	if o.Verdict != model.Failed || o.Reason != "stopped auto services: Spooler" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}

type firstThenHang struct {
	first string
	done  bool
}

func (f *firstThenHang) Exec(ctx context.Context, _ []string) (probe.ExecResult, error) {
	if !f.done {
		f.done = true
		return out(f.first), nil
	}
	<-ctx.Done()
	return probe.ExecResult{}, ctx.Err()
}

// The probe ending before the first answer is ERROR, never BOOTED or FAILED.
func TestWindowsProbeEnded(t *testing.T) {
	hang := hanging(probetest.Scripted{}, winCmd)
	in := probe.Input{OS: probe.Windows, Checks: config.Checks{CommandTimeout: time.Hour}}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if o := probe.Run(ctx, hang, in); o.Verdict != model.Error || o.Reason != "probe timed out" {
		t.Errorf("deadline: got %v %q", o.Verdict, o.Reason)
	}

	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if o := probe.Run(ctx, hang, in); o.Verdict != model.Error || o.Reason != "probe cancelled" {
		t.Errorf("cancel: got %v %q", o.Verdict, o.Reason)
	}
}

func TestWindowsWrappedExecBlocked(t *testing.T) {
	o := runWin(errExec{err: fmt.Errorf("agent: %w", probe.ErrExecBlocked)}, config.Checks{})
	if o.Verdict != model.Booted || o.Reason != "agent cannot run commands" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}
