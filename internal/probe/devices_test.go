package probe_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/probe"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/probe/probetest"
)

// Devices a container copy was restored without: PVE drops raw lxc lines
// for PBP's token, so units that need those devices fail (issue #141).
func devPiece(path, detail string) model.MissingPiece {
	return model.MissingPiece{Kind: "passthrough", Key: "lxc.mount.entry", Path: path, Detail: detail}
}

var allowPiece = model.MissingPiece{Kind: "passthrough", Key: "lxc.cgroup2.devices.allow", Detail: "c 188:* rwm"}

func journalC(unit string) string { return "journalctl -u " + unit + " -b --no-pager -n 50 -o cat" }

// zigbee is a guest whose z2m.service failed, with its journal.
func zigbee(journal string) probetest.Scripted {
	ex := failing(map[string]string{"z2m.service": "Type=simple\n"})
	ex[journalC("z2m.service")] = out(journal)
	return ex
}

func TestDeviceInJournalCovered(t *testing.T) {
	ex := zigbee("Starting Zigbee2MQTT…\nError: open(\"/dev/ttyUSB0\"): No such file or directory\n")
	ex["test -e /dev/ttyUSB0"] = probe.ExecResult{ExitCode: 1}
	o := runMissing(ex, allowPiece, devPiece("/dev/ttyUSB0", ""))
	if o.Verdict != model.Proven || len(o.FailedUnits) != 0 {
		t.Fatalf("got %+v", o)
	}
	if c := coveredMap(o)["z2m.service"]; c.Piece != devPiece("/dev/ttyUSB0", "") {
		t.Fatalf("covered %+v", o.NotCovered)
	}
}

// A service logs the path inside the container, under a bound directory.
func TestDeviceUnderBoundDirectoryCovered(t *testing.T) {
	ex := zigbee("cannot open /dev/serial/by-id/usb-x-port0: no such device\n")
	ex["test -e /dev/serial/by-id/usb-x-port0"] = probe.ExecResult{ExitCode: 1}
	o := runMissing(ex, devPiece("/dev/serial/by-id", ""))
	if o.Verdict != model.Proven || coveredMap(o)["z2m.service"].Piece.Path != "/dev/serial/by-id" {
		t.Fatalf("got %+v", o)
	}
}

// The piece is matched on its target, the path the guest sees.
func TestDeviceMatchedOnTarget(t *testing.T) {
	ex := zigbee("/dev/ttyZB: No such file or directory\n")
	ex["test -e /dev/ttyZB"] = probe.ExecResult{ExitCode: 1}
	o := runMissing(ex, devPiece("/dev/ttyZB", "/dev/serial/by-id/usb-x"))
	if o.Verdict != model.Proven {
		t.Fatalf("got %+v", o)
	}
	ex = zigbee("/dev/serial/by-id/usb-x: No such file or directory\n")
	ex["test -e /dev/serial/by-id/usb-x"] = probe.ExecResult{ExitCode: 1}
	if o := runMissing(ex, devPiece("/dev/ttyZB", "/dev/serial/by-id/usb-x")); o.Verdict != model.Failed {
		t.Fatalf("host source matched: %+v", o)
	}
}

func TestDeviceNotCovered(t *testing.T) {
	present := zigbee("open /dev/ttyUSB0: permission denied\n")
	present["test -e /dev/ttyUSB0"] = out("")
	longer := zigbee("open /dev/ttyUSB10 failed\n")
	longer["test -e /dev/ttyUSB10"] = probe.ExecResult{ExitCode: 1}
	journalFails := zigbee("")
	journalFails[journalC("z2m.service")] = probe.ExecResult{ExitCode: 1, Stdout: "/dev/ttyUSB0 missing\n"}
	journalFails["test -e /dev/ttyUSB0"] = probe.ExecResult{ExitCode: 1}
	for name, tc := range map[string]struct {
		ex    probe.Executor
		piece model.MissingPiece
	}{
		"present in the copy": {present, devPiece("/dev/ttyUSB0", "")},
		"longer name":         {longer, devPiece("/dev/ttyUSB1", "")},
		"journal fails":       {journalFails, devPiece("/dev/ttyUSB0", "")},
	} {
		if o := runMissing(tc.ex, tc.piece); o.Verdict != model.Failed || len(o.NotCovered) != 0 {
			t.Errorf("%s: got %+v", name, o)
		}
	}
}

