package sanitize

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

const br = "vmbr-pbp"

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestSanitizeQEMUReferenceVM(t *testing.T) {
	p := Sanitize(model.KindQEMU, map[string]string{
		"net0":    "virtio=BC:24:11:ED:57:35,bridge=vmbr0,firewall=1,tag=2",
		"onboot":  "1",
		"startup": "order=5",
		"serial0": "socket",
		"tags":    "prod;db",
		"scsi0":   "local-zfs:vm-100-disk-0,size=32G",
	}, br)
	if got := p.Set["net0"]; got != "virtio=BC:24:11:ED:57:35,bridge=vmbr-pbp,firewall=1" {
		t.Errorf("net0 = %q", got)
	}
	if p.Set["onboot"] != "0" || p.Set["tags"] != "pbp" {
		t.Errorf("onboot/tags not forced: %v", p.Set)
	}
	if !reflect.DeepEqual(p.Delete, []string{"startup"}) {
		t.Errorf("Delete = %v", p.Delete)
	}
	if len(p.Removed) != 0 || len(p.Unsafe) != 0 {
		t.Errorf("unexpected Removed/Unsafe: %v %v", p.Removed, p.Unsafe)
	}
	if _, ok := p.Set["scsi0"]; ok || contains(p.Delete, "scsi0") || contains(p.Delete, "serial0") {
		t.Errorf("disk/socket serial must be untouched: %v", p)
	}
}

func TestSanitizeQEMUPassthrough(t *testing.T) {
	p := Sanitize(model.KindQEMU, map[string]string{
		"hostpci0":  "0000:01:00.0,pcie=1",
		"usb0":      "host=1234:5678",
		"serial1":   "/dev/ttyUSB0",
		"parallel0": "/dev/parport0",
		"serial0":   "socket",
	}, br)
	if want := []string{"hostpci0", "parallel0", "serial1", "usb0"}; !reflect.DeepEqual(p.Delete, want) {
		t.Errorf("Delete = %v, want %v", p.Delete, want)
	}
	if len(p.Removed) != 4 {
		t.Fatalf("Removed = %v", p.Removed)
	}
	for _, r := range p.Removed {
		if r.Kind != "passthrough" || r.Key == "" {
			t.Errorf("bad piece %+v", r)
		}
	}
}

func TestSanitizeQEMUVirtiofsDeleted(t *testing.T) {
	p := Sanitize(model.KindQEMU, map[string]string{"virtiofs0": "shared-dir"}, br)
	if !contains(p.Delete, "virtiofs0") {
		t.Errorf("virtiofs0 not deleted: %v", p.Delete)
	}
	if !reflect.DeepEqual(p.Removed, []model.MissingPiece{{Kind: "passthrough", Key: "virtiofs0"}}) {
		t.Errorf("Removed = %v", p.Removed)
	}
}

func TestSanitizeHookscriptDeletedBothKinds(t *testing.T) {
	for _, k := range []model.GuestKind{model.KindQEMU, model.KindLXC} {
		p := Sanitize(k, map[string]string{"hookscript": "local:snippets/h.pl"}, br)
		if !contains(p.Delete, "hookscript") {
			t.Errorf("%s: hookscript not deleted: %v", k, p.Delete)
		}
		if len(p.Removed) != 0 {
			t.Errorf("%s: hookscript must not yield a piece: %v", k, p.Removed)
		}
	}
}

func TestSanitizeQEMUArgsUnsafe(t *testing.T) {
	p := Sanitize(model.KindQEMU, map[string]string{"args": "-device foo"}, br)
	if len(p.Unsafe) == 0 {
		t.Error("args must be unsafe")
	}
}

func TestSanitizeLXCNet(t *testing.T) {
	p := Sanitize(model.KindLXC, map[string]string{
		"net0": "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:00:00:01,ip=10.0.0.5/24,gw=10.0.0.1,rate=50",
	}, br)
	want := "name=eth0,bridge=vmbr-pbp,hwaddr=BC:24:11:00:00:01,ip=10.0.0.5/24,gw=10.0.0.1"
	if p.Set["net0"] != want {
		t.Errorf("net0 = %q", p.Set["net0"])
	}
}

func TestSanitizeLXCMounts(t *testing.T) {
	p := Sanitize(model.KindLXC, map[string]string{
		"mp0":  "/mnt/media,mp=/media",
		"mp1":  "local-zfs:subvol-114-disk-1,mp=/data,size=8G",
		"dev0": "/dev/dri/card0",
	}, br)
	if want := []string{"dev0", "mp0"}; !reflect.DeepEqual(p.Delete, want) {
		t.Errorf("Delete = %v", p.Delete)
	}
	want := []model.MissingPiece{
		{Kind: "passthrough", Key: "dev0"},
		{Kind: "bind-mount", Key: "mp0", Path: "/media"},
	}
	if !reflect.DeepEqual(p.Removed, want) {
		t.Errorf("Removed = %v", p.Removed)
	}
}

