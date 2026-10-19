package pve

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"git.breadthanalytics.com/tdiana/pbv/internal/model"
)

const realGuestConfig = `{"data":{"agent":"1","boot":"order=scsi0;net0","cores":4,"cpu":"host","digest":"abc123","memory":"16500","name":"delta1","net0":"virtio=BC:24:11:ED:57:35,bridge=vmbr0,firewall=1,tag=2","onboot":1,"ostype":"l26","scsi0":"VMStorage_Local_M.2_LVMThin:vm-100-disk-0,discard=on,format=raw,iothread=1,size=33G,ssd=1","serial0":"socket","startup":"order=5","tags":"development"}}`

type captured struct {
	method, path, rawQuery string
	form                   url.Values
}

func capture(t *testing.T, reply string) (*Client, *captured) {
	t.Helper()
	got := &captured{}
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.method, got.path, got.rawQuery = r.Method, r.URL.Path, r.URL.RawQuery
		got.form, _ = url.ParseQuery(string(b))
		w.Write([]byte(reply))
	}))
	return c, got
}

func TestGuests(t *testing.T) {
	c, got := capture(t, `{"data":[
		{"id":"qemu/100","type":"qemu","vmid":100,"node":"pve1","name":"delta1","status":"running","tags":"development;web"},
		{"id":"lxc/101","type":"lxc","vmid":101,"node":"pve2","name":"kasm","status":"stopped"},
		{"id":"storage/pve1/local","type":"storage"}]}`)
	g, err := c.Guests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.path != "/api2/json/cluster/resources" || got.rawQuery != "type=vm" {
		t.Errorf("request = %+v", got)
	}
	want := []GuestRef{
		{Key: model.GuestKey{Kind: model.KindQEMU, VMID: 100}, Node: "pve1", Name: "delta1", Status: "running", Tags: []string{"development", "web"}},
		{Key: model.GuestKey{Kind: model.KindLXC, VMID: 101}, Node: "pve2", Name: "kasm", Status: "stopped"},
	}
	if !reflect.DeepEqual(g, want) {
		t.Errorf("got %+v\nwant %+v", g, want)
	}
}

func TestRestoreQEMU(t *testing.T) {
	c, got := capture(t, `{"data":"UPID:pve1:1:qmrestore"}`)
	upid, err := c.Restore(context.Background(), "pve1", model.KindQEMU, 9100, "PBS:backup/vm/100/2026-07-26T06:00:03Z", "scratch")
	if err != nil || upid != "UPID:pve1:1:qmrestore" {
		t.Fatalf("upid=%q err=%v", upid, err)
	}
	want := url.Values{"archive": {"PBS:backup/vm/100/2026-07-26T06:00:03Z"}, "vmid": {"9100"}, "storage": {"scratch"}, "unique": {"1"}}
	if got.method != "POST" || got.path != "/api2/json/nodes/pve1/qemu" || !reflect.DeepEqual(got.form, want) {
		t.Errorf("request = %+v", got)
	}
}

func TestRestoreLXC(t *testing.T) {
	c, got := capture(t, `{"data":"UPID:pve1:2:vzrestore"}`)
	if _, err := c.Restore(context.Background(), "pve1", model.KindLXC, 9101, "PBS:backup/ct/101/x", "scratch"); err != nil {
		t.Fatal(err)
	}
	want := url.Values{"ostemplate": {"PBS:backup/ct/101/x"}, "vmid": {"9101"}, "storage": {"scratch"}, "restore": {"1"}, "unique": {"1"}}
	if got.method != "POST" || got.path != "/api2/json/nodes/pve1/lxc" || !reflect.DeepEqual(got.form, want) {
		t.Errorf("request = %+v", got)
	}
}

func TestGuestConfigStringifies(t *testing.T) {
	c, got := capture(t, `{"data":{"cores":4,"memory":"16500","onboot":1,"digest":"zzz","template":true}}`)
	cfg, err := c.GuestConfig(context.Background(), "pve1", model.KindQEMU, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got.method != "GET" || got.path != "/api2/json/nodes/pve1/qemu/100/config" {
		t.Errorf("request = %+v", got)
	}
	want := map[string]string{"cores": "4", "memory": "16500", "onboot": "1", "template": "true"}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("got %v", cfg)
	}
}

