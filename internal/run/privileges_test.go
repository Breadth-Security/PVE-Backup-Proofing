package run

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// privCalls are the Privileges calls the fake API recorded, in order.
func privCalls(f *runFixture) []string {
	var out []string
	for _, c := range f.api.CallLog() {
		if p, ok := strings.CutPrefix(c, "Privileges "); ok {
			out = append(out, p)
		}
	}
	return out
}

func TestPreflightFailsOnMissingCorePrivilege(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.api.Privs = map[string][]string{"/vms": {"VM.Allocate", "VM.Audit", "VM.Config.HWType", "VM.Config.Network",
		"VM.Config.Options", "VM.PowerMgmt", "VM.GuestAgent.Audit", "VM.GuestAgent.Unrestricted"}}
	err := Preflight(context.Background(), f.d)
	if err == nil || !strings.Contains(err.Error(), "token privileges: token pbp@pve!proof lacks VM.Backup on /vms") ||
		!strings.Contains(err.Error(), "pveum acl modify /vms") {
		t.Fatalf("err = %v", err)
	}
	if got := f.tgt.CallLog(); !slices.Equal(got, []string{"CheckNode"}) {
		t.Errorf("target calls %q: preflight went on after the privilege check", got)
	}
}

func TestPreflightFailsOnRestoreStorageAlsoSource(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.d.Cfg.Sources.Storages = []string{"local-zfs", "pbs"}
	f.api.Privs = map[string][]string{"/storage/local-zfs": {}}
	err := Preflight(context.Background(), f.d)
	if err == nil || !strings.Contains(err.Error(), "lacks Datastore.AllocateSpace, Datastore.Audit on /storage/local-zfs") {
		t.Errorf("err = %v", err)
	}
}

func TestPreflightIgnoresSourceStorageMisses(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.api.Privs = map[string][]string{"/storage/pbs": {}}
	if err := Preflight(context.Background(), f.d); err != nil {
		t.Fatal(err)
	}
	want := []string{"/vms", "/storage/local-zfs", "/nodes/pve1", "/sdn/zones/localnetwork"}
	if got := privCalls(f); !slices.Equal(got, want) {
		t.Errorf("privileges read on %q, want the core paths %q", got, want)
	}
}

func TestPreflightPrivilegesReadError(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.api.FailOn = func(method string, args []string) error {
		if method == "Privileges" && slices.Equal(args, []string{"/sdn/zones/localnetwork"}) {
			return errors.New("boom")
		}
		return nil
	}
	err := Preflight(context.Background(), f.d)
	if err == nil || !strings.Contains(err.Error(), "read the token's privileges on /sdn/zones/localnetwork") {
		t.Errorf("err = %v", err)
	}
}
