package probe_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

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

func TestWindowsNonZeroExit(t *testing.T) {
	o := runWin(probetest.Scripted{winCmd: {ExitCode: 1}}, config.Checks{})
	if o.Verdict != model.Failed || o.Reason != "probe error: powershell exited 1" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}
