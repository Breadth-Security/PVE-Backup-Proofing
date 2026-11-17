package sanitize

import (
	"reflect"
	"testing"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
)

// rawLine parses one config line the way a backed-up config is read.
func rawLine(t *testing.T, line string) RawLine {
	t.Helper()
	lines := RawLXCLines(ParseConfigText(line + "\n"))
	if len(lines) != 1 {
		t.Fatalf("RawLXCLines(%q) = %v, want one line", line, lines)
	}
	return lines[0]
}

// Device-only raw lines only grant or bind-mount character devices, so PVE
// dropping them leaves the copy with less, never more (issue #141). Fields
// keep the runs of spaces real configs have.
func TestDeviceOnly(t *testing.T) {
	yes := []string{
		"lxc.cgroup2.devices.allow: a",
		"lxc.cgroup2.devices.allow: c 188:* rwm",
		"lxc.cgroup.devices.allow: b 8:* rwm",
		"lxc.mount.entry: /dev/serial/by-id  dev/serial/by-id  none bind,optional,create=dir",
		"lxc.mount.entry: /dev/ttyUSB0       dev/ttyUSB0       none bind,optional,create=file",
		"lxc.mount.entry: /dev/net/tun dev/net/tun none bind,create=file",
		"lxc.mount.entry: /dev/dri dev/dri none rbind,optional,create=dir",
		"lxc.mount.entry: /dev/bus/usb/001 dev/bus/usb/001 none bind,optional,create=dir",
		"lxc.mount.entry: /dev/apex_0 dev/apex_0 none optional,bind,create=file",
	}
	no := []string{
		"lxc.cgroup2.devices.deny: c 10:200 rwm",
		"lxc.mount.entry: /dev/sdb1 mnt/data ext4 defaults",
		"lxc.mount.entry: /dev/sdb1 mnt/data none bind,create=file",
		"lxc.mount.entry: /dev/disk/by-id/x mnt/x none bind",
		"lxc.mount.entry: /dev/zvol/rpool/x mnt/x none bind",
		"lxc.mount.entry: /mnt/media mnt/media none bind",
		"lxc.mount.entry: /dev/ttyUSB0 dev/ttyUSB0 none ro",
		"lxc.mount.entry: /dev/ttyUSB0",
		"lxc.mount.entry: /dev/../mnt/x mnt/x none bind",
		"lxc.mount.entry: /dev/net/tun2 dev/net/tun2 none bind",
		"lxc.autodev: 1",
		"lxc.hook.autodev: sh -c 'mknod -m 0666 ${LXC_ROOTFS_MOUNT}/dev/x c 1 1'",
		"lxc.apparmor.profile: unconfined",
		"lxc.idmap: u 0 100000 65536",
		"lxc.cap.drop:",
	}
	for _, l := range yes {
		if !DeviceOnly(rawLine(t, l)) {
			t.Errorf("DeviceOnly(%q) = false, want true", l)
		}
	}
	for _, l := range no {
		if DeviceOnly(rawLine(t, l)) {
			t.Errorf("DeviceOnly(%q) = true, want false", l)
		}
	}
}

