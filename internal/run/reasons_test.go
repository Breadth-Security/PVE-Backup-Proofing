package run

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/pve"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/sanitize"
)

// Reasons go into the notes and the notification: they name the guest by
// its own label, never PBP's temporary VMID, and carry no API internals
// (issue #73).
func TestScrub(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"unable to restore CT 990000 - unexpected end of archive", "unable to restore CT123 - unexpected end of archive"},
		{"VM 990000 is locked (backup)", "VM is locked (backup)"},
		{"start 990000: guest did not boot", "start: guest did not boot"},
		{"sanitize config 990000: pve api /nodes/pve1/qemu/990000/config: 500: boom", "see the guest's log"},
		{"x: Permission check failed (/vms/990000, VM.Config.Disk): Permission check failed (/vms/990000, VM.Config.Disk)", "see the guest's log"},
		{"read failed: read failed", "read failed"},
	} {
		if got := scrub(tc.in, 990000, "CT123"); got != tc.want {
			t.Errorf("scrub(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	long := scrub(strings.Repeat("é", 400), 990000, "CT123")
	if n := utf8.RuneCountInString(long); n > maxReasonRunes {
		t.Errorf("not cut: %d runes", n)
	}
}

func TestPermissionDenied(t *testing.T) {
	err := &pve.APIError{Status: 403, Path: "/nodes/pve1/qemu/990000/config", Message: "Permission check failed (/vms/990000, VM.Config.Disk)"}
	if priv, ok := permissionDenied(err); !ok || priv != "VM.Config.Disk" {
		t.Errorf("got %q %v", priv, ok)
	}
	task := errors.New("unable to restore CT 990000 - 403 Permission check failed (/, Sys.Modify)")
	if priv, ok := permissionDenied(task); !ok || priv != "Sys.Modify" {
		t.Errorf("task text: got %q %v", priv, ok)
	}
	if _, ok := permissionDenied(errors.New("no space left on device")); ok {
		t.Error("false positive")
	}
}

// assertPlainReason fails when a reason shows PBP internals (a temporary
// VMID of the 9900xx test ranges, API paths or status codes) or would not
// fit the notes line (issue #73).
func assertPlainReason(t *testing.T, who, reason string) {
	t.Helper()
	for _, bad := range []string{"9900", "pve api", "/api2/", "/nodes/", "Permission check failed"} {
		if strings.Contains(reason, bad) {
			t.Errorf("%s: reason %q shows %q", who, reason, bad)
		}
	}
	if n := utf8.RuneCountInString(reason); n > 180 {
		t.Errorf("%s: reason is %d runes, too long for the notes line: %q", who, n, reason)
	}
}

func TestScrubVolumeAndTarget(t *testing.T) {
	for in, want := range map[string]string{
		"lvcreate 'pve/vm-990000-disk-0' error: VG has insufficient free space": "lvcreate 'pve/vm-…-disk-0' error: VG has insufficient free space",
		"restore pbs:backup/ct/123/2026 to 990000: unable to read config":       "restore pbs:backup/ct/123/2026: unable to read config",
	} {
		if got := scrub(in, 990000, "CT123"); got != want {
			t.Errorf("scrub(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWithSuffixKeepsSuffix(t *testing.T) {
	long := strings.Repeat("x", 160)
	got := withSuffix(withSuffix(long, "; test copy not removed (retried at the next run)"), "; backup left protected until the next run")
	if n := utf8.RuneCountInString(got); n > maxNotedRunes {
		t.Errorf("%d runes", n)
	}
	if !strings.HasSuffix(got, "; backup left protected until the next run") {
		t.Errorf("suffix lost: %q", got)
	}
}

// The up-front refusal names what only root@pam can restore or remove,
// raw lines first, at most three per group (issue #140). A list too long
// for the notes line is cut; the docs pointer always stays.
func TestRootOnlyReason(t *testing.T) {
	kd := func(k, d string) sanitize.KeyDetail { return sanitize.KeyDetail{Key: k, Detail: d} }
	tail := ": only root@pam can (" + docsToken + ")"
	tests := []struct {
		raw  []string
		keys []sanitize.KeyDetail
		want string // exact, or the prefix before a cut ("…") when cut
		cut  bool
	}{
		{nil, []sanitize.KeyDetail{kd("dev1", "/dev/ttyUSB0"), kd("hookscript", "")},
			"PBP cannot proof dev1 (/dev/ttyUSB0), hookscript" + tail, false},
		{[]string{"lxc.apparmor.profile"}, nil,
			"PBP cannot proof raw lxc settings (lxc.apparmor.profile)" + tail, false},
		{[]string{"lxc.apparmor.profile", "lxc.idmap"}, []sanitize.KeyDetail{kd("dev0", "/dev/ttyUSB0")},
			"PBP cannot proof raw lxc settings (lxc.apparmor.profile, lxc.idmap), dev0 (/dev/ttyUSB0)" + tail, false},
		{[]string{"lxc.a", "lxc.b", "lxc.c", "lxc.d"}, nil,
			"PBP cannot proof raw lxc settings (lxc.a, lxc.b, lxc.c and 1 more)" + tail, false},
		{nil, []sanitize.KeyDetail{kd("usb0", "1-1"), kd("usb1", "1-2"), kd("usb2", "1-3"), kd("usb3", "1-4"), kd("usb4", "1-5")},
			"PBP cannot proof usb0 (1-1), usb1 (1-2), usb2 (1-3) and 2 more" + tail, false},
		{[]string{"lxc.cgroup2.devices.deny", "lxc.apparmor.profile", "lxc.idmap"}, []sanitize.KeyDetail{kd("dev0", "/dev/serial/by-id/usb-Silicon_Labs_CP2102N_USB_to_UART_Bridge-if00-port0")},
			"PBP cannot proof raw lxc settings (lxc.cgroup2.devices.deny, lxc.apparmor.profile, lxc.idmap), d", true},
	}
	for _, tc := range tests {
		got := rootOnlyReason(tc.raw, tc.keys)
		if n := utf8.RuneCountInString(got); n > maxReasonRunes {
			t.Errorf("%d runes, over the cap: %q", n, got)
		}
		if !tc.cut && got != tc.want {
			t.Errorf("rootOnlyReason(%q, %v)\n got  %q\n want %q", tc.raw, tc.keys, got, tc.want)
		}
		if tc.cut && (!strings.HasPrefix(got, tc.want) || !strings.HasSuffix(got, "…"+tail)) {
			t.Errorf("rootOnlyReason(%q, %v) = %q, want %q…%s", tc.raw, tc.keys, got, tc.want, tail)
		}
	}
}
