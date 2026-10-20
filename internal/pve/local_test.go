package pve

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"git.breadthanalytics.com/tdiana/Proxmox-Backup-Proofing/internal/model"
)

type fakeRun struct {
	out  string
	err  error
	name string
	args []string
}

func (f *fakeRun) run(_ context.Context, name string, args []string) ([]byte, error) {
	f.name, f.args = name, args
	return []byte(f.out), f.err
}

func TestLocalClientGuests(t *testing.T) {
	f := &fakeRun{out: `[{"type":"qemu","vmid":990001,"node":"pve1","name":"a","status":"running","tags":"pbp;x"},{"type":"storage","id":"s"}]`}
	c := NewLocal("/usr/bin/pvesh", f.run)
	got, err := c.Guests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != (model.GuestKey{Kind: model.KindQEMU, VMID: 990001}) || !slices.Equal(got[0].Tags, []string{"pbp", "x"}) {
		t.Errorf("guests = %+v", got)
	}
	want := []string{"get", "/cluster/resources", "--type", "vm", "--output-format", "json"}
	if f.name != "/usr/bin/pvesh" || !slices.Equal(f.args, want) {
		t.Errorf("ran %s %v, want %v", f.name, f.args, want)
	}
}

func TestLocalClientNetIface(t *testing.T) {
	f := &fakeRun{out: `{"iface":"vmbr9","type":"bridge","bridge_ports":"none"}`}
	got, err := NewLocal("pvesh", f.run).NetIface(context.Background(), "pve1", "vmbr9")
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "bridge" || got.BridgePorts != "none" {
		t.Errorf("iface = %+v", got)
	}
	want := []string{"get", "/nodes/pve1/network/vmbr9", "--output-format", "json"}
	if !slices.Equal(f.args, want) {
		t.Errorf("args = %v, want %v", f.args, want)
	}
}

func TestLocalClientVersion(t *testing.T) {
	f := &fakeRun{out: `{"version":"9.2.18","release":"9.2"}`}
	got, err := NewLocal("pvesh", f.run).Version(context.Background())
	if err != nil || got != "9.2.18" {
		t.Errorf("version = %q, %v", got, err)
	}
	if want := []string{"get", "/version", "--output-format", "json"}; !slices.Equal(f.args, want) {
		t.Errorf("args = %v, want %v", f.args, want)
	}
}

func TestLocalClientStorageStatus(t *testing.T) {
	f := &fakeRun{out: `{"active":1,"enabled":1,"content":"images,rootdir"}`}
	st, err := NewLocal("pvesh", f.run).StorageStatus(context.Background(), "pve1", "local-lvm")
	if err != nil || !st.Active || !st.Enabled || st.Content != "images,rootdir" {
		t.Errorf("status = %+v, %v", st, err)
	}
}

func TestLocalClientErrors(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeRun{err: boom}
	if _, err := NewLocal("pvesh", f.run).Version(context.Background()); !errors.Is(err, boom) {
		t.Errorf("run error = %v", err)
	}
	f = &fakeRun{out: "not json"}
	if _, err := NewLocal("pvesh", f.run).Version(context.Background()); err == nil {
		t.Error("bad JSON accepted")
	}
	if err := NewLocal("pvesh", f.run).do(context.Background(), http.MethodPost, "/x", nil, nil, nil); err == nil {
		t.Error("non-GET accepted")
	}
}

func TestLocalClientNodes(t *testing.T) {
	f := &fakeRun{out: `[{"node":"pve1","status":"online"},{"node":"pve2"}]`}
	got, err := NewLocal("pvesh", f.run).Nodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"pve1", "pve2"}) {
		t.Errorf("nodes = %v", got)
	}
	want := []string{"get", "/nodes", "--output-format", "json"}
	if !slices.Equal(f.args, want) {
		t.Errorf("args = %v, want %v", f.args, want)
	}
}