func TestRawLXCLinesOrder(t *testing.T) {
	got := RawLXCLines(ParseConfigText("lxc.mount.entry: /dev/ttyUSB0 dev/ttyUSB0 none bind\nhostname: x\nlxc.cgroup2.devices.allow:  a \n"))
	want := []RawLine{
		{Key: "lxc.mount.entry", Value: "/dev/ttyUSB0 dev/ttyUSB0 none bind"},
		{Key: "lxc.cgroup2.devices.allow", Value: "a"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RawLXCLines = %q, want %q", got, want)
	}
	if got := RawLXCLines(map[string]string{"hostname": "x"}); got != nil {
		t.Errorf("RawLXCLines = %q, want none", got)
	}
}

func TestDevicePathAndKeepList(t *testing.T) {
	for v, want := range map[string]string{
		"/dev/net/tun":                 "/dev/net/tun",
		"/dev/net/tun,uid=0,mode=0666": "/dev/net/tun",
		"path=/dev/net/tun,mode=0666":  "/dev/net/tun",
		"mode=0666,path=/dev/fuse":     "/dev/fuse",
	} {
		if got := DevicePath(v); got != want {
			t.Errorf("DevicePath(%q) = %q, want %q", v, got, want)
		}
	}
	for _, p := range []string{"/dev/net/tun", "/dev/fuse", "/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom"} {
		if !KeptDevice(p) {
			t.Errorf("KeptDevice(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"/dev/net/tun2", "/dev/net", "/dev/kvm", "/dev/loop0", "/dev/loop-control", "/dev/ttyUSB0", "/dev/dri/renderD128", "/dev/net/../net/tun", ""} {
		if KeptDevice(p) {
			t.Errorf("KeptDevice(%q) = true, want false", p)
		}
	}
	got := KeptDevices(map[string]string{"dev10": "/dev/fuse", "dev2": "path=/dev/net/tun", "dev0": "/dev/ttyUSB0", "hostname": "x"})
	want := []KeyDetail{{Key: "dev2", Detail: "/dev/net/tun"}, {Key: "dev10", Detail: "/dev/fuse"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("KeptDevices = %v, want %v", got, want)
	}
}

// A keep-listed device stays in the copy: removing a devN is root@pam's
// alone, and a virtual device reaches nothing outside the container.
// Anything else is still removed.
func TestSanitizeKeepsVirtualDevices(t *testing.T) {
	p := Sanitize(model.KindLXC, map[string]string{
		"dev0": "/dev/net/tun,mode=0666",
		"dev1": "/dev/dri/card0",
		"dev2": "path=/dev/fuse",
	}, br)
	if want := []string{"dev1"}; !reflect.DeepEqual(p.Delete, want) {
		t.Errorf("Delete = %v, want %v", p.Delete, want)
	}
	if want := []model.MissingPiece{{Kind: "passthrough", Key: "dev1"}}; !reflect.DeepEqual(p.Removed, want) {
		t.Errorf("Removed = %v, want %v", p.Removed, want)
	}
}

// The device lines of a privileged container from issue #141: all
// device-only, so each distinct one is a piece the proof cannot cover.
const deviceLines = `lxc.cgroup2.devices.allow: a
lxc.cgroup2.devices.allow: c 188:* rwm
lxc.cgroup2.devices.allow: c 189:* rwm
lxc.mount.entry: /dev/serial/by-id  dev/serial/by-id  none bind,optional,create=dir
lxc.mount.entry: /dev/ttyUSB0       dev/ttyUSB0       none bind,optional,create=file
lxc.mount.entry: /dev/ttyUSB1       dev/ttyUSB1       none bind,optional,create=file
lxc.mount.entry: /dev/ttyACM0       dev/ttyACM0       none bind,optional,create=file
lxc.mount.entry: /dev/ttyACM1       dev/ttyACM1       none bind,optional,create=file
lxc.cgroup2.devices.allow: c 10:200 rwm
lxc.mount.entry: /dev/net/tun dev/net/tun none bind,create=file
lxc.mount.entry: /dev/ttyUSB0 dev/ttyUSB0 none bind,optional,create=file
lxc.mount.entry: /dev/serial/by-id/usb-x dev/ttyZB none bind,optional,create=file
`

func TestMissingFromOriginalDroppedDevices(t *testing.T) {
	got := MissingFromOriginal(model.KindLXC, ParseConfigText("hostname: x\n"+deviceLines))
	pt := func(key, path, detail string) model.MissingPiece {
		return model.MissingPiece{Kind: "passthrough", Key: key, Path: path, Detail: detail}
	}
	want := []model.MissingPiece{
		pt("lxc.cgroup2.devices.allow", "", "a"),
		pt("lxc.cgroup2.devices.allow", "", "c 188:* rwm"),
		pt("lxc.cgroup2.devices.allow", "", "c 189:* rwm"),
		pt("lxc.mount.entry", "/dev/serial/by-id", ""),
		pt("lxc.mount.entry", "/dev/ttyUSB0", ""),
		pt("lxc.mount.entry", "/dev/ttyUSB1", ""),
		pt("lxc.mount.entry", "/dev/ttyACM0", ""),
		pt("lxc.mount.entry", "/dev/ttyACM1", ""),
		pt("lxc.cgroup2.devices.allow", "", "c 10:200 rwm"),
		pt("lxc.mount.entry", "/dev/net/tun", ""),
		pt("lxc.mount.entry", "/dev/ttyZB", "/dev/serial/by-id/usb-x"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}

	// One line PVE drops that is not a device: the container is refused
	// before restoring, so no piece describes a copy that never exists.
	if got := MissingFromOriginal(model.KindLXC, ParseConfigText(deviceLines+"lxc.apparmor.profile: unconfined\n")); got != nil {
		t.Errorf("with a non-device line: got %v, want none", got)
	}
}

// PVE also reads "lxc.key = value"; such a line is classified like the
// colon form, and a raw line PBP cannot parse blocks (review finding).
func TestRawLXCLinesEqualsForm(t *testing.T) {
	got := RawLXCLines(ParseConfigText("lxc.apparmor.profile = unconfined\nlxc.mount.entry = /dev/sdb1 mnt/data none bind\nlxc.cgroup2.devices.allow= c 10:200 rwm\nlxc.garbage\n"))
	want := []RawLine{
		{Key: "lxc.apparmor.profile", Value: "unconfined"},
		{Key: "lxc.mount.entry", Value: "/dev/sdb1 mnt/data none bind"},
		{Key: "lxc.cgroup2.devices.allow", Value: "c 10:200 rwm"},
		{Key: "lxc.garbage"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RawLXCLines = %q\nwant %q", got, want)
	}
	raw, _ := RootOnly(model.KindLXC, ParseConfigText("lxc.mount.entry = /dev/sdb1 mnt/data none bind\nlxc.garbage\n"))
	if !reflect.DeepEqual(raw, []string{"lxc.garbage", "lxc.mount.entry"}) {
		t.Errorf("RootOnly raw = %q", raw)
	}
}

// Only a target under /dev/ is a path the guest's services can name; any
// other target gives the piece no Path, so it can never cover a unit
// (a target of "dev" would otherwise cover every absent /dev path).
func TestDroppedDeviceTargetOutsideDev(t *testing.T) {
	got := MissingFromOriginal(model.KindLXC, ParseConfigText(
		"lxc.mount.entry: /dev/dri dev none rbind,create=dir\nlxc.mount.entry: /dev/dri mnt/gpu none bind\nlxc.mount.entry: /dev/ttyUSB0 dev/ttyUSB0 none bind\n"))
	want := []model.MissingPiece{
		{Kind: "passthrough", Key: "lxc.mount.entry", Detail: "/dev/dri dev none rbind,create=dir"},
		{Kind: "passthrough", Key: "lxc.mount.entry", Detail: "/dev/dri mnt/gpu none bind"},
		{Kind: "passthrough", Key: "lxc.mount.entry", Path: "/dev/ttyUSB0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}
