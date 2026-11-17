package sanitize

import (
	"reflect"
	"testing"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
)

// RootOnly lists what only root@pam could restore or remove from the copy,
// read from PVE's own permission checks (issue #140). Mapped devices need
// Mapping.Use instead, a grant, so they are not listed.
func TestRootOnly(t *testing.T) {
	kd := func(k, d string) KeyDetail { return KeyDetail{Key: k, Detail: d} }
	tests := []struct {
		name string
		kind model.GuestKind
		cfg  map[string]string
		raw  []string
		keys []KeyDetail
	}{
		{"ct real device", model.KindLXC, map[string]string{"dev0": "/dev/ttyUSB0"}, nil, []KeyDetail{kd("dev0", "/dev/ttyUSB0")}},
		{"ct no prefix match", model.KindLXC, map[string]string{"dev0": "/dev/net/tun2,mode=0666"}, nil, []KeyDetail{kd("dev0", "/dev/net/tun2")}},
		{"ct hookscript", model.KindLXC, map[string]string{"hookscript": "local:snippets/x.sh"}, nil, []KeyDetail{kd("hookscript", "")}},
		{"ct kept devices", model.KindLXC, map[string]string{"dev0": "/dev/net/tun", "dev1": "path=/dev/fuse,mode=0666"}, nil, nil},
		{"ct device-only raw lines", model.KindLXC, ParseConfigText(deviceLines), nil, nil},
		{"ct mixed raw lines", model.KindLXC, ParseConfigText("lxc.cgroup2.devices.allow: a\nlxc.apparmor.profile: unconfined\n"), []string{"lxc.apparmor.profile"}, nil},
		{"ct raw lines sorted", model.KindLXC, ParseConfigText("lxc.idmap: u 0 100000 65536\nlxc.apparmor.profile: unconfined\nlxc.idmap: g 0 100000 65536\n"), []string{"lxc.apparmor.profile", "lxc.idmap"}, nil},
		{"ct raw and keys", model.KindLXC, map[string]string{"lxc": "lxc.idmap: u 0 1 2", "dev0": "/dev/ttyUSB0"}, []string{"lxc.idmap"}, []KeyDetail{kd("dev0", "/dev/ttyUSB0")}},
		{"ct order", model.KindLXC, map[string]string{"dev10": "/dev/ttyUSB1", "dev2": "/dev/ttyUSB0", "hookscript": "local:snippets/x.sh"}, nil,
			[]KeyDetail{kd("hookscript", ""), kd("dev2", "/dev/ttyUSB0"), kd("dev10", "/dev/ttyUSB1")}},
		{"vm hookscript", model.KindQEMU, map[string]string{"hookscript": "local:snippets/x.sh"}, nil, []KeyDetail{kd("hookscript", "")}},
		{"vm parallel", model.KindQEMU, map[string]string{"parallel0": "/dev/parport0"}, nil, []KeyDetail{kd("parallel0", "/dev/parport0")}},
		{"vm ivshmem", model.KindQEMU, map[string]string{"ivshmem": "size=32"}, nil, []KeyDetail{kd("ivshmem", "")}},
		{"vm serial device", model.KindQEMU, map[string]string{"serial0": "/dev/ttyS0"}, nil, []KeyDetail{kd("serial0", "/dev/ttyS0")}},
		{"vm usb host", model.KindQEMU, map[string]string{"usb0": "host=1234:5678,usb3=1"}, nil, []KeyDetail{kd("usb0", "1234:5678")}},
		{"vm usb bare", model.KindQEMU, map[string]string{"usb0": "1-1.2"}, nil, []KeyDetail{kd("usb0", "1-1.2")}},
		{"vm pci bare", model.KindQEMU, map[string]string{"hostpci0": "0000:01:00.0"}, nil, []KeyDetail{kd("hostpci0", "0000:01:00.0")}},
		{"vm pci host", model.KindQEMU, map[string]string{"hostpci0": "host=0000:01:00.0,pcie=1"}, nil, []KeyDetail{kd("hostpci0", "0000:01:00.0")}},
		{"vm pci mapped romfile", model.KindQEMU, map[string]string{"hostpci0": "mapping=gpu,romfile=x.rom"}, nil, []KeyDetail{kd("hostpci0", "romfile")}},
		{"vm allowed", model.KindQEMU, map[string]string{
			"serial0": "socket", "usb0": "spice", "usb1": "host=spice", "usb2": "mapping=stick",
			"hostpci0": "mapping=gpu", "rng0": "source=/dev/hwrng", "virtiofs0": "dirid=x",
		}, nil, nil},
		{"vm order", model.KindQEMU, map[string]string{"usb10": "host=1-1", "usb2": "host=1-2", "hostpci1": "01:00.0", "ivshmem": "size=1", "hookscript": "x"}, nil,
			[]KeyDetail{kd("hookscript", ""), kd("ivshmem", ""), kd("hostpci1", "01:00.0"), kd("usb2", "1-2"), kd("usb10", "1-1")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, keys := RootOnly(tc.kind, tc.cfg)
			if !reflect.DeepEqual(raw, tc.raw) {
				t.Errorf("raw = %q, want %q", raw, tc.raw)
			}
			if !reflect.DeepEqual(keys, tc.keys) {
				t.Errorf("keys = %v, want %v", keys, tc.keys)
			}
		})
	}
}