func TestSanitizeLXCRawUnsafe(t *testing.T) {
	p := Sanitize(model.KindLXC, map[string]string{"lxc": "lxc.cgroup2.devices.allow: c 195:* rwm"}, br)
	if len(p.Unsafe) == 0 || p.Unsafe[0] != "raw lxc.* config" {
		t.Errorf("Unsafe = %v", p.Unsafe)
	}
}

func TestSanitizeNetBridgeProperty(t *testing.T) {
	qemu := []string{
		"virtio=AA:BB:CC:DD:EE:01,bridge=vmbr0",
		"e1000=AA:BB:CC:DD:EE:02,bridge=vmbr1,tag=2,trunks=10;20",
		"virtio=AA:BB:CC:DD:EE:03",
		"virtio=AA:BB:CC:DD:EE:04,firewall=1,bridge=vmbr0,rate=5,link_down=1",
		"virtio=AA:BB:CC:DD:EE:05,bridge=vmbr0,mtu=1500,queues=4",
		"rtl8139=AA:BB:CC:DD:EE:06,tag=7,bridge=ovsbr0,trunks=10;20;30",
		"model=virtio,macaddr=AA:BB:CC:DD:EE:07,bridge=vmbr0",
		"vmxnet3=AA:BB:CC:DD:EE:08,bridge=vmbr0,firewall=0",
	}
	for _, v := range qemu {
		p := Sanitize(model.KindQEMU, map[string]string{"net0": v, "net3": v}, br)
		for _, k := range []string{"net0", "net3"} {
			got, ok := p.Set[k]
			if !ok {
				t.Fatalf("%q: %s missing from Set", v, k)
			}
			checkNet(t, v, got)
			if first := strings.SplitN(v, ",", 2)[0]; !strings.HasPrefix(got, first) {
				t.Errorf("%q: first field changed: %q", v, got)
			}
		}
	}
}

func TestSanitizeNetWithoutBridgeAppends(t *testing.T) {
	p := Sanitize(model.KindLXC, map[string]string{"net0": "name=eth0,ip=dhcp"}, br)
	if p.Set["net0"] != "name=eth0,ip=dhcp,bridge=vmbr-pbp" {
		t.Errorf("net0 = %q", p.Set["net0"])
	}
}

func TestSanitizeNetUnchangedStillInSet(t *testing.T) {
	p := Sanitize(model.KindLXC, map[string]string{"net0": "name=eth0,bridge=vmbr-pbp"}, br)
	if p.Set["net0"] != "name=eth0,bridge=vmbr-pbp" {
		t.Errorf("net0 = %q", p.Set["net0"])
	}
}

func checkNet(t *testing.T, in, out string) {
	t.Helper()
	n := 0
	for _, f := range strings.Split(out, ",") {
		k, v, _ := strings.Cut(f, "=")
		switch k {
		case "bridge":
			n++
			if v != br {
				t.Errorf("%q -> %q: wrong bridge", in, out)
			}
		case "tag", "trunks", "rate":
			t.Errorf("%q -> %q: %s not dropped", in, out, k)
		}
	}
	if n != 1 {
		t.Errorf("%q -> %q: want exactly one bridge, got %d", in, out, n)
	}
}

func TestSanitizeDeleteSorted(t *testing.T) {
	p := Sanitize(model.KindQEMU, map[string]string{
		"usb1": "host=1:2", "usb0": "host=3:4", "startup": "x", "hookscript": "h", "hostpci0": "a",
	}, br)
	if !sort.StringsAreSorted(p.Delete) || len(p.Delete) != 5 {
		t.Errorf("Delete = %v", p.Delete)
	}
}

func TestParseConfigText(t *testing.T) {
	txt := "#description line\n#more: stuff\nname: web\nnet0: virtio=AA,bridge=vmbr0\n\nonboot: 1\n[snap1]\nname: old\nnet0: other\n"
	got := ParseConfigText(txt)
	want := map[string]string{"name": "web", "net0": "virtio=AA,bridge=vmbr0", "onboot": "1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestParseConfigTextRawLXC(t *testing.T) {
	txt := "hostname: ct\nlxc.cgroup2.devices.allow: c 195:* rwm\nlxc.mount.entry: /dev/x dev/x none bind 0 0\n"
	got := ParseConfigText(txt)
	if got["hostname"] != "ct" || got["lxc"] != "lxc.cgroup2.devices.allow: c 195:* rwm\nlxc.mount.entry: /dev/x dev/x none bind 0 0" {
		t.Errorf("got %v", got)
	}
}

func TestSanitizeLXCNetBridgeFirstField(t *testing.T) {
	p := Sanitize(model.KindLXC, map[string]string{"net0": "bridge=vmbr0,name=eth0,tag=3"}, br)
	if p.Set["net0"] != "bridge=vmbr-pbp,name=eth0" {
		t.Errorf("net0 = %q", p.Set["net0"])
	}
}