func TestGuestConfigReal(t *testing.T) {
	c, _ := capture(t, realGuestConfig)
	cfg, err := c.GuestConfig(context.Background(), "pve1", model.KindQEMU, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["digest"]; ok {
		t.Error("digest not dropped")
	}
	if len(cfg) != 13 || cfg["cores"] != "4" || cfg["onboot"] != "1" || cfg["boot"] != "order=scsi0;net0" ||
		cfg["scsi0"] != "VMStorage_Local_M.2_LVMThin:vm-100-disk-0,discard=on,format=raw,iothread=1,size=33G,ssd=1" {
		t.Errorf("cfg = %v", cfg)
	}
}

func TestGuestConfigLXCRawLines(t *testing.T) {
	c, got := capture(t, `{"data":{"hostname":"kasm","lxc":[["lxc.cgroup2.devices.allow","c 195:* rwm"],["lxc.mount.entry","/dev/nvidia0 dev/nvidia0 none bind,optional,create=file"]]}}`)
	cfg, err := c.GuestConfig(context.Background(), "pve1", model.KindLXC, 101)
	if err != nil {
		t.Fatal(err)
	}
	if got.path != "/api2/json/nodes/pve1/lxc/101/config" {
		t.Errorf("path = %q", got.path)
	}
	want := "lxc.cgroup2.devices.allow: c 195:* rwm\nlxc.mount.entry: /dev/nvidia0 dev/nvidia0 none bind,optional,create=file"
	if cfg["lxc"] != want || cfg["hostname"] != "kasm" {
		t.Errorf("cfg = %q", cfg)
	}
}

func TestSetGuestConfigMethodByKind(t *testing.T) {
	for _, tc := range []struct {
		kind   model.GuestKind
		method string
	}{{model.KindQEMU, "POST"}, {model.KindLXC, "PUT"}} {
		c, got := capture(t, `{"data":null}`)
		err := c.SetGuestConfig(context.Background(), "pve1", tc.kind, 9100,
			map[string]string{"onboot": "0", "net0": "virtio,bridge=vmbr9"}, []string{"hostpci0", "usb0"})
		if err != nil {
			t.Fatal(err)
		}
		want := url.Values{"onboot": {"0"}, "net0": {"virtio,bridge=vmbr9"}, "delete": {"hostpci0,usb0"}}
		if got.method != tc.method || got.path != "/api2/json/nodes/pve1/"+string(tc.kind)+"/9100/config" || !reflect.DeepEqual(got.form, want) {
			t.Errorf("%s: request = %+v", tc.kind, got)
		}
	}
}

func TestSetGuestConfigNoDelete(t *testing.T) {
	c, got := capture(t, `{"data":null}`)
	if err := c.SetGuestConfig(context.Background(), "pve1", model.KindQEMU, 1, map[string]string{"a": "b"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.form["delete"]; ok {
		t.Errorf("delete sent: %v", got.form)
	}
}

func TestOriginalConfig(t *testing.T) {
	c, got := capture(t, `{"data":"#note\nname: delta1\ncores: 4\n"}`)
	s, err := c.OriginalConfig(context.Background(), "pve1", "PBS:backup/vm/100/2026-07-26T06:00:03Z")
	if err != nil || s != "#note\nname: delta1\ncores: 4\n" {
		t.Fatalf("s=%q err=%v", s, err)
	}
	if got.method != "GET" || got.path != "/api2/json/nodes/pve1/vzdump/extractconfig" ||
		got.rawQuery != "volume=PBS%3Abackup%2Fvm%2F100%2F2026-07-26T06%3A00%3A03Z" {
		t.Errorf("request = %+v", got)
	}
}

func TestPowerOps(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		call   func(c *Client) (string, error)
		path   string
		form   url.Values
		method string
	}{
		{"start", func(c *Client) (string, error) { return c.Start(ctx, "pve1", model.KindQEMU, 9100) }, "/api2/json/nodes/pve1/qemu/9100/status/start", url.Values{}, "POST"},
		{"shutdown", func(c *Client) (string, error) { return c.Shutdown(ctx, "pve1", model.KindLXC, 9101) }, "/api2/json/nodes/pve1/lxc/9101/status/shutdown", url.Values{"timeout": {"60"}}, "POST"},
		{"stop", func(c *Client) (string, error) { return c.Stop(ctx, "pve1", model.KindQEMU, 9100) }, "/api2/json/nodes/pve1/qemu/9100/status/stop", url.Values{}, "POST"},
	}
	for _, tc := range cases {
		c, got := capture(t, `{"data":"UPID:x"}`)
		upid, err := tc.call(c)
		if err != nil || upid != "UPID:x" {
			t.Fatalf("%s: upid=%q err=%v", tc.name, upid, err)
		}
		if got.method != tc.method || got.path != tc.path || !reflect.DeepEqual(got.form, tc.form) {
			t.Errorf("%s: request = %+v", tc.name, got)
		}
	}
}

func TestDestroy(t *testing.T) {
	c, got := capture(t, `{"data":"UPID:d"}`)
	upid, err := c.Destroy(context.Background(), "pve1", model.KindLXC, 9101)
	if err != nil || upid != "UPID:d" {
		t.Fatalf("upid=%q err=%v", upid, err)
	}
	if got.method != "DELETE" || got.path != "/api2/json/nodes/pve1/lxc/9101" || got.rawQuery != "destroy-unreferenced-disks=1&purge=1" {
		t.Errorf("request = %+v", got)
	}
}

func TestGuestStatus(t *testing.T) {
	c, got := capture(t, `{"data":{"status":"running","vmid":9100}}`)
	s, err := c.GuestStatus(context.Background(), "pve1", model.KindQEMU, 9100)
	if err != nil || s != "running" || got.path != "/api2/json/nodes/pve1/qemu/9100/status/current" {
		t.Errorf("s=%q err=%v req=%+v", s, err, got)
	}
}

func TestAgentPing(t *testing.T) {
	c, got := capture(t, `{"data":{}}`)
	if err := c.AgentPing(context.Background(), "pve1", 9100); err != nil {
		t.Fatal(err)
	}
	if got.method != "POST" || got.path != "/api2/json/nodes/pve1/qemu/9100/agent/ping" {
		t.Errorf("request = %+v", got)
	}
}

func TestAgentOSInfo(t *testing.T) {
	c, got := capture(t, `{"data":{"result":{"id":"debian","name":"Debian GNU/Linux","version-id":"12"}}}`)
	o, err := c.AgentOSInfo(context.Background(), "pve1", 9100)
	if err != nil || o != (OSInfo{ID: "debian", Name: "Debian GNU/Linux"}) {
		t.Errorf("o=%+v err=%v", o, err)
	}
	if got.method != "GET" || got.path != "/api2/json/nodes/pve1/qemu/9100/agent/get-osinfo" {
		t.Errorf("request = %+v", got)
	}
}

func TestAgentExec(t *testing.T) {
	c, got := capture(t, `{"data":{"pid":4242}}`)
	pid, err := c.AgentExec(context.Background(), "pve1", 9100, []string{"sh", "-c", "echo hi"})
	if err != nil || pid != 4242 {
		t.Fatalf("pid=%d err=%v", pid, err)
	}
	if got.method != "POST" || got.path != "/api2/json/nodes/pve1/qemu/9100/agent/exec" {
		t.Errorf("request = %+v", got)
	}
	if !reflect.DeepEqual(got.form["command"], []string{"sh", "-c", "echo hi"}) {
		t.Errorf("form = %v", got.form)
	}
}

func TestAgentExecStatus(t *testing.T) {
	c, got := capture(t, `{"data":{"exited":1,"exitcode":3,"out-data":"hi\n","err-data":"oops"}}`)
	st, err := c.AgentExecStatus(context.Background(), "pve1", 9100, 4242)
	if err != nil {
		t.Fatal(err)
	}
	if got.method != "GET" || got.path != "/api2/json/nodes/pve1/qemu/9100/agent/exec-status" || got.rawQuery != "pid=4242" {
		t.Errorf("request = %+v", got)
	}
	if st != (AgentExecStatus{Exited: true, ExitCode: 3, OutData: "hi\n", ErrData: "oops"}) {
		t.Errorf("st = %+v", st)
	}
}
