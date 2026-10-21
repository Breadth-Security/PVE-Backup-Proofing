package probe_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/config"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/probe"
	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/probe/probetest"
)

// showResult is the command the probe runs to tell a nesting failure from
// another: exit status 226 is systemd's NAMESPACE failure.
func showResult(unit string) string {
	return "systemctl show -p Result -p ExecMainStatus -p Triggers -- " + unit
}

// nestingGuest is a container whose failed units are those seen on pve1 in
// an unprivileged Debian 12 container without nesting (issue #34), plus
// extra.
func nestingGuest(extra map[string]string) probetest.Scripted {
	ex := healthy()
	failed := "systemd-logind.service loaded failed failed User Login Management\n" +
		"systemd-networkd.service loaded failed failed Network Configuration\n" +
		"systemd-networkd.socket loaded failed failed Network Service Netlink Socket\n" +
		"tmp.mount loaded failed failed Temporary Directory /tmp\n"
	ex[showResult("systemd-logind.service")] = out("Result=exit-code\nExecMainStatus=226\n")
	ex[showResult("systemd-networkd.service")] = out("Result=exit-code\nExecMainStatus=226\n")
	ex[showResult("systemd-networkd.socket")] = out("Result=service-start-limit-hit\n")
	ex[showResult("tmp.mount")] = out("Result=exit-code\n")
	for unit, result := range extra {
		failed += unit + " loaded failed failed x\n"
		ex[showResult(unit)] = out(result)
	}
	ex[failedC] = out(failed)
	return ex
}

func runNesting(ex probetest.Scripted, noNesting bool) probe.Outcome {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return probe.Run(ctx, ex, probe.Input{OS: probe.Linux, OSType: "debian", NoNesting: noNesting,
		Checks: config.Checks{CommandTimeout: time.Second}})
}

// Without nesting, units that fail only because systemd cannot set up their
// sandbox (the same failures the production guest has) make the guest
// BOOTED with a reason naming them, never FAILED: the backup is not at fault.
func TestNoNestingSandboxFailuresAreBooted(t *testing.T) {
	o := runNesting(nestingGuest(nil), true)
	const want = "container without nesting: 4 units cannot start sandboxed (systemd-logind.service, systemd-networkd.service, systemd-networkd.socket, tmp.mount); enable nesting=1"
	if o.Verdict != model.Booted || o.Reason != want {
		t.Fatalf("got %v %q\nwant BOOTED %q", o.Verdict, o.Reason, want)
	}
	if !slices.Equal(o.FailedUnits, []string{"systemd-logind.service", "systemd-networkd.service", "systemd-networkd.socket", "tmp.mount"}) {
		t.Errorf("failed units = %q", o.FailedUnits)
	}
}

// Any other failure still counts: it is FAILED, naming only that unit.
// Seen on pve1: influxdb.service exited with status 0 (not 226), and a
// socket whose service failed for another reason.
func TestNoNestingOtherFailuresStillFail(t *testing.T) {
	for name, extra := range map[string]map[string]string{
		"service": {"influxdb.service": "Result=exit-code\nExecMainStatus=0\n"},
		"socket":  {"app.socket": "Result=service-start-limit-hit\n", "app.service": "Result=exit-code\nExecMainStatus=1\n"},
		"mount":   {"srv-data.mount": "Result=exit-code\n"},
	} {
		o := runNesting(nestingGuest(extra), true)
		if o.Verdict != model.Failed {
			t.Errorf("%s: got %v %q, want FAILED", name, o.Verdict, o.Reason)
			continue
		}
		for _, u := range []string{"systemd-logind.service", "tmp.mount"} {
			if containsWord(o.Reason, u) {
				t.Errorf("%s: reason %q names nesting unit %s", name, o.Reason, u)
			}
		}
		for unit := range extra {
			if !containsWord(o.Reason, unit) {
				t.Errorf("%s: reason %q does not name %s", name, o.Reason, unit)
			}
		}
	}
}

// A container with nesting, or a VM, is judged as before: every failed unit
// is FAILED.
func TestNestingSandboxFailuresStillFailWithNesting(t *testing.T) {
	o := runNesting(nestingGuest(nil), false)
	if o.Verdict != model.Failed || o.Reason != "failed: systemd-logind.service, systemd-networkd.service, systemd-networkd.socket, tmp.mount" {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}

// containsWord reports whether unit appears as a whole word in a reason.
func containsWord(reason, unit string) bool {
	return slices.Contains(strings.FieldsFunc(reason, func(r rune) bool { return r == ' ' || r == ',' || r == ':' }), unit)
}

// A check that failed too is named in the BOOTED reason, not hidden behind
// the nesting one (code review 5.11).
func TestNoNestingNamesFailedChecks(t *testing.T) {
	ex := nestingGuest(nil)
	ex[ssC] = out("")
	ex["cat /proc/net/tcp /proc/net/tcp6"] = out("  sl  local_address rem_address   st\n")
	o := runNesting(ex, true)
	if o.Verdict != model.Booted || !strings.HasSuffix(o.Reason, "; checks also failed: nginx:ports:80,443") {
		t.Fatalf("got %v %q", o.Verdict, o.Reason)
	}
}

// A socket whose service has another name is judged by that service, read
// from the socket's Triggers (code review 5.13).
func TestNoNestingSocketTriggers(t *testing.T) {
	ex := nestingGuest(map[string]string{
		"dbus-broker.service": "Result=exit-code\nExecMainStatus=226\n",
		"dbus.socket":         "Result=service-start-limit-hit\nTriggers=dbus-broker.service\n",
	})
	o := runNesting(ex, true)
	if o.Verdict != model.Booted || !slices.Contains(o.FailedUnits, "dbus.socket") || strings.Contains(o.Reason, "failed:") {
		t.Fatalf("got %v %q (failed units %v)", o.Verdict, o.Reason, o.FailedUnits)
	}
	if !strings.Contains(o.Reason, "dbus.socket") {
		t.Errorf("dbus.socket not counted as a nesting failure: %q", o.Reason)
	}
}
