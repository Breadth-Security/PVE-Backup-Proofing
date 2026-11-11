package run

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/model"
	"git.breadthanalytics.com/tdiana/PVE-Backup-Proofing/internal/pve"
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

// hasEnv reports whether an environment error of s contains sub.
func hasEnv(s model.RunSummary, sub string) bool {
	return slices.ContainsFunc(s.EnvironmentErrors, func(e string) bool { return strings.Contains(e, sub) })
}

func listed(f *runFixture, storage string) bool {
	return slices.ContainsFunc(f.api.CallLog(), func(c string) bool { return strings.HasPrefix(c, "ListBackups pve1 "+storage) })
}

func TestRunSkipsSourceStorageWithoutPrivileges(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	f.add("pbs-sync", model.KindQEMU, 102, 1024, "")
	f.api.Privs = map[string][]string{"/storage/pbs": {"Datastore.Audit"}}
	s := f.run(context.Background(), Options{Trigger: "manual"})
	if slices.Contains(f.api.CallLog(), "ListBackups pve1 pbs") || !listed(f, "pbs-sync") {
		t.Errorf("calls %q: want pbs-sync listed, pbs not", f.api.CallLog())
	}
	if _, ok := byVMID(t, s)[102]; !ok {
		t.Error("guest on pbs-sync not proofed")
	}
	if !hasEnv(s, "storage pbs not read: token pbp@pve!proof lacks Datastore.AllocateSpace on /storage/pbs") {
		t.Errorf("env = %q", s.EnvironmentErrors)
	}
	if s.Severity() != "warning" || s.RunError != "" {
		t.Errorf("severity %s, run error %q", s.Severity(), s.RunError)
	}
}

func TestRunSkipsSourceWhenPrivilegesUnreadable(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs-sync", model.KindQEMU, 102, 1024, "")
	f.api.FailOn = func(method string, args []string) error {
		if method == "Privileges" && slices.Equal(args, []string{"/storage/pbs"}) {
			return errors.New("boom")
		}
		return nil
	}
	s := f.run(context.Background(), Options{Trigger: "manual"})
	if !hasEnv(s, "storage pbs not read: cannot read the token's privileges on /storage/pbs") {
		t.Errorf("env = %q", s.EnvironmentErrors)
	}
	if _, ok := byVMID(t, s)[102]; !ok {
		t.Error("guest on pbs-sync not proofed")
	}
}

func TestRunWarnsOnSourceStorageWithNoBackups(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs-sync", model.KindQEMU, 102, 1024, "")
	f.d.Cfg.Sources.Storages = []string{"pbs", "pbs-sync"} // pbs configured, but empty
	s := f.run(context.Background(), Options{Trigger: "manual"})
	if !hasEnv(s, "storage pbs lists no backups for the token") || hasEnv(s, "storage pbs-sync lists no backups") {
		t.Errorf("env = %q", s.EnvironmentErrors)
	}
	if s.Severity() != "warning" {
		t.Errorf("severity %s", s.Severity())
	}
}

func TestRunNoWarningWhenOnlyInRangeBackups(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs", model.KindQEMU, 990005, 1024, "")
	f.add("pbs-sync", model.KindQEMU, 102, 1024, "")
	s := f.run(context.Background(), Options{Trigger: "manual"})
	if hasEnv(s, "lists no backups") {
		t.Errorf("env = %q", s.EnvironmentErrors)
	}
}

func TestRunGuestDoesNotWarnForOtherGuestsBackups(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	f.add("pbs-sync", model.KindQEMU, 102, 1024, "")
	s := f.run(context.Background(), Options{Trigger: "manual", OnlyVMID: 102})
	if hasEnv(s, "lists no backups") {
		t.Errorf("env = %q", s.EnvironmentErrors)
	}
}

func TestRunAllSourcesUnreadable(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.add("pbs", model.KindQEMU, 101, 1024, "")
	f.add("pbs-sync", model.KindQEMU, 102, 1024, "")
	f.api.Privs = map[string][]string{"/storage/pbs": {}, "/storage/pbs-sync": {}}
	s := f.run(context.Background(), Options{Trigger: "manual"})
	if !strings.Contains(s.RunError, "no source storage could be read") || s.Severity() != "error" {
		t.Errorf("run error %q, severity %s", s.RunError, s.Severity())
	}
}

func TestCheckSourcesOK(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.d.Cfg.Sources.Storages = []string{"pbs", "pbs-sync"}
	for _, st := range f.d.Cfg.Sources.Storages {
		f.api.Storage[st] = pve.StorageStatus{Active: true, Enabled: true, Content: "backup"}
	}
	if err := CheckSources(context.Background(), f.d); err != nil {
		t.Error(err)
	}
}

func TestCheckSourcesReportsEach(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.d.Cfg.Sources.Storages = []string{"pbs", "pbs-sync"}
	f.api.Storage["pbs"] = pve.StorageStatus{Active: true, Enabled: true, Content: "backup"}
	f.api.Storage["pbs-sync"] = pve.StorageStatus{Active: true, Enabled: true, Content: "images,rootdir"}
	f.api.Privs = map[string][]string{"/storage/pbs": {"Datastore.Audit"}}
	err := CheckSources(context.Background(), f.d)
	if err == nil || !strings.Contains(err.Error(), "lacks Datastore.AllocateSpace on /storage/pbs") ||
		!strings.Contains(err.Error(), `storage pbs-sync does not hold backup content (has "images,rootdir")`) {
		t.Errorf("err = %v", err)
	}
}

func TestCheckSourcesInactive(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.api.Storage["pbs"] = pve.StorageStatus{Active: false, Enabled: true, Content: "backup"}
	if err := CheckSources(context.Background(), f.d); err == nil || !strings.Contains(err.Error(), "storage pbs is not active and enabled") {
		t.Errorf("err = %v", err)
	}
}

// A storage the token cannot read is reported once, by its privileges, not
// again by PVE's 403 on its status (found on a test node).
func TestCheckSourcesSkipsStatusWhenUnreadable(t *testing.T) {
	f := newRunFixture(t, 990000, 990009)
	f.api.Storage["pbs"] = pve.StorageStatus{Active: true, Enabled: true, Content: "backup"}
	f.api.Privs = map[string][]string{"/storage/pbs": {}}
	err := CheckSources(context.Background(), f.d)
	if err == nil || !strings.Contains(err.Error(), "lacks Datastore.AllocateSpace, Datastore.Audit on /storage/pbs") {
		t.Fatalf("err = %v", err)
	}
	if slices.Contains(f.api.CallLog(), "StorageStatus pve1 pbs") {
		t.Errorf("read the status of a storage the token cannot read: %q", f.api.CallLog())
	}
}