// recorder records every command the probe runs.
type recorder struct {
	probe.Executor
	mu    sync.Mutex
	calls []string
}

func (r *recorder) Exec(ctx context.Context, argv []string) (probe.ExecResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, strings.Join(argv, " "))
	r.mu.Unlock()
	return r.Executor.Exec(ctx, argv)
}

func (r *recorder) ran(prefix string) bool {
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// Only a guest with dropped devices that have a path reads unit journals.
func TestJournalReadOnlyForDevicePaths(t *testing.T) {
	for name, missing := range map[string][]model.MissingPiece{
		"no pieces":          nil,
		"allow lines only":   {allowPiece},
		"a bind mount":       {{Kind: "bind-mount", Key: "mp0", Path: "/media"}},
		"a removed dev0 key": {{Kind: "passthrough", Key: "dev0"}},
	} {
		r := &recorder{Executor: zigbee("/dev/ttyUSB0 missing\n")}
		if o := runMissing(r, missing...); o.Verdict != model.Failed {
			t.Errorf("%s: got %+v", name, o)
		}
		if r.ran("journalctl -u") {
			t.Errorf("%s: read a unit journal: %q", name, r.calls)
		}
	}
}

// A transport error reading the journal leaves the unit untraced, and the
// guest cannot be judged.
func TestJournalExecErrorUntraced(t *testing.T) {
	o := runMissing(cmdErr{zigbee(""), "journalctl", errors.New("boom")}, devPiece("/dev/ttyUSB0", ""))
	if len(o.NotCovered) != 0 || o.Verdict == model.Proven {
		t.Fatalf("got %+v", o)
	}
}

// A unit the journal cannot tie to a dropped device fails the guest, and
// the reason says which devices the copy lacks and how to ignore the unit.
func TestDeviceHint(t *testing.T) {
	ex := zigbee("crashed\n")
	o := runMissing(ex, allowPiece, devPiece("/dev/ttyUSB0", ""), devPiece("/dev/ttyACM0", ""))
	want := "failed: z2m.service · copy has no /dev/ttyUSB0, /dev/ttyACM0 (raw lxc lines); to skip a unit that needs them, use checks.guests ignore_units"
	if o.Verdict != model.Failed || o.Reason != want {
		t.Fatalf("got %s %q\nwant %q", o.Verdict, o.Reason, want)
	}
	o = runMissing(zigbee("crashed\n"), allowPiece)
	want = "failed: z2m.service · copy has no device rules (raw lxc lines); to skip a unit that needs them, use checks.guests ignore_units"
	if o.Reason != want {
		t.Fatalf("got %q\nwant %q", o.Reason, want)
	}
	if o := runMissing(zigbee("crashed\n"), model.MissingPiece{Kind: "passthrough", Key: "dev0"}); o.Reason != "failed: z2m.service" {
		t.Fatalf("hint without dropped raw lines: %q", o.Reason)
	}
}

// The hint survives the notes line's cut (160 runes): the unit list is cut
// before it, and at most two devices are named.
func TestDeviceHintFitsTheNotes(t *testing.T) {
	props := map[string]string{}
	for _, u := range []string{"zigbee2mqtt", "mosquitto", "homeassistant", "node-red", "esphome-dashboard", "frigate"} {
		props[u+".service"] = "Type=simple\n"
	}
	o := runMissing(failing(props), devPiece("/dev/ttyUSB0", ""), devPiece("/dev/ttyACM0", ""), devPiece("/dev/serial/by-id", ""))
	tail := " · copy has no /dev/ttyUSB0, /dev/ttyACM0 and 1 more (raw lxc lines); to skip a unit that needs them, use checks.guests ignore_units"
	if !strings.HasPrefix(o.Reason, "failed: ") || !strings.HasSuffix(o.Reason, "…"+tail) {
		t.Fatalf("reason %q", o.Reason)
	}
	if n := len([]rune(o.Reason)); n > 160 {
		t.Fatalf("%d runes: %q", n, o.Reason)
	}
}
